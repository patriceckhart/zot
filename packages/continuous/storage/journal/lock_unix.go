//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package journal

import (
	"errors"
	"os"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"golang.org/x/sys/unix"
)

const strictSupported = true

func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return storage.ErrLocked
	}
	return err
}
func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
