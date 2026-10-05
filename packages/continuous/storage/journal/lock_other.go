//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package journal

import (
	"fmt"
	"os"
)

const strictSupported = false

func lockFile(*os.File) error {
	return fmt.Errorf("continuous writer locking unsupported on this platform")
}
func unlockFile(*os.File) error    { return nil }
func durableSync(f *os.File) error { return f.Sync() }
func syncDirectory(string) error {
	return fmt.Errorf("continuous directory synchronization unsupported on this platform")
}
