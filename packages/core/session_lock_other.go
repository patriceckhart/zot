//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package core

import (
	"fmt"
	"os"
)

func lockSessionFile(*os.File) error {
	return fmt.Errorf("named session writer locking is unsupported on this platform")
}
