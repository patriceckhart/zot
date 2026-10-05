//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package sqlite

import (
	"fmt"
	"os"
)

func lockFile(*os.File) error {
	return fmt.Errorf("continuous writer locking unsupported on this platform")
}

func unlockFile(*os.File) error { return nil }
