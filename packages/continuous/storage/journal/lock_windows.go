package journal

import (
	"errors"
	"fmt"
	"os"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"golang.org/x/sys/windows"
)

// Directory creation durability is not yet validated on Windows. Never silently
// downgrade the default strict request. Explicit process mode remains available.
const strictSupported = false

func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return storage.ErrLocked
	}
	return err
}
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
func durableSync(f *os.File) error { return f.Sync() }
func syncDirectory(string) error {
	return fmt.Errorf("strict directory durability unavailable on Windows")
}
