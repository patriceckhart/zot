package journal

import (
	"os"

	"golang.org/x/sys/unix"
)

// F_FULLFSYNC requests flushing device caches, beyond ordinary fsync on macOS.
func durableSync(f *os.File) error {
	if err := f.Sync(); err != nil {
		return err
	}
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
