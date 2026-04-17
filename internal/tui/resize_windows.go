//go:build windows

package tui

import (
	"os"
	"time"
)

// Windows doesn't have SIGWINCH; resize events are signaled via input
// records. For v1 we just ignore resize and rely on the next render
// cycle to pick up the new size via term.GetSize.
func (p *ProcTerm) installResizeHandler() {}

func (p *ProcTerm) SetNonblock(enable bool) error { return nil }

// peekStdin on Windows: no non-blocking primitive in v1; fall through
// to a blocking read. This means bare Esc on Windows is only detected
// after the next keystroke. Good enough for v1.
func peekStdin(in *os.File, _ time.Duration) (byte, bool, error) {
	var b [1]byte
	_, err := in.Read(b[:])
	if err != nil {
		return 0, false, err
	}
	return b[0], true, nil
}
