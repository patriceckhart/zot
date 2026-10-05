//go:build windows

package continuous

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// Windows has no Unix domain sockets usable from every supported version, so
// the private local endpoint is a named pipe. The pipe is created with a
// security descriptor that grants access only to the creating user and
// rejects remote clients. Each accepted connection is one pipe instance with
// overlapped I/O so reads and writes honour Close and deadlines.

// LocalEndpointName returns the pipe name for a local endpoint chosen by
// path. The name embeds a hash of the absolute path so two stores on one
// machine do not collide, and keeps a readable suffix for diagnostics.
func LocalEndpointName(path string) string {
	if strings.HasPrefix(path, `\\.\pipe\`) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	h := fnv.New64a()
	h.Write([]byte(strings.ToLower(abs)))
	base := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, filepath.Base(abs))
	return fmt.Sprintf(`\\.\pipe\zot-continuous-%x-%s`, h.Sum64(), base)
}

// ListenLocal creates a named pipe endpoint accessible only to the current
// user. Dial with DialLocal(LocalEndpointName(path)).
func ListenLocal(path string) (net.Listener, error) {
	name := LocalEndpointName(path)
	sd, err := ownerOnlyDescriptor()
	if err != nil {
		return nil, err
	}
	l := &pipeListener{name: name, sd: sd, done: make(chan struct{})}
	// Create the first instance eagerly so a name collision or permission
	// problem surfaces at listen time, not at the first accept.
	first, err := l.newInstance(true)
	if err != nil {
		return nil, err
	}
	l.pending = first
	return l, nil
}

// DialLocal connects to a named pipe created by ListenLocal. A busy pipe (all
// instances in use) is retried until ctx ends.
func DialLocal(ctx context.Context, name string) (net.Conn, error) {
	if !strings.HasPrefix(name, `\\.\pipe\`) {
		name = LocalEndpointName(name)
	}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	for {
		h, err := windows.CreateFile(name16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			return newPipeConn(h, name, false), nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("connect to pipe %s: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ownerOnlyDescriptor grants full access to the current user and SYSTEM
// only, equivalent in spirit to a mode 0600 socket.
func ownerOnlyDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	sddl := fmt.Sprintf("O:%sG:%sD:P(A;;GA;;;%s)(A;;GA;;;SY)", user.User.Sid.String(), user.User.Sid.String(), user.User.Sid.String())
	return windows.SecurityDescriptorFromString(sddl)
}

type pipeListener struct {
	name    string
	sd      *windows.SECURITY_DESCRIPTOR
	mu      sync.Mutex
	pending *pipeConn
	closed  bool
	done    chan struct{}
}

func (l *pipeListener) newInstance(first bool) (*pipeConn, error) {
	name16, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafeSizeofSecurityAttributes), SecurityDescriptor: l.sd}
	h, err := windows.CreateNamedPipe(name16, flags, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, sa)
	if err != nil {
		return nil, fmt.Errorf("create pipe %s: %w", l.name, err)
	}
	return newPipeConn(h, l.name, true), nil
}

const unsafeSizeofSecurityAttributes = 24

func (l *pipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	c := l.pending
	l.pending = nil
	l.mu.Unlock()
	if c == nil {
		var err error
		if c, err = l.newInstance(false); err != nil {
			return nil, err
		}
	}
	// Prepare the next instance before connecting this one so a client
	// that dials immediately after finds an instance to connect to.
	next, err := l.newInstance(false)
	if err != nil {
		c.Close()
		return nil, err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		next.Close()
		c.Close()
		return nil, net.ErrClosed
	}
	l.pending = next
	l.mu.Unlock()
	if err := c.connect(l.done); err != nil {
		c.Close()
		if errors.Is(err, net.ErrClosed) {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	return c, nil
}

func (l *pipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.done)
	pending := l.pending
	l.pending = nil
	l.mu.Unlock()
	if pending != nil {
		pending.Close()
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.name) }

type pipeAddr string

func (pipeAddr) Network() string  { return "pipe" }
func (a pipeAddr) String() string { return string(a) }

// pipeConn is one pipe instance (server) or client handle with overlapped
// I/O. Each direction has its own event; Close cancels in-flight I/O.
type pipeConn struct {
	handle   windows.Handle
	name     string
	server   bool
	readEv   windows.Handle
	writeEv  windows.Handle
	closeMu  sync.Mutex
	closed   bool
	deadline struct {
		mu    sync.Mutex
		read  time.Time
		write time.Time
	}
}

func newPipeConn(h windows.Handle, name string, server bool) *pipeConn {
	rev, _ := windows.CreateEvent(nil, 1, 0, nil)
	wev, _ := windows.CreateEvent(nil, 1, 0, nil)
	return &pipeConn{handle: h, name: name, server: server, readEv: rev, writeEv: wev}
}

// connect waits for a client on a server instance. done ends the wait when
// the listener closes.
func (c *pipeConn) connect(done <-chan struct{}) error {
	ov := &windows.Overlapped{HEvent: c.readEv}
	err := windows.ConnectNamedPipe(c.handle, ov)
	switch {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
		return nil
	case errors.Is(err, windows.ERROR_IO_PENDING):
	default:
		return err
	}
	return c.wait(ov, c.readEv, done, time.Time{})
}

// wait blocks on an overlapped operation until it completes, done closes,
// or the deadline passes. Cancellation aborts the operation.
func (c *pipeConn) wait(ov *windows.Overlapped, ev windows.Handle, done <-chan struct{}, deadline time.Time) error {
	finished := make(chan error, 1)
	go func() {
		_, err := windows.WaitForSingleObject(ev, windows.INFINITE)
		finished <- err
	}()
	var timer <-chan time.Time
	if !deadline.IsZero() {
		timer = time.After(time.Until(deadline))
	}
	select {
	case err := <-finished:
		if err != nil {
			return err
		}
	case <-done:
		windows.CancelIoEx(c.handle, ov)
		<-finished
		return net.ErrClosed
	case <-timer:
		windows.CancelIoEx(c.handle, ov)
		<-finished
		return os.ErrDeadlineExceeded
	}
	var n uint32
	if err := windows.GetOverlappedResult(c.handle, ov, &n, false); err != nil {
		if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			return net.ErrClosed
		}
		return err
	}
	return nil
}

func (c *pipeConn) Read(p []byte) (int, error) {
	c.closeMu.Lock()
	closed := c.closed
	c.closeMu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	windows.ResetEvent(c.readEv)
	ov := &windows.Overlapped{HEvent: c.readEv}
	var n uint32
	err := windows.ReadFile(c.handle, p, &n, ov)
	if err != nil && errors.Is(err, windows.ERROR_IO_PENDING) {
		c.deadline.mu.Lock()
		deadline := c.deadline.read
		c.deadline.mu.Unlock()
		if err = c.wait(ov, c.readEv, nil, deadline); err == nil {
			err = windows.GetOverlappedResult(c.handle, ov, &n, false)
		}
	}
	switch {
	case err == nil:
		if n == 0 {
			return 0, io.EOF
		}
		return int(n), nil
	case errors.Is(err, windows.ERROR_BROKEN_PIPE), errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED), errors.Is(err, windows.ERROR_NO_DATA):
		return int(n), io.EOF
	case errors.Is(err, windows.ERROR_OPERATION_ABORTED):
		return int(n), net.ErrClosed
	}
	return int(n), err
}

func (c *pipeConn) Write(p []byte) (int, error) {
	c.closeMu.Lock()
	closed := c.closed
	c.closeMu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	total := 0
	for total < len(p) {
		windows.ResetEvent(c.writeEv)
		ov := &windows.Overlapped{HEvent: c.writeEv}
		var n uint32
		err := windows.WriteFile(c.handle, p[total:], &n, ov)
		if err != nil && errors.Is(err, windows.ERROR_IO_PENDING) {
			c.deadline.mu.Lock()
			deadline := c.deadline.write
			c.deadline.mu.Unlock()
			if err = c.wait(ov, c.writeEv, nil, deadline); err == nil {
				err = windows.GetOverlappedResult(c.handle, ov, &n, false)
			}
		}
		total += int(n)
		if err != nil {
			if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA) {
				return total, io.ErrClosedPipe
			}
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (c *pipeConn) Close() error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closed = true
	c.closeMu.Unlock()
	// Cancel pending I/O so blocked readers return, then release the handle.
	windows.CancelIoEx(c.handle, nil)
	if c.server {
		windows.FlushFileBuffers(c.handle)
		windows.DisconnectNamedPipe(c.handle)
	}
	err := windows.CloseHandle(c.handle)
	windows.CloseHandle(c.readEv)
	windows.CloseHandle(c.writeEv)
	return err
}

func (c *pipeConn) LocalAddr() net.Addr  { return pipeAddr(c.name) }
func (c *pipeConn) RemoteAddr() net.Addr { return pipeAddr(c.name) }

func (c *pipeConn) SetDeadline(t time.Time) error {
	c.deadline.mu.Lock()
	c.deadline.read, c.deadline.write = t, t
	c.deadline.mu.Unlock()
	return nil
}

func (c *pipeConn) SetReadDeadline(t time.Time) error {
	c.deadline.mu.Lock()
	c.deadline.read = t
	c.deadline.mu.Unlock()
	return nil
}

func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	c.deadline.mu.Lock()
	c.deadline.write = t
	c.deadline.mu.Unlock()
	return nil
}
