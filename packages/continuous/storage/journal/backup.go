package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

var backupMagic = [4]byte{'Z', 'C', 'B', '1'}

const backupHeaderSize = 24

// Backup creates a new, lossless archive of the current journal's committed
// prefix. The source is verified and exclusively locked through the copy, but
// its epoch and files are unchanged. Referenced external artifacts are not part
// of this prototype. Options describe destination durability, not the source.
func Backup(ctx context.Context, source, destination string, opts Options) (Verification, error) {
	return backup(ctx, source, destination, opts, nil)
}

// Backup archives the committed prefix of this open store without stopping
// its writer. The boundary is captured under the store lock; committed bytes
// are append-only, so the prefix stays stable while it is copied. The archive
// has the same format as the offline Backup and passes VerifyBackup.
func (s *Store) Backup(ctx context.Context, destination string, opts Options) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	mode, err := archiveDurability(opts)
	if err != nil {
		return Verification{}, err
	}
	if err := outsideStore(s.path, destination); err != nil {
		return Verification{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Verification{}, storage.ErrClosed
	}
	marker := boundaryMarker(s.revision, s.end)
	end := s.end
	segs, err := openSegments(s.path, true)
	s.mu.Unlock()
	if err != nil {
		return Verification{}, err
	}
	defer segs.close()
	if err := segs.validate(end); err != nil {
		return Verification{}, fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
	}
	data := io.NewSectionReader(segs, 0, end)
	report, err := scanFrames(ctx, data, end, marker, nil)
	if err != nil {
		return Verification{}, err
	}
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Verification{}, err
	}
	ok := false
	defer func() {
		if !ok {
			dst.Close()
			os.Remove(destination)
		}
	}()
	hash := sha256.New()
	out := io.MultiWriter(dst, hash)
	if err := writeAll(out, append(append([]byte(nil), backupMagic[:]...), marker...)); err != nil {
		return Verification{}, err
	}
	if err := copyContext(ctx, out, io.NewSectionReader(segs, 0, report.CommittedBytes), report.CommittedBytes); err != nil {
		return Verification{}, err
	}
	if err := writeAll(dst, hash.Sum(nil)); err != nil {
		return Verification{}, err
	}
	if err := syncArchive(ctx, dst, filepath.Dir(destination), mode, nil, "backup"); err != nil {
		return Verification{}, err
	}
	if err := dst.Close(); err != nil {
		return Verification{}, err
	}
	ok = true
	report.Scope = "journal-backup"
	report.UnacknowledgedTailBytes = 0
	return report, nil
}

func backup(ctx context.Context, source, destination string, opts Options, hook faultHook) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	mode, err := archiveDurability(opts)
	if err != nil {
		return Verification{}, err
	}
	if err := outsideStore(source, destination); err != nil {
		return Verification{}, err
	}
	return readJournal(ctx, source, func(data *journalData, marker []byte) (report Verification, retErr error) {
		report, err := scanJournal(ctx, data, marker, nil)
		if err != nil {
			return Verification{}, err
		}
		var dst *os.File
		err = hook.step("backup.file.create", func() error {
			var err error
			dst, err = os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			return err
		})
		closed := false
		if dst != nil {
			defer func() {
				if !closed {
					retErr = errors.Join(retErr, dst.Close())
				}
				if retErr != nil {
					retErr = errors.Join(retErr, os.Remove(destination))
				}
			}()
		}
		if err != nil {
			return Verification{}, err
		}
		hash := sha256.New()
		out := io.MultiWriter(dst, hash)
		header := append(append([]byte(nil), backupMagic[:]...), marker...)
		if err := hook.step("backup.header.write", func() error { return writeAll(out, header) }); err != nil {
			return Verification{}, err
		}
		if err := hook.step("backup.data.write", func() error {
			return copyContext(ctx, out, io.NewSectionReader(data, 0, report.CommittedBytes), report.CommittedBytes)
		}); err != nil {
			return Verification{}, err
		}
		if err := hook.step("backup.checksum.write", func() error { return writeAll(dst, hash.Sum(nil)) }); err != nil {
			return Verification{}, err
		}
		if err := syncArchive(ctx, dst, filepath.Dir(destination), mode, hook, "backup"); err != nil {
			return Verification{}, err
		}
		if err := hook.step("backup.file.close", func() error {
			closed = true
			return dst.Close()
		}); err != nil {
			return Verification{}, err
		}
		report.Scope = "journal-backup"
		report.UnacknowledgedTailBytes = 0
		return report, nil
	})
}

// VerifyBackup validates both the complete archive checksum and committed journal
// semantics without extracting it. Archives are private data, not authenticated
// or encrypted packages. External modification during a read is unsupported.
func VerifyBackup(ctx context.Context, path string) (report Verification, retErr error) {
	f, err := openArchive(ctx, path)
	if err != nil {
		return Verification{}, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	report, _, err = verifyArchive(ctx, f)
	return report, err
}

func openArchive(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exists, err := regularFileExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	return os.Open(path)
}

func verifyArchive(ctx context.Context, f *os.File) (Verification, []byte, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return Verification{}, nil, err
	}
	if info.Size() < backupHeaderSize+sha256.Size {
		return Verification{}, nil, storage.ErrCorrupt
	}
	var header [backupHeaderSize]byte
	if _, err := f.ReadAt(header[:], 0); err != nil || !bytes.Equal(header[:4], backupMagic[:]) {
		return Verification{}, nil, storage.ErrCorrupt
	}
	end := binary.BigEndian.Uint64(header[12:20])
	if end > uint64(^uint64(0)>>1)-(backupHeaderSize+sha256.Size) || int64(end)+backupHeaderSize+sha256.Size != info.Size() {
		return Verification{}, nil, storage.ErrCorrupt
	}
	hash := sha256.New()
	if err := copyContext(ctx, hash, io.NewSectionReader(f, 0, info.Size()-sha256.Size), info.Size()-sha256.Size); err != nil {
		return Verification{}, nil, err
	}
	var sum [sha256.Size]byte
	if _, err := f.ReadAt(sum[:], info.Size()-sha256.Size); err != nil || !bytes.Equal(sum[:], hash.Sum(nil)) {
		return Verification{}, nil, storage.ErrCorrupt
	}
	report, err := scanFrames(ctx, io.NewSectionReader(f, backupHeaderSize, int64(end)), int64(end), header[4:], nil)
	if err != nil {
		return Verification{}, nil, err
	}
	report.Scope = "journal-backup"
	return report, header[4:], nil
}

func archiveDurability(opts Options) (storage.Durability, error) {
	mode := opts.Durability
	if mode == "" {
		mode = storage.Strict
	}
	if mode != storage.Strict && mode != storage.Process {
		return "", fmt.Errorf("unsupported archive durability")
	}
	if mode == storage.Strict && !strictSupported {
		return "", fmt.Errorf("strict archive durability unavailable on this platform")
	}
	return mode, nil
}

func syncArchive(ctx context.Context, f *os.File, directory string, mode storage.Durability, hook faultHook, phase string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mode != storage.Strict {
		return nil
	}
	if err := hook.step(phase+".file.sync", func() error { return durableSync(f) }); err != nil {
		return err
	}
	if err := hook.step(phase+".directory.sync", func() error { return syncDirectory(directory) }); err != nil {
		return err
	}
	return hook.step(phase+".device.sync", func() error { return durableSync(f) })
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader, size int64) error {
	buf := make([]byte, 64<<10)
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(len(buf)), size)
		if _, err := io.ReadFull(src, buf[:n]); err != nil {
			return err
		}
		if err := writeAll(dst, buf[:n]); err != nil {
			return err
		}
		size -= n
	}
	return ctx.Err()
}

func outsideStore(source, destination string) error {
	root, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("backup requires an existing parent directory: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(filepath.Join(parent, filepath.Base(destination)))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
		return fmt.Errorf("backup destination must be outside the source store")
	}
	return nil
}
