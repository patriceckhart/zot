package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// configure replaces the root conversation's configuration.
func (h *nativeHarness) configure(t *testing.T, id string, mutate func(*AgentConfig)) Conversation {
	t.Helper()
	cur, err := h.r.Conversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	cfg := cur.Config
	mutate(&cfg)
	c, err := h.r.Configure(context.Background(), id, cur.Revision, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Abort withdraws inputs queued behind the aborted chain; queued writes
// are still written, in order. Nothing starts.
func TestNativeAbortWithdrawsQueuedInputs(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "never"},
	}, tool)
	defer h.r.Close()
	c := h.nativeRoot(t)
	first, _ := h.r.Submit(ctx, c.ID, "a", "", "go")
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.sched.Run(runCtx) }()
	<-tool.started
	queued, _ := h.r.Submit(ctx, c.ID, "a", "", "later")
	note, err := h.r.SubmitWith(ctx, c.ID, "a", "", "a note", SubmitOptions{Kind: SubmissionWrite})
	if err != nil || note.State != "queued" || note.Kind != SubmissionWrite {
		t.Fatalf("write behind an active chain: %+v %v", note, err)
	}
	if _, err := h.r.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	close(tool.block)
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, want := range []struct{ id, state string }{{first.ID, "aborted"}, {queued.ID, "withdrawn"}, {note.ID, submissionWritten}} {
		if s, err := h.r.WaitSubmission(waitCtx, want.id); err != nil || s.State != want.state {
			t.Fatalf("submission %s: %+v %v", want.state, s, err)
		}
	}
	stop()
	<-done
	if _, active, _ := h.r.Chain(ctx, c.ID); active {
		t.Fatal("abort started the next chain")
	}
	if h.client.requestCount() != 1 {
		t.Fatalf("requests: %d", h.client.requestCount())
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant user user tool_result steer" {
		t.Fatalf("entries: %s", got)
	}
	checkValid(t, h.r)
}

// After a failed chain, queued inputs stay queued and start nothing until
// the next prompt submission places them, oldest first.
func TestNativeFailureHoldsQueueUntilNextSubmission(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{err: errors.New("bad request")},
		{text: "answer to all"},
	})
	defer h.r.Close()
	c := h.nativeRoot(t)
	first, _ := h.r.Submit(ctx, c.ID, "a", "", "first")
	held, _ := h.r.Submit(ctx, c.ID, "a", "", "held")
	h.run(t)
	if s, _ := h.r.Submission(ctx, first.ID); s.State != "failed" {
		t.Fatalf("first: %+v", s)
	}
	if s, _ := h.r.Submission(ctx, held.ID); s.State != "queued" {
		t.Fatalf("held input started after a failure: %+v", s)
	}
	if cur, _ := h.r.Conversation(ctx, c.ID); !cur.QueueHeld {
		t.Fatal("queue not held")
	}
	if n, _ := h.r.AdmitQueued(ctx); n != 0 {
		t.Fatalf("admission started a held queue: %d", n)
	}
	checkValid(t, h.r)
	next, err := h.r.Submit(ctx, c.ID, "a", "", "next")
	if err != nil || next.State != "running" {
		t.Fatalf("next: %+v %v", next, err)
	}
	h.run(t)
	for _, id := range []string{held.ID, next.ID} {
		if s, _ := h.r.Submission(ctx, id); s.State != "answered" {
			t.Fatalf("submission: %+v", s)
		}
	}
	// The held input is placed before the new one.
	req := h.client.requests[1].Messages
	if n := len(req); n < 2 || core.MessageText(req[n-2]) != "held" || core.MessageText(req[n-1]) != "next" {
		t.Fatalf("placement order: %+v", req)
	}
	checkValid(t, h.r)
}

// A write submission appends its entry without a model request. While a
// chain runs it waits for the next boundary and is placed in order.
func TestNativeWriteSubmissions(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "answer"}})
	defer h.r.Close()
	c := h.nativeRoot(t)
	w, err := h.r.SubmitWith(ctx, c.ID, "a", "w-1", "background fact", SubmitOptions{Kind: SubmissionWrite})
	if err != nil || w.State != submissionWritten {
		t.Fatalf("idle write: %+v %v", w, err)
	}
	if _, active, _ := h.r.Chain(ctx, c.ID); active {
		t.Fatal("a write started a model request")
	}
	if again, err := h.r.SubmitWith(ctx, c.ID, "a", "w-1", "background fact", SubmitOptions{Kind: SubmissionWrite}); err != nil || again.ID != w.ID {
		t.Fatalf("write retry: %+v %v", again, err)
	}
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "w-1", "background fact", SubmitOptions{}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("kind change accepted: %v", err)
	}
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "", "x", SubmitOptions{Kind: SubmissionWrite, Policy: PolicySteer}); err == nil {
		t.Fatal("write with a policy accepted")
	}
	checkValid(t, h.r)
	h.r.Submit(ctx, c.ID, "a", "", "question")
	h.run(t)
	if h.client.requestCount() != 1 {
		t.Fatalf("requests: %d", h.client.requestCount())
	}
	if msgs := h.client.requests[0].Messages; len(msgs) != 2 || core.MessageText(msgs[0]) != "background fact" {
		t.Fatalf("write not in context: %+v", msgs)
	}
	checkValid(t, h.r)
}

// "one" placement answers one queued prompt per chain and steers one input
// per boundary; the default places all at once.
func TestNativePlacementModes(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "a1"}, {text: "a2"}, {text: "a3"}})
	defer h.r.Close()
	c := h.nativeRoot(t)
	if _, err := h.r.Configure(ctx, c.ID, c.Revision, AgentConfig{FollowUpMode: "some"}); err == nil {
		t.Fatal("unknown placement mode accepted")
	}
	h.configure(t, c.ID, func(cfg *AgentConfig) { cfg.FollowUpMode = PlacementOne })
	h.r.Submit(ctx, c.ID, "a", "", "one")
	h.r.Submit(ctx, c.ID, "a", "", "two")
	h.r.Submit(ctx, c.ID, "a", "", "three")
	h.run(t)
	if h.client.requestCount() != 3 {
		t.Fatalf("one-at-a-time requests: %d", h.client.requestCount())
	}
	for i, want := range []string{"one", "two", "three"} {
		msgs := h.client.requests[i].Messages
		if core.MessageText(msgs[len(msgs)-1]) != want {
			t.Fatalf("chain %d answered %q", i, core.MessageText(msgs[len(msgs)-1]))
		}
	}
	checkValid(t, h.r)
}

func TestNativeSteerPlacementOne(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "after first steer"},
		{text: "after second steer"},
	}, tool)
	defer h.r.Close()
	c := h.nativeRoot(t)
	h.configure(t, c.ID, func(cfg *AgentConfig) { cfg.SteerMode = PlacementOne })
	h.r.Submit(ctx, c.ID, "a", "", "go")
	go func() {
		<-tool.started
		h.r.SubmitWith(ctx, c.ID, "a", "", "s1", SubmitOptions{Policy: PolicySteer})
		h.r.SubmitWith(ctx, c.ID, "a", "", "s2", SubmitOptions{Policy: PolicySteer})
		close(tool.block)
	}()
	h.run(t)
	// s1 joins the first chain at its boundary; s2 waits for the next
	// chain, which answers it.
	if h.client.requestCount() != 3 {
		t.Fatalf("requests: %d", h.client.requestCount())
	}
	second := h.client.requests[1].Messages
	if core.MessageText(second[len(second)-1]) != "s1" {
		t.Fatalf("first boundary placed: %q", core.MessageText(second[len(second)-1]))
	}
	third := h.client.requests[2].Messages
	if core.MessageText(third[len(third)-1]) != "s2" {
		t.Fatalf("second chain answered: %q", core.MessageText(third[len(third)-1]))
	}
	checkValid(t, h.r)
}

// terminateTool asks to end the run with its result.
type terminateTool struct {
	name  string
	calls atomic.Int32
}

func (t *terminateTool) Name() string                        { return t.name }
func (t *terminateTool) Description() string                 { return "ends the run" }
func (t *terminateTool) Schema() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (t *terminateTool) ReplayPolicy() core.ToolReplayPolicy { return core.ReplayNever }
func (t *terminateTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.calls.Add(1)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "done"}}, Terminate: true}, nil
}

// A round whose every result asks for termination ends the run without
// another model request. A mixed round continues.
func TestNativeToolTermination(t *testing.T) {
	ctx := context.Background()
	stopper := &terminateTool{name: "finish"}
	other := &effectTool{name: "effect"}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "finish", `{}`), call("c2", "effect", `{}`)}},
		{calls: []provider.ToolCallBlock{call("c3", "finish", `{}`)}},
		{text: "never"},
	}, stopper, other)
	defer h.r.Close()
	c := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, c.ID, "a", "", "go")
	h.run(t)
	if h.client.requestCount() != 2 {
		t.Fatalf("requests: %d", h.client.requestCount())
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("submission: %+v", s)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result tool_result assistant tool_result" {
		t.Fatalf("entries: %s", got)
	}
	checkValid(t, h.r)
}

// Parallel tool execution: every call of a round starts before any
// finishes, each keeps its own task, and the results stay paired in call
// order. A crash with both calls running recovers each by its own policy.
func TestNativeParallelTools(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	var mu sync.Mutex
	running := 0
	peak := 0
	tool := &funcTool{name: "slow", run: func(ctx context.Context) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
		}
		mu.Lock()
		running--
		mu.Unlock()
	}}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("p1", "slow", `{"n":1}`), call("p2", "slow", `{"n":2}`), call("p3", "slow", `{"n":3}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.nativeRoot(t)
	h.configure(t, c.ID, func(cfg *AgentConfig) { cfg.ParallelTools = true })
	h.r.Submit(ctx, c.ID, "a", "", "go")
	runCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- h.sched.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := running
		mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parallel calls running: %d", n)
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak != 3 {
		t.Fatalf("peak concurrency %d", peak)
	}
	second := h.client.requests[1].Messages
	results := second[len(second)-1].Content
	if len(results) != 3 {
		t.Fatalf("results: %+v", results)
	}
	for i, id := range []string{"p1", "p2", "p3"} {
		if results[i].(provider.ToolResultBlock).CallID != id {
			t.Fatalf("result order: %+v", results)
		}
	}
	checkValid(t, h.r)
}

type funcTool struct {
	name  string
	run   func(context.Context)
	calls atomic.Int32
}

func (t *funcTool) Name() string                        { return t.name }
func (t *funcTool) Description() string                 { return "synthetic" }
func (t *funcTool) Schema() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (t *funcTool) ReplayPolicy() core.ToolReplayPolicy { return core.ReplayNever }
func (t *funcTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.calls.Add(1)
	t.run(ctx)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok " + string(args)}}}, nil
}

// Parallel calls interrupted by a crash: each is reported interrupted by
// its own task, none is repeated, and the round stays paired.
func TestNativeParallelToolsCrashRecovery(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{}, 2)
	tool := &funcTool{name: "slow", run: func(ctx context.Context) {
		started <- struct{}{}
		<-ctx.Done()
	}}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("p1", "slow", `{}`), call("p2", "slow", `{}`)}},
		{text: "recovered"},
	}, tool)
	defer h.r.Close()
	c := h.nativeRoot(t)
	h.configure(t, c.ID, func(cfg *AgentConfig) { cfg.ParallelTools = true })
	h.r.Submit(ctx, c.ID, "a", "", "go")
	runCtx, crash := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.sched.Run(runCtx) }()
	<-started
	<-started
	crash()
	<-done
	h.sched.Join(ctx)
	// A new scheduler (a restarted process) recovers both calls.
	x, err := NewNativeExecutor(h.r, h.svc.currentEngine, ExecutionOptions{RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	reg := TaskRegistry{}
	x.Register(reg)
	h.r.claims = sync.Map{}
	sched := x.Scheduler(reg)
	runCtx2, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := sched.Run(runCtx2); err != nil {
		t.Fatal(err)
	}
	if tool.calls.Load() != 2 {
		t.Fatalf("interrupted calls repeated: %d", tool.calls.Load())
	}
	entries := h.entries(t, c.ID)
	if got := entryTypes(entries); got != "user assistant tool_result tool_result assistant" {
		t.Fatalf("entries: %s", got)
	}
	for _, e := range entries[2:4] {
		if !strings.Contains(e.Content, "interrupted") {
			t.Fatalf("recovered result: %q", e.Content)
		}
	}
	checkValid(t, h.r)
}

// The provider session identity is created with the conversation, survives
// configuration, reset, and compaction, and differs for forks and owned
// conversations.
func TestProviderSessionIdentity(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "one"}, {text: "two"}, {text: "three"}})
	defer h.r.Close()
	c := h.nativeRoot(t)
	if c.ProviderSession == "" || c.ProviderSession == c.ID {
		t.Fatalf("root session: %q", c.ProviderSession)
	}
	again, _ := h.r.OpenRoot(ctx, "workspace", AgentConfig{})
	if again.ProviderSession != c.ProviderSession {
		t.Fatal("reopen changed the session")
	}
	h.r.Submit(ctx, c.ID, "a", "", "one")
	h.run(t)
	h.configure(t, c.ID, func(cfg *AgentConfig) { cfg.Model = "other" })
	if _, err := h.r.Reset(ctx, c.ID, "fresh"); err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, c.ID, "a", "", "two")
	h.run(t)
	for i, req := range h.client.requests {
		if req.SessionID != c.ProviderSession {
			t.Fatalf("request %d session %q", i, req.SessionID)
		}
	}
	cur, _ := h.r.Conversation(ctx, c.ID)
	fork, err := h.r.Fork(ctx, c.ID, cur.EntrySequence, nil)
	if err != nil || fork.ProviderSession == "" || fork.ProviderSession == c.ProviderSession {
		t.Fatalf("fork session: %+v %v", fork, err)
	}
	owned, err := h.r.CreateOwnedConversation(ctx, c.ID, "owner", "k", nil)
	if err != nil || owned.ProviderSession == "" || owned.ProviderSession == c.ProviderSession {
		t.Fatalf("owned session: %+v %v", owned, err)
	}
	// A conversation written before the field existed uses its ID.
	if (Conversation{ID: "x"}).ProviderSessionID() != "x" {
		t.Fatal("fallback")
	}
}

// Extensions are selected by name, resolved at each use, and replaced in
// place by a reload. Their tools, prompt sections, and hooks apply only to
// conversations that select them, and a missing extension fails closed.
func TestNativeExtensionsAndHooks(t *testing.T) {
	ctx := context.Background()
	reg := NewExtensionRegistry()
	extTool := &effectTool{name: "ext_lookup"}
	var events []string
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		events = append(events, s)
		mu.Unlock()
	}
	yielded := false
	if err := reg.Register(Extension{
		Name:   "audit",
		Tools:  []core.Tool{extTool},
		System: "Audit section v1.",
		Hooks: Hooks{
			BeforeRequest: func(ctx context.Context, req *HookRequest) error {
				record("before_request")
				req.System += "\nHooked."
				return nil
			},
			AfterResponse: func(ctx context.Context, c Conversation, msg provider.Message) error {
				record("after_response")
				return nil
			},
			OnYield: func(ctx context.Context, c Conversation, msg provider.Message) (string, error) {
				record("on_yield")
				if !yielded {
					yielded = true
					return "please double check", nil
				}
				return "", nil
			},
			BeforeTool: func(ctx context.Context, c Conversation, call provider.ToolCallBlock) (ToolDecision, error) {
				record("before_tool:" + call.ID)
				switch call.ID {
				case "blocked":
					return ToolDecision{Block: true, Reason: "not today"}, nil
				case "rewrite":
					return ToolDecision{Args: json.RawMessage(`{"rewritten":true}`)}, nil
				}
				return ToolDecision{}, nil
			},
			AfterTool: func(ctx context.Context, c Conversation, call provider.ToolCallBlock, result core.ToolResult) (core.ToolResult, error) {
				record("after_tool:" + call.ID)
				result.Content = append(result.Content, provider.TextBlock{Text: " [audited]"})
				return result, nil
			},
			AfterTools: func(ctx context.Context, c Conversation, results []ToolRoundResult) (bool, error) {
				record("after_tools")
				return false, nil
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(Extension{Name: "bad", Tasks: []TaskDefinition{{Kind: "zot.mine"}}}); err == nil {
		t.Fatal("reserved task kind accepted")
	}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("blocked", "ext_lookup", `{}`), call("rewrite", "ext_lookup", `{"orig":1}`)}},
		{text: "first answer"},
		{text: "checked"},
	})
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{Extensions: reg})
	c := h.nativeRoot(t)
	h.configure(t, c.ID, func(cfg *AgentConfig) { cfg.Extensions = []string{"audit"} })
	sub, _ := h.r.Submit(ctx, c.ID, "a", "", "go")
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("submission: %+v", s)
	}
	first := h.client.requests[0]
	if !strings.Contains(first.System, "Audit section v1.") || !strings.Contains(first.System, "Hooked.") {
		t.Fatalf("system: %q", first.System)
	}
	found := false
	for _, tl := range first.Tools {
		found = found || tl.Name == "ext_lookup"
	}
	if !found {
		t.Fatal("extension tool not offered")
	}
	if extTool.calls.Load() != 1 {
		t.Fatalf("blocked call executed: %d", extTool.calls.Load())
	}
	entries := h.entries(t, c.ID)
	if got := entryTypes(entries); got != "user assistant tool_result tool_result assistant continue assistant" {
		t.Fatalf("entries: %s", got)
	}
	if !strings.Contains(entries[2].Content, "not today") || !strings.Contains(entries[3].Content, `"rewritten":true`) || !strings.Contains(entries[3].Content, "[audited]") {
		t.Fatalf("hooked results: %q %q", entries[2].Content, entries[3].Content)
	}
	if third := h.client.requests[2].Messages; core.MessageText(third[len(third)-1]) != "please double check" {
		t.Fatalf("continuation not sent: %+v", third)
	}
	mu.Lock()
	got := strings.Join(events, ",")
	mu.Unlock()
	for _, want := range []string{"before_request", "after_response", "before_tool:blocked", "before_tool:rewrite", "after_tool:rewrite", "after_tools", "on_yield"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hook %s did not run: %s", want, got)
		}
	}
	if strings.Contains(got, "after_tool:blocked") {
		t.Fatal("after tool ran for a blocked call")
	}
	checkValid(t, h.r)

	// Reload in place: the next request uses the new section.
	if err := reg.Register(Extension{Name: "audit", System: "Audit section v2."}); err != nil {
		t.Fatal(err)
	}
	h.client.steps = append(h.client.steps, scriptStep{text: "v2"})
	h.r.Submit(ctx, c.ID, "a", "", "again")
	h.run(t)
	last := h.client.requests[len(h.client.requests)-1]
	if !strings.Contains(last.System, "Audit section v2.") || strings.Contains(last.System, "v1") {
		t.Fatalf("reload not applied: %q", last.System)
	}
	// A conversation that does not select the extension does not see it.
	other, _ := h.r.OpenRoot(ctx, "other", AgentConfig{})
	h.client.steps = append(h.client.steps, scriptStep{text: "plain"})
	h.r.Submit(ctx, other.ID, "a", "", "plain")
	h.run(t)
	if plain := h.client.requests[len(h.client.requests)-1]; strings.Contains(plain.System, "Audit") {
		t.Fatalf("unselected extension applied: %q", plain.System)
	}
	// A missing extension fails the chain instead of running without it.
	reg.Remove("audit")
	missing, _ := h.r.Submit(ctx, c.ID, "a", "", "missing")
	h.run(t)
	if s, _ := h.r.Submission(ctx, missing.ID); s.State != "failed" {
		t.Fatalf("missing extension: %+v", s)
	}
	checkValid(t, h.r)
}

// An extension's task kind runs on the same scheduler and is resolved at
// each use.
func TestExtensionTaskDefinitions(t *testing.T) {
	ctx := context.Background()
	reg := NewExtensionRegistry()
	ran := atomic.Int32{}
	if err := reg.Register(Extension{Name: "jobs", Tasks: []TaskDefinition{{
		Kind: "jobs.count", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "run"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"run": func(ctx context.Context, tc TaskContext) (Next, error) {
				ran.Add(1)
				return Next{Outcome: "completed", Result: "counted"}, nil
			},
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(Extension{Name: "dup", Tasks: []TaskDefinition{{Kind: "jobs.count", Version: 1, Initial: func(json.RawMessage) (Next, error) { return Next{}, nil }, Phases: map[string]func(context.Context, TaskContext) (Next, error){"x": nil}}}}); err == nil {
		t.Fatal("duplicate task kind accepted")
	}
	h := newNativeHarness(t, newMemoryStore(), nil)
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{Extensions: reg})
	c := h.nativeRoot(t)
	task, err := h.r.CreateTask(ctx, TaskRegistry{"jobs.count": withReservedPhases(mustDef(t, reg, "jobs.count"))}, c.ID, TaskSpec{Kind: "jobs.count"})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t)
	done, _ := h.r.WaitTask(ctx, task.ID)
	if done.Outcome != "completed" || ran.Load() != 1 {
		t.Fatalf("extension task: %+v ran=%d", done, ran.Load())
	}
}

func mustDef(t *testing.T, reg *ExtensionRegistry, kind string) TaskDefinition {
	t.Helper()
	def, ok := reg.task(kind)
	if !ok {
		t.Fatal("definition missing")
	}
	return def
}

// Observers see nothing before its commit: streamed text arrives after the
// partial flush that records it, and with partials disabled, together with
// the response commit. A crash before the commit publishes nothing.
func TestNativePublishesOnlyCommittedEvents(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "streamed answer"}})
	defer h.r.Close()
	var mu sync.Mutex
	var seen []string
	var revisions []uint64
	h.withOptions(t, ExecutionOptions{PartialFlushInterval: -1, Sink: func(ev core.AgentEvent) {
		snap, _ := h.r.Snapshot(context.Background())
		mu.Lock()
		seen = append(seen, ev.Type())
		revisions = append(revisions, snap.Revision())
		mu.Unlock()
	}})
	c := h.nativeRoot(t)
	h.r.Submit(ctx, c.ID, "a", "", "go")
	var answered uint64
	h.client.onStream = func(provider.Request) {}
	h.run(t)
	snap := mustSnap(t, h.r)
	cur, _ := conversation(snap, c.ID)
	for seq := uint64(1); seq <= cur.EntrySequence; seq++ {
		if e, ok, _ := read[Entry](snap, entryKey(c.ID, seq)); ok && e.Type == entryAssistant {
			answered = e.Revision
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || !strings.Contains(strings.Join(seen, ","), "text_delta") {
		t.Fatalf("events: %v", seen)
	}
	for i, rev := range revisions {
		if rev < answered {
			t.Fatalf("event %s published at revision %d before the response commit %d", seen[i], rev, answered)
		}
	}

	// With partials on, every delta reaches observers only once a committed
	// partial or the response contains its text.
	h3 := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "partial text"}})
	defer h3.r.Close()
	var bad []string
	h3.withOptions(t, ExecutionOptions{PartialFlushInterval: time.Millisecond, Sink: func(ev core.AgentEvent) {
		d, ok := ev.(core.EvTextDelta)
		if !ok {
			return
		}
		p, found, _ := h3.r.Partial(context.Background(), h3.nativeRootID)
		snap, _ := h3.r.Snapshot(context.Background())
		c, _ := conversation(snap, h3.nativeRootID)
		answered := false
		for seq := uint64(1); seq <= c.EntrySequence; seq++ {
			if e, ok, _ := read[Entry](snap, entryKey(c.ID, seq)); ok && e.Type == entryAssistant && strings.Contains(e.Content, d.Delta) {
				answered = true
			}
		}
		if !answered && (!found || !strings.Contains(p.Text, d.Delta)) {
			bad = append(bad, d.Delta)
		}
	}})
	c3 := h3.nativeRoot(t)
	h3.nativeRootID = c3.ID
	h3.r.Submit(ctx, c3.ID, "a", "", "go")
	h3.run(t)
	if len(bad) != 0 {
		t.Fatalf("deltas published before their commit: %q", bad)
	}

	// A crash inside the request: nothing is published.
	seen, revisions = nil, nil
	h2 := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "lost"}})
	defer h2.r.Close()
	published := 0
	h2.withOptions(t, ExecutionOptions{PartialFlushInterval: -1, Sink: func(core.AgentEvent) { published++ }})
	c2 := h2.nativeRoot(t)
	h2.r.Submit(ctx, c2.ID, "a", "", "go")
	runCtx, crash := context.WithCancel(ctx)
	h2.client.onStream = func(provider.Request) { crash() }
	h2.sched.Run(runCtx)
	h2.sched.Join(ctx)
	if published != 0 {
		t.Fatalf("uncommitted events published: %d", published)
	}
}

// The view watch delivers operation-level changes of the conversation's
// typed documents, and the agent event stream derives message and tool
// events from commits after a fresh snapshot.
func TestWatchViewAndAgentEvents(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "done"},
	}, &effectTool{name: "effect"})
	defer h.r.Close()
	c := h.nativeRoot(t)
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	stream, err := h.r.SubscribeAgentEvents(streamCtx, c.ID, 256)
	if err != nil {
		t.Fatal(err)
	}
	start := stream.Snapshot.Revision
	h.r.Submit(ctx, c.ID, "a", "", "go")
	h.run(t)
	var types []string
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case ev := <-stream.Events:
			types = append(types, ev.Type)
			if ev.Type == "message_end" && ev.Entry != nil && ev.Entry.Content == "done" {
				break collect
			}
		case <-deadline:
			t.Fatalf("events so far: %v", types)
		}
	}
	got := strings.Join(types, ",")
	for _, want := range []string{"message_end", "message_start", "tool_execution_start", "tool_execution_end"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if strings.Index(got, "tool_execution_start") > strings.Index(got, "tool_execution_end") {
		t.Fatalf("tool events out of order: %s", got)
	}
	stop()
	if err := stream.Err(); err != nil {
		t.Fatalf("stream end: %v", err)
	}

	// View changes from the start revision.
	viewCtx, stopView := context.WithCancel(ctx)
	docs := map[string]bool{}
	errDone := errors.New("done")
	err = h.r.WatchView(viewCtx, c.ID, start, func(ch ViewChange) error {
		for _, op := range ch.Ops {
			docs[op.Doc] = true
		}
		if docs[ViewEntries] && docs[ViewExecution] && docs[ViewQueue] && docs[ViewUsage] && docs[ViewAgent] && docs[ViewProvider] {
			return errDone
		}
		return nil
	})
	stopView()
	if !errors.Is(err, errDone) {
		t.Fatalf("view docs: %v %v", docs, err)
	}

	// A subscriber that does not read falls behind and is told to
	// resubscribe. Without a lag the stream would run until the deadline
	// and end without an error.
	lagCtx, stopLag := context.WithTimeout(ctx, 5*time.Second)
	defer stopLag()
	lagging, err := h.r.SubscribeAgentEvents(lagCtx, c.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	h.client.steps = append(h.client.steps, scriptStep{text: "x"}, scriptStep{text: "y"})
	h.r.Submit(ctx, c.ID, "a", "", "one")
	h.r.Submit(ctx, c.ID, "a", "", "two")
	h.run(t)
	if err := lagging.Err(); !errors.Is(err, ErrEventLag) {
		t.Fatalf("lag: %v", err)
	}
}
