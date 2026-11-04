//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package core

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockSessionFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrSessionInUse
	}
	return err
}
