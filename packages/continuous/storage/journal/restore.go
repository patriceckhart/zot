package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Restore verifies an archive before exclusively creating a new journal
// directory. It never merges with or replaces an existing store. The returned
// revision and epoch are the archived ones, the first writer open advances them.
// Abrupt termination can leave a partial directory, never open it as a writer
// without successful verification. Concurrent external mutation is unsupported.
func Restore(ctx context.Context, archive, destination string, opts Options) (Verification, error) {
	return restore(ctx, archive, destination, opts, nil)
}

func restore(ctx context.Context, archive, destination string, opts Options, hook faultHook) (report Verification, retErr error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	mode, err := archiveDurability(opts)
	if err != nil {
		return Verification{}, err
	}
	source, err := openArchive(ctx, archive)
	if err != nil {
		return Verification{}, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	report, marker, err := verifyArchive(ctx, source)
	if err != nil {
		return Verification{}, err
	}
	created := false
	complete := false
	var owned []string
	defer func() {
		if created && !complete {
			// Remove only files belonging to this operation, never recursively
			// remove unexpected files or an existing destination.
			for _, name := range owned {
				if err := os.Remove(filepath.Join(destination, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					retErr = errors.Join(retErr, err)
				}
			}
			retErr = errors.Join(retErr, os.Remove(destination))
		}
	}()
	if err := hook.step("restore.directory.create", func() error {
		if err := os.Mkdir(destination, 0o700); err != nil {
			return fmt.Errorf("create new restore directory: %w", err)
		}
		created = true
		return nil
	}); err != nil {
		return Verification{}, err
	}
	var lock *os.File
	if err := hook.step("restore.lock.create", func() error {
		var err error
		lock, err = os.OpenFile(filepath.Join(destination, "writer.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			owned = append(owned, "writer.lock")
		}
		return err
	}); err != nil {
		if lock != nil {
			lock.Close()
		}
		return Verification{}, err
	}
	locked := false
	defer func() {
		if locked {
			retErr = errors.Join(retErr, unlockFile(lock))
		}
		retErr = errors.Join(retErr, lock.Close())
	}()
	if err := lockFile(lock); err != nil {
		return Verification{}, err
	}
	locked = true
	if mode == storage.Strict {
		if err := hook.step("restore.lock.sync", func() error { return durableSync(lock) }); err != nil {
			return Verification{}, err
		}
	}
	if err := restoreFile(ctx, destination, "commits.log", io.NewSectionReader(source, backupHeaderSize, report.CommittedBytes), report.CommittedBytes, mode, hook, &owned); err != nil {
		return Verification{}, err
	}
	// Publish the boundary only after the complete journal data is written and
	// synchronized. A missing or truncated committed file fails verification.
	if err := restoreFile(ctx, destination, "boundary", bytes.NewReader(marker), int64(len(marker)), mode, hook, &owned); err != nil {
		return Verification{}, err
	}
	if mode == storage.Strict {
		if err := hook.step("restore.directory.sync", func() error { return syncDirectory(destination) }); err != nil {
			return Verification{}, err
		}
		if err := hook.step("restore.parent.sync", func() error { return syncDirectory(filepath.Dir(destination)) }); err != nil {
			return Verification{}, err
		}
		if err := hook.step("restore.device.sync", func() error { return durableSync(lock) }); err != nil {
			return Verification{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	complete = true
	report.Scope = "journal"
	return report, nil
}

func restoreFile(ctx context.Context, directory, name string, src io.Reader, size int64, mode storage.Durability, hook faultHook, owned *[]string) (retErr error) {
	var f *os.File
	closed := false
	defer func() {
		if f != nil && !closed {
			retErr = errors.Join(retErr, f.Close())
		}
	}()
	phase := "restore." + name
	if err := hook.step(phase+".create", func() error {
		var err error
		f, err = os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			*owned = append(*owned, name)
		}
		return err
	}); err != nil {
		return err
	}
	if err := hook.step(phase+".write", func() error { return copyContext(ctx, f, src, size) }); err != nil {
		return err
	}
	if mode == storage.Strict {
		if err := hook.step(phase+".sync", func() error { return durableSync(f) }); err != nil {
			return err
		}
	}
	return hook.step(phase+".close", func() error {
		closed = true
		return f.Close()
	})
}
