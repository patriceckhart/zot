//go:build !windows

package tui

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func (p *ProcTerm) installResizeHandler() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			for _, cb := range p.resizeCBs {
				cb()
			}
		}
	}()
}

func (p *ProcTerm) SetNonblock(enable bool) error {
	return syscall.SetNonblock(int(p.in.Fd()), enable)
}

// peekStdin polls the stdin fd for up to d; if a byte is ready, reads
// and returns it. Returns (0, false, nil) on timeout.
func peekStdin(in *os.File, d time.Duration) (byte, bool, error) {
	fd := int(in.Fd())
	var rset syscall.FdSet
	fdSet(&rset, fd)
	tv := syscall.Timeval{
		Sec:  int64(d / time.Second),
		Usec: int32((d % time.Second) / time.Microsecond),
	}
	err := syscall.Select(fd+1, &rset, nil, nil, &tv)
	if err != nil {
		if err == syscall.EINTR {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !fdIsSet(&rset, fd) {
		return 0, false, nil
	}
	var b [1]byte
	_, rerr := in.Read(b[:])
	if rerr != nil {
		return 0, false, rerr
	}
	return b[0], true, nil
}

// fdSet sets bit fd in the fd_set. Plain Go implementation so we don't
// need platform-specific helpers from golang.org/x/sys.
func fdSet(set *syscall.FdSet, fd int) {
	set.Bits[fd/64] |= 1 << (uint(fd) % 64)
}

func fdIsSet(set *syscall.FdSet, fd int) bool {
	return set.Bits[fd/64]&(1<<(uint(fd)%64)) != 0
}
