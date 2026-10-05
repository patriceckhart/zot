//go:build linux || freebsd || openbsd || netbsd || dragonfly

package journal

import "os"

func durableSync(f *os.File) error { return f.Sync() }
