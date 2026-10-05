package continuous

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// An attached driver submits through the host, projects committed entries as
// agent events, mirrors the transcript, and detaching leaves host work
// running. A second attach sees the completed answer.
func TestAttachedDriverFollowsHostAndDetaches(t *testing.T) {
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	client := &scriptedClient{steps: []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{"n":1}`)}},
		{text: "first answer"},
		{text: "second answer"},
	}}
	engine := EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(client, "scripted", "", core.NewRegistry(tool))
		a.MaxRetries = 0
		return a, nil
	})
	host, err := NewHost(r, engine, HostOptions{})
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
	c1 := dial()
	var conv Conversation
	if err := c1.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	driver := &AttachedDriver{Client: c1, ConversationID: conv.ID}
	view := core.NewAgent(nil, "scripted", "", core.NewRegistry())
	if _, err := driver.Load(ctx, view); err != nil || len(view.Messages()) != 0 {
		t.Fatalf("load empty: %v %d", err, len(view.Messages()))
	}
	// Detach while the tool is blocked: the driver returns ErrDetached, the
	// host keeps going.
	var events []string
	promptCtx, detach := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- driver.Prompt(promptCtx, view, "do it", func(ev core.AgentEvent) { events = append(events, ev.Type()) })
	}()
	<-tool.started
	detach()
	if err := <-done; !errors.Is(err, ErrDetached) {
		t.Fatalf("detach: %v", err)
	}
	close(tool.block)
	// Reattach with a fresh client and wait for the run to settle.
	c2 := dial()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var snap ConversationSnapshot
		if err := c2.CallInto(ctx, "conversation.snapshot", map[string]any{"id": conv.ID, "limit": 100}, &snap); err != nil {
			t.Fatal(err)
		}
		if snap.Run != nil && snap.Run.Phase == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not finish after detach")
		}
		time.Sleep(5 * time.Millisecond)
	}
	driver2 := &AttachedDriver{Client: c2, ConversationID: conv.ID}
	view2 := core.NewAgent(nil, "scripted", "", core.NewRegistry())
	if _, err := driver2.Load(ctx, view2); err != nil {
		t.Fatal(err)
	}
	msgs := view2.Messages()
	if len(msgs) != 4 || msgs[0].Role != provider.RoleUser || msgs[1].Role != provider.RoleAssistant || msgs[2].Role != provider.RoleTool || core.MessageText(msgs[3]) != "first answer" {
		t.Fatalf("reloaded transcript: %d %+v", len(msgs), msgs)
	}
	// A full prompt through the second attachment projects the committed
	// entries as events and ends when the run settles.
	events = nil
	err = driver2.Prompt(ctx, view2, "again", func(ev core.AgentEvent) { events = append(events, ev.Type()) })
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	joined := ""
	for _, e := range events {
		joined += e + " "
	}
	if joined != "user_message assistant_start assistant_message turn_end done " {
		t.Fatalf("events: %s", joined)
	}
	if n := len(view2.Messages()); n != 6 || core.MessageText(view2.Messages()[5]) != "second answer" {
		t.Fatalf("mirrored transcript: %d", n)
	}
	// Watch frames for a slow consumer: dropping the watch closes the channel
	// instead of buffering forever.
	watch, err := c2.Watch(ctx, conv.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	received := 0
	timeout := time.After(5 * time.Second)
	for received < 3 {
		select {
		case _, ok := <-watch:
			if !ok {
				t.Fatal("watch closed early")
			}
			received++
		case <-timeout:
			t.Fatal("history replay stalled")
		}
	}
}
