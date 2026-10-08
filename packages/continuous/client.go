package continuous

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Client speaks the host protocol over one connection. It is safe for
// concurrent calls; responses are matched by request ID and watch frames are
// delivered to their watch. Closing the client never cancels host work.
type Client struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
	next   int
	calls  map[string]chan hostResponse
	// watches receive commit frames by watch ID.
	watches map[string]chan json.RawMessage
	closed  chan struct{}
	err     error
}

// ClientError is a protocol-level failure with its code.
type ClientError struct {
	Method string
	Code   string
	Msg    string
}

func (e *ClientError) Error() string { return fmt.Sprintf("%s: %s (%s)", e.Method, e.Msg, e.Code) }

// Dial connects to a Unix socket path or a host:port address and performs the
// hello handshake when token is set. Plain TCP is for loopback hosts; remote
// hosts require DialTLS.
func Dial(ctx context.Context, address, token string) (*Client, error) {
	return DialTLS(ctx, address, token, nil)
}

// DialTLS is Dial with a TLS configuration for TCP addresses. A nil config
// dials plaintext, which the host only offers on loopback.
func DialTLS(ctx context.Context, address, token string, tlsConfig *tls.Config) (*Client, error) {
	var conn net.Conn
	var err error
	switch {
	case IsLocalAddress(address):
		conn, err = DialLocal(ctx, address)
	case tlsConfig != nil:
		d := &tls.Dialer{NetDialer: &net.Dialer{}, Config: tlsConfig}
		conn, err = d.DialContext(ctx, "tcp", address)
	default:
		var d net.Dialer
		conn, err = d.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to host: %w", err)
	}
	return NewClient(ctx, conn, token)
}

// IsLocalAddress reports whether address names a local endpoint (a Unix
// socket path or a Windows pipe name) rather than a TCP host:port.
func IsLocalAddress(address string) bool {
	if strings.HasPrefix(address, `\\.\pipe\`) {
		return true
	}
	return !strings.Contains(address, ":") || strings.HasPrefix(address, "/") || strings.HasPrefix(address, ".") || filepath.IsAbs(address)
}

// ClientTLSConfig trusts only the CA bundle in caFile and requires TLS 1.3.
func ClientTLSConfig(caFile string) (*tls.Config, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

// NewClient wraps an established connection.
func NewClient(ctx context.Context, conn net.Conn, token string) (*Client, error) {
	c := &Client{conn: conn, reader: bufio.NewReaderSize(conn, 1<<20), calls: map[string]chan hostResponse{}, watches: map[string]chan json.RawMessage{}, closed: make(chan struct{})}
	go c.readLoop()
	if token != "" {
		if _, err := c.Call(ctx, "hello", map[string]any{"token": token}); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// Close ends the connection. Pending calls fail; watches end.
func (c *Client) Close() error { return c.conn.Close() }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.closed }

func (c *Client) readLoop() {
	defer func() {
		c.mu.Lock()
		if c.err == nil {
			c.err = io.EOF
		}
		for id, ch := range c.calls {
			close(ch)
			delete(c.calls, id)
		}
		for id, ch := range c.watches {
			close(ch)
			delete(c.watches, id)
		}
		c.mu.Unlock()
		close(c.closed)
	}()
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
		var frame struct {
			hostResponse
			WatchID string          `json:"watch_id"`
			Commit  json.RawMessage `json:"commit"`
		}
		if json.Unmarshal(line, &frame) != nil {
			continue
		}
		c.mu.Lock()
		switch frame.Type {
		case "response":
			if ch, ok := c.calls[frame.ID]; ok {
				ch <- frame.hostResponse
				delete(c.calls, frame.ID)
			}
			// A watch can fail after its initial acknowledgement. Such a
			// response has no pending call, but still terminates the stream.
			if !frame.Success {
				if ch, ok := c.watches[frame.ID]; ok {
					close(ch)
					delete(c.watches, frame.ID)
				}
			}
		case "commit":
			if ch, ok := c.watches[frame.WatchID]; ok {
				select {
				case ch <- frame.Commit:
				default:
					// A slow consumer drops the watch rather than growing
					// client memory without bound; it must resnapshot.
					close(ch)
					delete(c.watches, frame.WatchID)
				}
			}
		case "watch_end":
			if ch, ok := c.watches[frame.WatchID]; ok {
				close(ch)
				delete(c.watches, frame.WatchID)
			}
		}
		c.mu.Unlock()
	}
}

func (c *Client) send(method string, params any) (string, chan hostResponse, error) {
	return c.sendWatch(method, params, nil)
}

// notify sends a request whose response nobody awaits, without registering
// it, so fire-and-forget requests leave nothing behind.
func (c *Client) notify(method string, params any) {
	id, _, err := c.send(method, params)
	if err == nil {
		c.forget(id)
	}
}

// sendWatch sends a request, registering frames as the commit channel for
// the request's ID before the request leaves the client. Commit frames can
// arrive immediately after the acknowledgement; registering afterwards would
// drop them.
func (c *Client) sendWatch(method string, params any, frames chan json.RawMessage) (string, chan hostResponse, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return "", nil, c.err
	}
	c.next++
	id := fmt.Sprintf("c%d", c.next)
	ch := make(chan hostResponse, 1)
	c.calls[id] = ch
	if frames != nil {
		c.watches[id] = frames
	}
	c.mu.Unlock()
	b, err := json.Marshal(hostRequest{ID: id, Method: method, Params: mustJSON(params)})
	if err != nil {
		c.mu.Lock()
		delete(c.calls, id)
		delete(c.watches, id)
		c.mu.Unlock()
		return "", nil, err
	}
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		c.mu.Lock()
		delete(c.calls, id)
		delete(c.watches, id)
		c.mu.Unlock()
		return "", nil, err
	}
	return id, ch, nil
}

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

// Call performs one request and returns its data.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, ch, err := c.send(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		if !resp.Success {
			return nil, &ClientError{Method: method, Code: resp.Code, Msg: resp.Error}
		}
		return mustJSON(resp.Data), nil
	case <-ctx.Done():
		// Forget the call so an abandoned request does not stay registered
		// until the connection closes. A late response is then ignored.
		c.forget(id)
		return nil, ctx.Err()
	}
}

// forget drops the local registrations of a request.
func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.calls, id)
	delete(c.watches, id)
	c.mu.Unlock()
}

// pending reports registered calls and watches, for tests.
func (c *Client) pending() (calls, watches int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls), len(c.watches)
}

// CallInto decodes the result into out.
func (c *Client) CallInto(ctx context.Context, method string, params any, out any) error {
	data, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Watch streams commits touching a conversation after a revision until ctx
// ends or the host ends the watch. A closed channel with ctx still live means
// the client fell behind or the host dropped the watch: take a new snapshot.
func (c *Client) Watch(ctx context.Context, conversationID string, after uint64) (<-chan storage.Commit, error) {
	frames := make(chan json.RawMessage, 256)
	id, ack, err := c.sendWatch("conversation.watch", map[string]any{"id": conversationID, "after": after}, frames)
	if err != nil {
		return nil, err
	}
	select {
	case resp, ok := <-ack:
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		if !resp.Success {
			c.forget(id)
			return nil, &ClientError{Method: "conversation.watch", Code: resp.Code, Msg: resp.Error}
		}
	case <-ctx.Done():
		// The host may still open the watch: forget it locally and ask the
		// host to end it, best effort.
		c.forget(id)
		c.notify("watch.cancel", map[string]any{"watch_id": id})
		return nil, ctx.Err()
	}
	out := make(chan storage.Commit, 64)
	go func() {
		defer close(out)
		defer func() {
			// A local buffer overflow removes the registration before this
			// goroutine exits, but the host watch still needs cancellation.
			// Cancelling an already-ended host watch is harmless.
			c.forget(id)
			c.notify("watch.cancel", map[string]any{"watch_id": id})
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case raw, ok := <-frames:
				if !ok {
					return
				}
				var cm storage.Commit
				if json.Unmarshal(raw, &cm) != nil {
					continue
				}
				select {
				case out <- cm:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// EntriesFromCommit extracts the entries a commit wrote for a conversation,
// in sequence order.
func EntriesFromCommit(cm storage.Commit, conversationID string) []Entry {
	prefix := "entry/" + conversationID + "/"
	var out []Entry
	for _, op := range cm.Operations {
		if op.Delete || !strings.HasPrefix(op.Key, prefix) {
			continue
		}
		var e Entry
		if json.Unmarshal(op.Value, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// RunFromCommit extracts the run record a commit wrote, when any.
//
// A commit carries the chain record of the conversation: a
// written chain projects to an active run with the chain's run ID; a
// deleted chain projects to a done run whose outcome comes from the
// settled submissions in the same commit. Turn and tool detail are not
// carried by a commit; fetch task.view or a snapshot for them.
func RunFromCommit(cm storage.Commit, conversationID string) (Run, bool) {
	var chainOp *storage.Operation
	for i, op := range cm.Operations {
		if op.Key == runKey(conversationID) && !op.Delete {
			var run Run
			if json.Unmarshal(op.Value, &run) == nil && run.Outcome != "migrated" {
				return run, true
			}
		}
		if op.Key == chainKey(conversationID) {
			chainOp = &cm.Operations[i]
		}
	}
	if chainOp == nil {
		return Run{}, false
	}
	if !chainOp.Delete {
		var chain Chain
		if json.Unmarshal(chainOp.Value, &chain) != nil {
			return Run{}, false
		}
		return Run{ID: chain.RunID, ConversationID: conversationID, Submissions: chain.Submissions, Phase: "request", Turn: 1, Attempt: 1, Notices: chain.Notices, Revision: cm.Revision}, true
	}
	// Settlement: the generation task's terminal record names the run.
	for _, op := range cm.Operations {
		if !strings.HasPrefix(op.Key, "task/") || op.Delete {
			continue
		}
		var t Task
		if json.Unmarshal(op.Value, &t) != nil || t.Kind != TaskKindGeneration || t.ConversationID != conversationID || t.State != "terminal" {
			continue
		}
		if run, ok := settledRun(t); ok {
			return run, true
		}
	}
	return Run{}, false
}

// ProjectEntryEvents turns committed entries into the agent events a UI
// renders for an in-process turn. Assistant entries produce the message and
// its tool calls; tool results produce EvToolResult. Streaming deltas are not
// reproduced: committed state has no partial output by design.
func ProjectEntryEvents(entries []Entry, sink func(core.AgentEvent)) {
	for _, e := range entries {
		switch e.Type {
		case entryAssistant:
			msg, err := core.DecodeMessage(e.Message)
			if err != nil {
				continue
			}
			sink(core.EvAssistantStart{})
			sink(core.EvAssistantMessage{Message: msg})
			for _, block := range msg.Content {
				if tc, ok := block.(provider.ToolCallBlock); ok {
					sink(core.EvToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Arguments})
				}
			}
		case entryToolResult:
			msg, err := core.DecodeMessage(e.Message)
			if err != nil {
				continue
			}
			for _, block := range msg.Content {
				if tr, ok := block.(provider.ToolResultBlock); ok {
					status := "completed"
					if tr.IsError {
						status = "failed"
					}
					sink(core.EvToolResult{ID: tr.CallID, Status: status, Executed: true, Result: core.ToolResult{Content: tr.Content, IsError: tr.IsError}})
				}
			}
		}
	}
}

// ErrDetached is returned by attached drivers when the client context ended
// while the host still owned the work.
var ErrDetached = errors.New("detached from host; the run continues on the host")
