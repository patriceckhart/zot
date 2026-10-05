package journal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Verification covers the journal format and transaction semantics, not runtime
// record schemas, transcript pairing, ownership, permissions, or hardware health.
// Tail bytes are outside committed authority and are reported without repair.
type Verification struct {
	Valid                   bool   `json:"valid"`
	Scope                   string `json:"scope"`
	Schema                  int    `json:"schema"`
	Revision                uint64 `json:"revision"`
	Epoch                   uint64 `json:"epoch"`
	CommittedBytes          int64  `json:"committed_bytes"`
	UnacknowledgedTailBytes int64  `json:"unacknowledged_tail_bytes"`
}

// Verify exclusively locks an existing local journal without acquiring a writer
// epoch, truncating tails, or changing files. It scans one bounded commit at a
// time. As with Open, external replacement or mutation is unsupported.
func Verify(ctx context.Context, path string) (Verification, error) {
	return readJournal(ctx, path, func(data *journalData, marker []byte) (Verification, error) {
		return scanJournal(ctx, data, marker, nil)
	})
}

// journalData is the read-only logical commit stream of a store: commits.log
// followed by rotated segments.
type journalData struct {
	*segmentSet
}

// Size is the number of bytes present, including an unacknowledged tail.
func (d *journalData) Size() int64 { return d.segmentSet.size() }

// readJournal keeps the existing writer lock through inspection or backup.
func readJournal(ctx context.Context, path string, inspect func(*journalData, []byte) (Verification, error)) (report Verification, retErr error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Verification{}, fmt.Errorf("inspect existing journal directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Verification{}, fmt.Errorf("store must be a real directory")
	}
	for _, name := range []string{"writer.lock", "commits.log", "boundary"} {
		exists, err := regularFileExists(filepath.Join(path, name))
		if err != nil {
			return Verification{}, err
		}
		if !exists {
			return Verification{}, fmt.Errorf("%w: missing journal file %s", storage.ErrCorrupt, name)
		}
	}
	// O_RDWR is needed by the Windows exclusive lock. No file is created or
	// written, and the same lock protects this scan from API writers.
	lock, err := os.OpenFile(filepath.Join(path, "writer.lock"), os.O_RDWR, 0)
	if err != nil {
		return Verification{}, err
	}
	if err := lockFile(lock); err != nil {
		return Verification{}, errors.Join(err, lock.Close())
	}
	defer func() { retErr = errors.Join(retErr, unlockFile(lock), lock.Close()) }()
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	segs, err := openSegments(path, true)
	if err != nil {
		return Verification{}, err
	}
	defer func() { retErr = errors.Join(retErr, segs.close()) }()
	marker, err := readBoundary(filepath.Join(path, "boundary"))
	if err != nil {
		return Verification{}, err
	}
	if len(marker) == 20 {
		end := int64(binary.BigEndian.Uint64(marker[8:16]))
		if err := segs.validate(end); err != nil {
			return Verification{}, fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
		}
	}
	return inspect(&journalData{segs}, marker)
}

func readBoundary(path string) (marker []byte, retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != 20 {
		return nil, storage.ErrCorrupt
	}
	marker = make([]byte, 20)
	if _, err := io.ReadFull(f, marker); err != nil {
		return nil, storage.ErrCorrupt
	}
	return marker, nil
}
