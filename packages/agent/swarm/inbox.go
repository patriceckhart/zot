package swarm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Inbox is the supervisor-side handle on a per-agent unix socket
// that the parent zot uses to send follow-up prompts (and other
// control lines) to a running swarm agent.
//
// Protocol on the wire: one UTF-8 message per line, newline-
// terminated. Multiline user input uses an additive user-json frame with a
// JSON string payload. Listener decodes it before dispatch. Message kinds:
//
//	user <text>...  - append <text> as the next user turn
//	user-json <JSON string>  - multiline user input, decoded as one user message
//	user-prompt <JSON Prompt> - user input with images, validated by the runner
//	cancel          — cancel the agent's in-flight turn
//	shutdown        — graceful exit; child will write a final
//	                  EvTurnEnd-equivalent JSON event and quit
//
// The child listens on the same socket via Listener (below) and
// translates the messages into core.Agent.Prompt calls. The
// parent never reads from the socket — it's a one-way channel
// driven by the supervisor.
//
// Why a socket and not a FIFO: FIFOs are POSIX-only and have
// awkward blocking-open semantics. A unix socket gives us:
//
//   - the ability to fail fast if no child is listening yet,
//     so SendInput returns a useful error to the TUI;
//   - clean shutdown semantics when the child closes the
//     listener;
//   - works under tests with a temp dir; and
//   - portable enough; Windows support is a follow-up that can
//     swap in a named pipe behind this same interface.
type Inbox struct {
	path string

	mu   sync.Mutex
	conn net.Conn // lazily dialed; one persistent connection per agent
}

// NewInbox returns a handle that will dial the socket at path on
// the first SendInput call. The socket file is expected to be
// created by the child (Listener.Open below). This split lets the
// parent build the Inbox before the child has booted; sends that
// happen before the child is ready get a clear "not yet" error
// the caller can retry or surface.
func NewInbox(path string) *Inbox { return &Inbox{path: path} }

// Path returns the absolute socket path. Used by the runner to
// pass the path to the child as a flag.
func (b *Inbox) Path() string { return b.path }

// SendInput writes msg to the inbox as one line. msg must already
// include the leading kind ("user ", "cancel", "shutdown") — the
// caller chooses the shape so we don't proliferate methods. The
// socket is reused across calls; transient dial failures yield
// ErrNotReady so the TUI can retry or report "agent not listening
// yet" rather than crashing.
func (b *Inbox) SendInput(msg string) error {
	if strings.ContainsAny(msg, "\r\n") {
		if !strings.HasPrefix(msg, "user ") {
			return fmt.Errorf("swarm: control messages must be a single line")
		}
		payload, err := json.Marshal(strings.TrimPrefix(msg, "user "))
		if err != nil {
			return fmt.Errorf("swarm: encode user input: %w", err)
		}
		msg = "user-json " + string(payload)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		c, err := dialUnix(b.path, 200*time.Millisecond)
		if err != nil {
			return err
		}
		b.conn = c
	}
	if _, err := io.WriteString(b.conn, msg+"\n"); err != nil {
		// Drop the connection so the next call redials. The
		// previous error is more informative than the redial's
		// would be, so surface this one.
		_ = b.conn.Close()
		b.conn = nil
		return err
	}
	return nil
}

// Close drops any persistent connection. Safe to call repeatedly.
// Does not unlink the socket file — that's the child's job
// (Listener.Close).
func (b *Inbox) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		err := b.conn.Close()
		b.conn = nil
		return err
	}
	return nil
}

// ErrNotReady is returned by SendInput when the child hasn't yet
// opened its listener. Callers can retry after a short backoff;
// the TUI surfaces it as "agent <id> not listening yet".
var ErrNotReady = errors.New("swarm: agent inbox not ready")

// dialUnix wraps net.DialTimeout with a small retry window so the
// happy path "child just started, supervisor sends right away"
// works without forcing the caller to poll.
//
// Errors collapse onto ErrNotReady whenever the failure means "no
// one is listening": either the socket file doesn't exist (child
// hasn't booted) or it does exist but the dial was refused (the
// previous child exited and left a stale inode). Both are
// recoverable by Swarm.Resume, and both should surface as the same
// user-facing hint rather than as a path-leaking error string.
func dialUnix(path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		c, err := net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return nil, ErrNotReady
	}
	if isNoListenerErr(lastErr) {
		return nil, ErrNotReady
	}
	return nil, fmt.Errorf("swarm: dial %s: %w", path, lastErr)
}

// isNoListenerErr reports whether err means "the socket file exists
// but no process is listening on it" — i.e. ECONNREFUSED on unix
// domain sockets. We pattern-match on errno via errors.Is rather
// than the error string so platform variants (linux vs darwin) all
// fold to the same case.
func isNoListenerErr(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// Listener is the child-side end of the inbox. The swarm-agent
// daemon mode constructs one with Listen, then iterates Lines
// to receive supervisor messages. Designed so a single goroutine
// in the child can drive prompting without juggling raw net code.
type Listener struct {
	path string
	ln   net.Listener
	// active is the most recent accepted connection. The protocol
	// only expects one supervisor (the parent zot), so newer
	// connections preempt older ones — the previous parent
	// presumably crashed.
	mu     sync.Mutex
	active net.Conn
	out    chan string
	done   chan struct{}
	wg     sync.WaitGroup
}

// Listen creates the socket at path and starts accepting. The
// caller must call Close to remove the socket file on shutdown.
// Returns the listener even if a stale socket exists from a
// previous run: we unlink first, mirroring how most unix daemons
// behave.
func Listen(path string) (*Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("inbox dir: %w", err)
	}
	// Best-effort cleanup of a stale socket. If the parent
	// process is still alive and using it, Listen will fail
	// below and the caller surfaces that as a real conflict.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("inbox listen: %w", err)
	}
	l := &Listener{
		path: path,
		ln:   ln,
		out:  make(chan string, 16),
		done: make(chan struct{}),
	}
	go l.acceptLoop()
	return l, nil
}

// Lines returns a channel of newline-stripped messages received
// from the supervisor. The channel closes when Close is called.
func (l *Listener) Lines() <-chan string { return l.out }

// Close stops accepting, drops the active connection, removes
// the socket file, and closes Lines. Idempotent.
func (l *Listener) Close() error {
	select {
	case <-l.done:
		return nil
	default:
		close(l.done)
	}
	l.mu.Lock()
	if l.active != nil {
		_ = l.active.Close()
	}
	l.mu.Unlock()
	_ = l.ln.Close()
	_ = os.Remove(l.path)
	return nil
}

func (l *Listener) acceptLoop() {
	defer func() {
		l.wg.Wait()
		close(l.out)
	}()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
				// Accept can fail transiently (signal); back off briefly.
				time.Sleep(20 * time.Millisecond)
				continue
			}
		}
		l.mu.Lock()
		if l.active != nil {
			_ = l.active.Close()
		}
		l.active = c
		l.mu.Unlock()
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.readLoop(c)
		}()
	}
}

func (l *Listener) readLoop(c net.Conn) {
	br := bufio.NewReader(c)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			msg := trimRightNL(line)
			if strings.HasPrefix(msg, "user-json ") {
				var text *string
				if err := json.Unmarshal([]byte(strings.TrimPrefix(msg, "user-json ")), &text); err != nil || text == nil {
					// Report invalid framing without exposing its payload.
					msg = "invalid supervisor message"
				} else {
					msg = "user " + *text
				}
			}
			select {
			case l.out <- msg:
			case <-l.done:
				_ = c.Close()
				return
			}
		}
		if err != nil {
			_ = c.Close()
			l.mu.Lock()
			if l.active == c {
				l.active = nil
			}
			l.mu.Unlock()
			return
		}
	}
}

func trimRightNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
