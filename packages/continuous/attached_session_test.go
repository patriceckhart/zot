package continuous

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type observedSession struct {
	session  *AttachedSession
	view     *core.Agent
	messages chan provider.Message
	deltas   chan string
	resets   chan struct{}
	done     chan error
}

func startObservedSession(t *testing.T, ctx context.Context, client *Client, conversationID string) observedSession {
	t.Helper()
	view := core.NewAgent(nil, "scripted", "", core.NewRegistry())
	s, err := NewAttachedSession(ctx, &AttachedDriver{Client: client, ConversationID: conversationID}, view)
	if err != nil {
		t.Fatal(err)
	}
	o := observedSession{s, view, make(chan provider.Message, 16), make(chan string, 32), make(chan struct{}, 16), make(chan error, 1)}
	go func() {
		o.done <- s.Observe(ctx, func(ev core.AgentEvent) {
			switch e := ev.(type) {
			case core.EvAssistantMessage:
				o.messages <- e.Message
			case core.EvTextDelta:
				o.deltas <- e.Delta
			}
		}, func() { o.resets <- struct{}{} })
	}()
	select {
	case <-s.ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() {
		s.Close()
		select {
		case <-o.done:
		case <-time.After(5 * time.Second):
			t.Error("observer did not stop")
		}
	})
	return o
}

func observedAnswer(t *testing.T, ctx context.Context, o observedSession, want string) {
	t.Helper()
	select {
	case msg := <-o.messages:
		if core.MessageText(msg) != want {
			t.Fatalf("answer=%q, want %q", core.MessageText(msg), want)
		}
	case <-ctx.Done():
		t.Fatal("idle view did not receive the answer", ctx.Err())
	}
}

func TestAttachedSessionKeepsIdleViewsInSync(t *testing.T) {
	r, dial, hostCtx := attachHarness(t, &effectTool{name: "effect"}, []scriptStep{{text: "first answer"}, {text: "second answer"}, {text: "third answer"}})
	ctx, cancel := context.WithTimeout(hostCtx, 5*time.Second)
	defer cancel()
	leftClient, rightClient := dial(), dial()
	var conv Conversation
	if err := leftClient.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	left := startObservedSession(t, ctx, leftClient, conv.ID)
	right := startObservedSession(t, ctx, rightClient, conv.ID)
	// An external submitter updates both idle views without editor input.
	sub, err := r.Submit(ctx, conv.ID, "external", "first", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitSubmission(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	observedAnswer(t, ctx, left, "first answer")
	observedAnswer(t, ctx, right, "first answer")
	// A local submission uses the same watch and emits no second event copy.
	img := attachmentImage(t)
	if err := right.session.PromptWithImages(ctx, right.view, "second", []provider.ImageBlock{img}, func(core.AgentEvent) { t.Error("prompt emitted duplicate events") }); err != nil {
		t.Fatal(err)
	}
	observedAnswer(t, ctx, left, "second answer")
	observedAnswer(t, ctx, right, "second answer")
	if err := left.session.PromptWithImages(ctx, left.view, "third", nil, func(core.AgentEvent) { t.Error("prompt emitted duplicate events") }); err != nil {
		t.Fatal(err)
	}
	observedAnswer(t, ctx, left, "third answer")
	observedAnswer(t, ctx, right, "third answer")
	for _, o := range []observedSession{left, right} {
		msgs := o.view.Messages()
		if len(msgs) != 6 {
			t.Fatalf("mirrored messages=%d, want 6", len(msgs))
		}
		assertAttachedImage(t, msgs, img)
		if len(o.messages) != 0 {
			t.Fatal("assistant event duplicated")
		}
		if _, watches := o.session.driver.Client.pending(); watches != 1 {
			t.Fatalf("idle watches=%d, want 1", watches)
		}
	}
}

func TestAttachedSessionPromptCancellationKeepsObservationAndHostWork(t *testing.T) {
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	_, dial, hostCtx := attachHarness(t, tool, []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}, {text: "finished after detaching prompt"}, {text: "still observing"}})
	ctx, cancel := context.WithTimeout(hostCtx, 5*time.Second)
	defer cancel()
	client := dial()
	var conv Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	o := startObservedSession(t, ctx, client, conv.ID)
	promptCtx, detach := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- o.session.PromptWithImages(promptCtx, o.view, "first", nil, nil) }()
	select {
	case <-tool.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	detach()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDetached) {
			t.Fatalf("detach=%v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The tool-call assistant message was also observed.
	observedAnswer(t, ctx, o, "")
	close(tool.block)
	observedAnswer(t, ctx, o, "finished after detaching prompt")
	if err := o.session.PromptWithImages(ctx, o.view, "second", nil, nil); err != nil {
		t.Fatal(err)
	}
	observedAnswer(t, ctx, o, "still observing")
}

func TestAttachedSessionCloseUnblocksAdmissionRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	admitting := make(chan struct{})
	go func() {
		reader := bufio.NewReader(b)
		enc := json.NewEncoder(b)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var req hostRequest
			if json.Unmarshal(line, &req) != nil {
				return
			}
			if req.Method == "conversation.submit" {
				close(admitting)
				continue // Hold the admission response until the view closes.
			}
			if err := enc.Encode(hostResponse{ID: req.ID, Type: "response", Success: true, Data: ConversationSnapshot{}}); err != nil {
				return
			}
		}
	}()
	client, err := NewClient(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	o := startObservedSession(t, ctx, client, "w")
	done := make(chan error, 1)
	go func() { done <- o.session.PromptWithImages(ctx, o.view, "prompt", nil, nil) }()
	select {
	case <-admitting:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	o.session.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDetached) {
			t.Fatalf("close during admission=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("view shutdown blocked on admission RPC", ctx.Err())
	}
	select {
	case <-o.session.done:
	case <-ctx.Done():
		t.Fatal("observer shutdown blocked on submission mutex", ctx.Err())
	}
}

func TestAttachedSessionCloseDoesNotCancelHostWork(t *testing.T) {
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	r, dial, hostCtx := attachHarness(t, tool, []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}, {text: "host kept working"}})
	ctx, cancel := context.WithTimeout(hostCtx, 5*time.Second)
	defer cancel()
	client := dial()
	var conv Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	o := startObservedSession(t, ctx, client, conv.ID)
	done := make(chan error, 1)
	go func() { done <- o.session.PromptWithImages(ctx, o.view, "first", nil, nil) }()
	select {
	case <-tool.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	run, _, err := r.Run(ctx, conv.ID)
	if err != nil || len(run.Submissions) != 1 {
		t.Fatalf("run: %+v, %v", run, err)
	}
	o.session.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDetached) {
			t.Fatalf("close=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("view shutdown did not unblock its prompt", ctx.Err())
	}
	close(tool.block)
	sub, err := r.WaitSubmission(ctx, run.Submissions[0])
	if err != nil || sub.State != "answered" {
		t.Fatalf("closing the view cancelled host work: %+v, %v", sub, err)
	}
}

type gatedAttachedClient struct{ release <-chan struct{} }

func (gatedAttachedClient) Name() string { return "synthetic" }
func (c gatedAttachedClient) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	events := make(chan provider.Event, 2)
	go func() {
		defer close(events)
		events <- provider.EventTextDelta{Delta: "live remote text"}
		select {
		case <-c.release:
			events <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "live remote text"}}}}
		case <-ctx.Done():
		}
	}()
	return events, nil
}

func TestAttachedSessionStreamsOtherClientsOutputWhileIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := newTestRuntime(t)
	release := make(chan struct{})
	engine := echoEngine(gatedAttachedClient{release}, nil)
	host, err := NewHost(r, engine, HostOptions{Execution: ExecutionOptions{PartialFlushInterval: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan error, 1)
	go func() { hostDone <- host.Run(ctx) }()
	defer func() { cancel(); <-hostDone }()
	client := dialClient(t, &HostServer{Host: host, Engine: engine})
	var conv Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	o := startObservedSession(t, ctx, client, conv.ID)
	sub, err := r.Submit(ctx, conv.ID, "external", "remote", "go")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-o.deltas:
		if text != "live remote text" {
			t.Fatalf("partial=%q", text)
		}
	case <-ctx.Done():
		t.Fatal("idle view did not stream remote output", ctx.Err())
	}
	if len(o.messages) != 0 {
		t.Fatal("stream was not observed before the final answer")
	}
	close(release)
	observedAnswer(t, ctx, o, "live remote text")
	if _, err := r.WaitSubmission(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAttachedSessionResubscribesAfterWatchEnds(t *testing.T) {
	_, dial, hostCtx := attachHarness(t, &effectTool{name: "effect"}, []scriptStep{{text: "after resubscribe"}})
	ctx, cancel := context.WithTimeout(hostCtx, 5*time.Second)
	defer cancel()
	client := dial()
	var conv Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	o := startObservedSession(t, ctx, client, conv.ID)
	<-o.resets
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
	case <-o.resets:
	case <-ctx.Done():
		t.Fatal("watch was not resnapshotted", ctx.Err())
	}
	if err := o.session.PromptWithImages(ctx, o.view, "new prompt", nil, nil); err != nil {
		t.Fatal(err)
	}
	observedAnswer(t, ctx, o, "after resubscribe")
	if len(o.view.Messages()) != 2 {
		t.Fatal("resubscribe duplicated transcript entries")
	}
}
