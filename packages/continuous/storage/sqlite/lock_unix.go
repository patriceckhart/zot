//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package sqlite

import (
	"errors"
	"os"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"golang.org/x/sys/unix"
)

func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return storage.ErrLocked
	}
	return err
}

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
