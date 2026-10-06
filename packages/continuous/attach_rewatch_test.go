package continuous

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func attachHarness(t *testing.T, tool *effectTool, steps []scriptStep, opts ...HostOptions) (*Runtime, func() *Client, context.Context) {
	t.Helper()
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{steps: steps}
	engine := EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(client, "scripted", "", core.NewRegistry(tool))
		a.MaxRetries = 0
		return a, nil
	})
	var o HostOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	host, err := NewHost(r, engine, o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go host.Run(ctx)
	t.Cleanup(func() { cancel(); r.Close() })
	server := &HostServer{Host: host, Engine: engine}
	dial := func() *Client {
		a, b := net.Pipe()
		go server.ServeConn(context.Background(), b)
		c, err := NewClient(context.Background(), a, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	return r, dial, ctx
}

// dropWatches simulates the client dropping every watch, as it does for a
// slow consumer.
func dropWatches(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for id, ch := range c.watches {
		close(ch)
		delete(c.watches, id)
		n++
	}
	return n
}

// A watch dropped while the submission still runs is reopened after the
// last applied revision: the prompt follows the run to its end, each entry
// is rendered once, and the prompt is never submitted again.
func TestAttachedDriverResumesDroppedWatch(t *testing.T) {
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	r, dial, ctx := attachHarness(t, tool, []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{"n":1}`)}},
		{text: "final answer"},
	})
	c := dial()
	var conv Conversation
	if err := c.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	driver := &AttachedDriver{Client: c, ConversationID: conv.ID}
	view := core.NewAgent(nil, "scripted", "", core.NewRegistry())
	events := make(chan string, 64)
	done := make(chan error, 1)
	go func() {
		done <- driver.Prompt(ctx, view, "do it", func(ev core.AgentEvent) { events <- ev.Type() })
	}()
	<-tool.started
	// Wait until the tool call is rendered, then drop the watch.
	waitEvent(t, events, "tool_call")
	if dropWatches(c) != 1 {
		t.Fatal("expected one live watch")
	}
	close(tool.block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not finish after the watch dropped")
	}
	close(events)
	var rest []string
	for ev := range events {
		rest = append(rest, ev)
	}
	got := strings.Join(rest, " ")
	if strings.Count(got, "tool_result") != 1 || strings.Count(got, "assistant_message") != 1 || !strings.HasSuffix(got, "turn_end done") {
		t.Fatalf("events after drop: %s", got)
	}
	msgs := view.Messages()
	if len(msgs) != 4 || core.MessageText(msgs[3]) != "final answer" {
		t.Fatalf("transcript: %d %+v", len(msgs), msgs)
	}
	snap, err := r.ConversationSnapshot(ctx, conv.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, e := range snap.Entries {
		if e.Type == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("prompt submitted %d times", users)
	}
}

func waitEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev == want {
				return
			}
		case <-timeout:
			t.Fatalf("no %s event", want)
		}
	}
}

// Cancelled calls and watches do not stay registered on a long-lived client.
func TestClientForgetsCancelledRequests(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	// A peer that reads requests and never answers.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	c, err := NewClient(context.Background(), a, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.Call(ctx, "runtime.status", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("call: %v", err)
		}
		if _, err := c.Watch(ctx, "conv", 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("watch: %v", err)
		}
	}
	// The best-effort watch.cancel requests are not awaited, so they leave
	// nothing behind either.
	if calls, watches := c.pending(); calls != 0 || watches != 0 {
		t.Fatalf("pending calls=%d watches=%d", calls, watches)
	}
}
