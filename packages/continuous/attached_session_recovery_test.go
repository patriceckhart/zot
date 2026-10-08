package continuous

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Hold the replacement watch request before it reaches the host. Other RPCs
// can still complete, making concurrent prompt admission during refresh visible.
type rewatchGateConn struct {
	net.Conn
	ctx     context.Context
	mu      sync.Mutex
	watches int
	started chan struct{}
	release chan struct{}
}

func (c *rewatchGateConn) Write(b []byte) (int, error) {
	var req hostRequest
	if json.Unmarshal(b, &req) == nil && req.Method == "conversation.watch" {
		c.mu.Lock()
		c.watches++
		gate := c.watches == 2
		c.mu.Unlock()
		if gate {
			close(c.started)
			select {
			case <-c.release:
			case <-c.ctx.Done():
				return 0, c.ctx.Err()
			}
		}
	}
	return c.Conn.Write(b)
}

func TestAttachedDriverReplaysSettlementAfterDroppedWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := newTestRuntime(t)
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	releaseTool := sync.OnceFunc(func() { close(tool.block) })
	engine := echoEngine(&scriptedClient{steps: []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "after resubscribe"},
	}}, core.NewRegistry(tool))
	host, err := NewHost(r, engine, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan error, 1)
	go func() { hostDone <- host.Run(ctx) }()
	defer func() { cancel(); <-hostDone }()
	a, b := net.Pipe()
	gate := &rewatchGateConn{Conn: a, ctx: ctx, started: make(chan struct{}), release: make(chan struct{})}
	defer a.Close()
	defer b.Close()
	server := &HostServer{Host: host, Engine: engine}
	go server.ServeConn(ctx, b)
	client, err := NewClient(ctx, gate, "")
	if err != nil {
		t.Fatal(err)
	}
	var conv Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	view := core.NewAgent(nil, "scripted", "", core.NewRegistry())
	driver := &AttachedDriver{Client: client, ConversationID: conv.ID}
	defer releaseTool()
	events := make(chan string, 64)
	done := make(chan error, 1)
	go func() { done <- driver.Prompt(ctx, view, "prompt", func(ev core.AgentEvent) { events <- ev.Type() }) }()
	select {
	case <-tool.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitEvent(t, events, "tool_call")
	run, _, err := r.Run(ctx, conv.ID)
	if err != nil || len(run.Submissions) != 1 {
		t.Fatalf("active run: %+v, %v", run, err)
	}
	if dropWatches(client) != 1 {
		t.Fatal("expected one live watch")
	}
	select {
	case <-gate.started:
	case <-ctx.Done():
		t.Fatal("replacement watch was not requested", ctx.Err())
	}
	// Settle before the replacement watch is sent. The driver must replay
	// the missed commits even when submission.get already reports answered.
	releaseTool()
	if _, err := r.WaitSubmission(ctx, run.Submissions[0]); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(events)
	var rest []string
	for ev := range events {
		rest = append(rest, ev)
	}
	got := strings.Join(rest, " ")
	if strings.Count(got, "tool_result") != 1 || strings.Count(got, "assistant_message") != 1 || !strings.HasSuffix(got, "turn_end done") {
		t.Fatalf("missed settlement events: %s", got)
	}
	if msgs := view.Messages(); len(msgs) != 4 || core.MessageText(msgs[3]) != "after resubscribe" {
		t.Fatalf("transcript: %+v", msgs)
	}
}

func TestAttachedSessionSerializesAdmissionWithResubscription(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := newTestRuntime(t)
	engine := echoEngine(&scriptedClient{steps: []scriptStep{{text: "after resubscribe"}}}, core.NewRegistry())
	host, err := NewHost(r, engine, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan error, 1)
	go func() { hostDone <- host.Run(ctx) }()
	defer func() { cancel(); <-hostDone }()
	a, b := net.Pipe()
	gate := &rewatchGateConn{Conn: a, ctx: ctx, started: make(chan struct{}), release: make(chan struct{})}
	defer a.Close()
	defer b.Close()
	server := &HostServer{Host: host, Engine: engine}
	go server.ServeConn(ctx, b)
	client, err := NewClient(ctx, gate, "")
	if err != nil {
		t.Fatal(err)
	}
	var conv Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	o := startObservedSession(t, ctx, client, conv.ID)
	client.mu.Lock()
	var id string
	for key := range client.watches {
		id = key
	}
	client.mu.Unlock()
	if _, err := client.Call(ctx, "watch.cancel", map[string]any{"watch_id": id}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-ctx.Done():
		t.Fatal("replacement watch was not requested", ctx.Err())
	}
	// A prompt must not be admitted between loading the replacement snapshot
	// and restoring its watch. Otherwise settlement can be resnapshotted as
	// historical context instead of being delivered as this prompt's events.
	if o.session.mu.TryLock() {
		o.session.mu.Unlock()
		t.Fatal("resubscription did not serialize prompt admission")
	}
	done := make(chan error, 1)
	go func() { done <- o.session.PromptWithImages(ctx, o.view, "new prompt", nil, nil) }()
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	observedAnswer(t, ctx, o, "after resubscribe")
	if len(o.view.Messages()) != 2 {
		t.Fatal("resubscribe duplicated transcript entries")
	}
}
