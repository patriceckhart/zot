package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// scriptedClient answers each request from a script. A step is either a
// complete assistant message or an error. Requests are recorded.
type scriptedClient struct {
	mu       sync.Mutex
	steps    []scriptStep
	requests []provider.Request
	onStream func(provider.Request)
}

type scriptStep struct {
	text  string
	calls []provider.ToolCallBlock
	err   error
	stop  provider.StopReason
}

func (c *scriptedClient) Name() string { return "synthetic" }
func (c *scriptedClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	if len(c.steps) == 0 {
		c.mu.Unlock()
		return nil, errors.New("script exhausted")
	}
	step := c.steps[0]
	c.steps = c.steps[1:]
	hook := c.onStream
	c.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	ch := make(chan provider.Event, 8)
	go func() {
		defer close(ch)
		ch <- provider.EventStart{Model: req.Model, Provider: "synthetic"}
		if step.err != nil {
			ch <- provider.EventDone{Stop: provider.StopError, Err: step.err}
			return
		}
		msg := provider.Message{Role: provider.RoleAssistant, Time: time.Now().UTC()}
		if step.text != "" {
			ch <- provider.EventTextDelta{Delta: step.text}
			msg.Content = append(msg.Content, provider.TextBlock{Text: step.text})
		}
		for _, call := range step.calls {
			msg.Content = append(msg.Content, call)
		}
		ch <- provider.EventUsage{Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}}
		stop := step.stop
		if stop == "" {
			stop = provider.StopEnd
			if len(step.calls) > 0 {
				stop = provider.StopToolUse
			}
		}
		ch <- provider.EventDone{Stop: stop, Message: msg}
	}()
	return ch, nil
}

func (c *scriptedClient) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// effectTool records every execution so tests can prove exactly-once or
// not-repeated behaviour. replay selects its declared policy.
type effectTool struct {
	name   string
	replay core.ToolReplayPolicy
	calls  atomic.Int32
	block  chan struct{}
	fail   bool
	// started receives once per execution before blocking, when set.
	started chan struct{}
}

func (t *effectTool) Name() string                        { return t.name }
func (t *effectTool) Description() string                 { return "synthetic effect" }
func (t *effectTool) Schema() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (t *effectTool) ReplayPolicy() core.ToolReplayPolicy { return t.replay }
func (t *effectTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.calls.Add(1)
	if t.started != nil {
		t.started <- struct{}{}
	}
	if t.block != nil {
		select {
		case <-t.block:
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}
	if t.fail {
		return core.ToolResult{}, errors.New("synthetic tool failure")
	}
	progress("working")
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "effect:" + string(args)}}}, nil
}

func newMemoryStore() storage.Store { return memory.Open() }

func call(id, name, args string) provider.ToolCallBlock {
	return provider.ToolCallBlock{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

type harness struct {
	r      *Runtime
	client *scriptedClient
	tools  core.Registry
	svc    *Service
	guard  func(provider.ToolCallBlock) (bool, string, json.RawMessage)
	built  atomic.Int32
}

func newHarness(t *testing.T, store storage.Store, steps []scriptStep, tools ...core.Tool) *harness {
	t.Helper()
	r, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{r: r, client: &scriptedClient{steps: steps}, tools: core.NewRegistry(tools...)}
	h.svc, err = NewService(r, EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		h.built.Add(1)
		a := core.NewAgent(h.client, "scripted", "You are synthetic.", h.tools)
		a.MaxRetries = 0
		if h.guard != nil {
			a.BeforeToolExecute = h.guard
		}
		return a, nil
	}), ExecutionOptions{RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) root(t *testing.T) Conversation {
	t.Helper()
	c, err := h.r.OpenRoot(context.Background(), "workspace", AgentConfig{Provider: "synthetic", Model: "scripted", Instructions: "Answer briefly."})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// entries lists a conversation's transcript entries without context
// entries, which record request shape rather than conversation content and
// are checked by their own tests. allEntries keeps them.
func (h *harness) entries(t *testing.T, id string) []Entry {
	t.Helper()
	all := h.allEntries(t, id)
	out := all[:0:0]
	for _, e := range all {
		if e.Type != entryContext {
			out = append(out, e)
		}
	}
	return out
}

func (h *harness) allEntries(t *testing.T, id string) []Entry {
	t.Helper()
	snap, err := h.r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := snap.Page("entry/"+id+"/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		var e Entry
		if err := json.Unmarshal(row.Value, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func entryTypes(entries []Entry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.Type
	}
	return strings.Join(parts, " ")
}

func TestExecutionAnswersQueuedInput(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{"n":1}`), call("c2", "effect", `{"n":2}`)}},
		{text: "done"},
	}, &effectTool{name: "effect"})
	defer h.r.Close()
	c := h.root(t)
	s, err := h.r.Submit(ctx, c.ID, "actor", "req-1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	// Admission claims the input for a new chain in the same commit, but
	// nothing executes without a scheduler or Step.
	if cur, _ := h.r.Submission(ctx, s.ID); cur.State != "running" || h.client.requestCount() != 0 {
		t.Fatalf("admission: %+v requests=%d", cur, h.client.requestCount())
	}
	var eventsMu sync.Mutex
	var events []string
	// Each task has its own event gate, so deliveries from different tool
	// tasks can overlap even though each gate preserves its own event order.
	h.svc.opts.Sink = func(ev core.AgentEvent) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, ev.Type())
	}
	run, ok, err := h.svc.Step(ctx, c.ID)
	if err != nil || !ok || run.Phase != "done" || run.Outcome != "completed" || run.Turn != 2 {
		t.Fatalf("step: %+v %v %v", run, ok, err)
	}
	settled, err := h.r.WaitSubmission(ctx, s.ID)
	if err != nil || settled.State != "answered" {
		t.Fatalf("settled: %+v %v", settled, err)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result tool_result assistant" {
		t.Fatalf("entries: %s", got)
	}
	if h.client.requestCount() != 2 {
		t.Fatalf("requests: %d", h.client.requestCount())
	}
	second := h.client.requests[1]
	if len(second.Messages) != 3 || second.Messages[0].Role != provider.RoleUser || second.Messages[2].Role != provider.RoleTool || len(second.Messages[2].Content) != 2 {
		t.Fatalf("second request context: %+v", second.Messages)
	}
	if !strings.Contains(second.System, "Answer briefly.") || second.SessionID != c.ProviderSessionID() || c.ProviderSession == "" || c.ProviderSession == c.ID {
		t.Fatalf("configuration not applied: %q %q", second.System, second.SessionID)
	}
	eventsMu.Lock()
	observedEvents := append([]string(nil), events...)
	eventsMu.Unlock()
	if !strings.Contains(strings.Join(observedEvents, ","), "tool_progress") || !strings.Contains(strings.Join(observedEvents, ","), "tool_result") {
		t.Fatalf("events: %v", observedEvents)
	}
	// Retrying the request ID returns the live, settled submission.
	again, err := h.r.Submit(ctx, c.ID, "actor", "req-1", "hello")
	if err != nil || again.ID != s.ID || again.State != "answered" {
		t.Fatalf("retry: %+v %v", again, err)
	}
	if _, ok, err := h.svc.Step(ctx, c.ID); err != nil || ok {
		t.Fatalf("idle step did work: %v %v", ok, err)
	}
	report, err := h.r.CheckIntegrity(ctx)
	if err != nil || !report.Valid || report.ActiveRuns != 0 {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	var exported strings.Builder
	if err := h.r.ExportSession(ctx, c.ID, &exported); err != nil {
		t.Fatal(err)
	}
	if strings.Count(exported.String(), `"type":"message"`) != 5 || !strings.Contains(exported.String(), `"call_id":"c2"`) {
		t.Fatalf("export: %s", exported.String())
	}
}

func TestExecutionRetriesTransientThenFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{err: errors.New("HTTP 503 service unavailable")},
		{err: errors.New("HTTP 503 service unavailable")},
		{err: errors.New("HTTP 503 service unavailable")},
	})
	defer h.r.Close()
	c := h.root(t)
	s, _ := h.r.Submit(ctx, c.ID, "actor", "", "hello")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "failed" || run.Attempt != 3 || !strings.Contains(run.Error, "503") {
		t.Fatalf("run: %+v %v", run, err)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user attempt attempt attempt" {
		t.Fatalf("entries: %s", got)
	}
	if cur, _ := h.r.Submission(ctx, s.ID); cur.State != "failed" {
		t.Fatalf("submission: %+v", cur)
	}
	// A new submission starts a new run; failed attempts stay out of context.
	h.client.steps = []scriptStep{{text: "recovered"}}
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "again"); err != nil {
		t.Fatal(err)
	}
	run, _, err = h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("second run: %+v %v", run, err)
	}
	last := h.client.requests[len(h.client.requests)-1]
	if len(last.Messages) != 2 {
		t.Fatalf("attempt entries leaked into context: %+v", last.Messages)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestExecutionNonRetryableError(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{err: errors.New("invalid api key")}})
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "hello")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "failed" || run.Attempt != 1 || h.client.requestCount() != 1 {
		t.Fatalf("run: %+v %v requests=%d", run, err, h.client.requestCount())
	}
}

func TestExecutionBlockedToolAndFailingTool(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`), call("c2", "broken", `{}`), call("c3", "missing", `{}`)}},
		{text: "done"},
	}, &effectTool{name: "effect"}, &effectTool{name: "broken", fail: true})
	defer h.r.Close()
	h.guard = func(tc provider.ToolCallBlock) (bool, string, json.RawMessage) {
		if tc.ID == "c1" {
			return false, "denied by test guard", nil
		}
		return true, "", nil
	}
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "hello")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	if h.tools["effect"].(*effectTool).calls.Load() != 0 {
		t.Fatal("blocked tool executed")
	}
	results := h.client.requests[1].Messages
	var texts []string
	for _, m := range results {
		if m.Role != provider.RoleTool {
			continue
		}
		for _, c := range m.Content {
			block := c.(provider.ToolResultBlock)
			if !block.IsError {
				t.Fatalf("expected error result: %+v", block)
			}
			texts = append(texts, core.MessageText(provider.Message{Content: block.Content}))
		}
	}
	if len(texts) != 3 || texts[0] != "denied by test guard" || !strings.Contains(texts[1], "synthetic tool failure") || !strings.Contains(texts[2], "unknown tool") {
		t.Fatalf("results: %v", texts)
	}
}

func TestExecutionCancellationLeavesRecoverableRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := &effectTool{name: "effect", block: make(chan struct{})}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	s, _ := h.r.Submit(ctx, c.ID, "actor", "", "hello")
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(ctx, c.ID)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for tool.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("tool never started")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("step: %v", err)
	}
	run, ok, err := h.r.Run(context.Background(), c.ID)
	if err != nil || !ok || run.Phase != "tools" || run.Tools[0].State != "running" {
		t.Fatalf("run after cancel: %+v %v", run, err)
	}
	if cur, _ := h.r.Submission(context.Background(), s.ID); cur.State != "running" {
		t.Fatalf("submission: %+v", cur)
	}
	// Recovery: the tool has no replay policy, so it is reported, not rerun.
	run, _, err = h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" || tool.calls.Load() != 1 {
		t.Fatalf("recovered: %+v %v calls=%d", run, err, tool.calls.Load())
	}
	if len(run.Notices) == 0 || !strings.Contains(run.Notices[len(run.Notices)-1], "not replayed") {
		t.Fatalf("notices: %v", run.Notices)
	}
	result := h.client.requests[1].Messages[2].Content[0].(provider.ToolResultBlock)
	if !result.IsError || !strings.Contains(core.MessageText(provider.Message{Content: result.Content}), "interrupted") {
		t.Fatalf("interrupted result: %+v", result)
	}
}

func TestExecutionReplaySafeToolRerunsAfterInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := &effectTool{name: "lookup", replay: core.ReplaySafe, block: make(chan struct{})}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "lookup", `{"q":1}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "hello")
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(ctx, c.ID)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for tool.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("tool never started")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	tool.block = nil
	// Replay requires fresh authorization. Deny it first.
	h.guard = func(provider.ToolCallBlock) (bool, string, json.RawMessage) { return false, "policy changed", nil }
	run, _, err := h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" || tool.calls.Load() != 1 {
		t.Fatalf("denied replay executed: %+v %v calls=%d", run, err, tool.calls.Load())
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "replay denied") {
		t.Fatalf("notices: %v", run.Notices)
	}
}

func TestExecutionReplaySafeToolAllowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := &effectTool{name: "lookup", replay: core.ReplaySafe, block: make(chan struct{})}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "lookup", `{"q":1}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "hello")
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(ctx, c.ID)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for tool.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("tool never started")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	tool.block = nil
	run, _, err := h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" || tool.calls.Load() != 2 {
		t.Fatalf("safe replay: %+v %v calls=%d", run, err, tool.calls.Load())
	}
	result := h.client.requests[1].Messages[2].Content[0].(provider.ToolResultBlock)
	if result.IsError || !strings.Contains(core.MessageText(provider.Message{Content: result.Content}), `effect:{"q":1}`) {
		t.Fatalf("replayed result: %+v", result)
	}
}

func TestExecutionContinuesLegacyImport(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "continuing"}})
	defer h.r.Close()
	c, err := h.r.ImportSession(ctx, strings.NewReader(`{"type":"meta","meta":{"id":"legacy","provider":"synthetic","model":"scripted"}}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"old question"}]}}
{"type":"message","message":{"role":"assistant","content":[{"id":"t1","name":"effect","arguments":{}}]}}
`))
	if err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, c.ID, "actor", "", "new question")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	msgs := h.client.requests[0].Messages
	if len(msgs) != 4 || msgs[2].Role != provider.RoleTool || !msgs[2].Content[0].(provider.ToolResultBlock).IsError {
		t.Fatalf("legacy context not repaired: %+v", msgs)
	}
}

func TestExecutionConcurrentStepsConflict(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", block: make(chan struct{})}
	// Either stepper may win the result commit; the loser may still have
	// sent a request. Provide enough answers for any interleaving.
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "done"},
		{text: "done"},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "hello")
	first := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(ctx, c.ID)
		first <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for tool.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("tool never started")
		}
		time.Sleep(time.Millisecond)
	}
	// A second stepper sees the running intent and tries to resolve it as an
	// interrupted call while the first stepper is still executing it. Both
	// race to commit a result for c1. Exactly one result entry may exist and
	// the loser must report ErrBusy; the tool must not run twice.
	go func() { close(tool.block) }()
	_, _, secondErr := h.svc.Step(ctx, c.ID)
	firstErr := <-first
	for _, err := range []error{firstErr, secondErr} {
		if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("unexpected stepper error: %v", err)
		}
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool executed %d times", tool.calls.Load())
	}
	results := 0
	for _, e := range h.entries(t, c.ID) {
		if e.Type == entryToolResult {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("tool result entries: %d", results)
	}
	// The store still converges: finish whatever remains.
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if run, _, err := h.r.Run(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("converge: %+v %v", run, err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestExecutionMaxTurns(t *testing.T) {
	ctx := context.Background()
	steps := make([]scriptStep, 0, 10)
	for i := 0; i < 10; i++ {
		steps = append(steps, scriptStep{calls: []provider.ToolCallBlock{call(fmt.Sprint("c", i), "effect", `{}`)}})
	}
	h := newHarness(t, newMemoryStore(), steps, &effectTool{name: "effect"})
	defer h.r.Close()
	h.svc.opts.MaxTurns = 3
	c := h.root(t)
	s, _ := h.r.Submit(ctx, c.ID, "actor", "", "loop")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "failed" || !strings.Contains(run.Error, "max turns") || h.client.requestCount() != 3 {
		t.Fatalf("run: %+v %v requests=%d", run, err, h.client.requestCount())
	}
	if cur, _ := h.r.Submission(ctx, s.ID); cur.State != "failed" {
		t.Fatalf("submission: %+v", cur)
	}
}

// Crash matrix: terminate the stepper at every commit boundary of a run that
// includes a tool round, reopen the journal, and prove recovery converges
// without duplicate effects or broken pairing.
func TestExecutionCrashAtEveryCommit(t *testing.T) {
	for _, backend := range []string{"journal", "sqlite"} {
		t.Run(backend, func(t *testing.T) { crashAtEveryCommit(t, backend) })
	}
}

func crashAtEveryCommit(t *testing.T, backend string) {
	ctx := context.Background()
	for crashAt := 1; ; crashAt++ {
		path := filepath.Join(t.TempDir(), "store")
		store, err := openCrashStore(ctx, backend, path)
		if err != nil {
			t.Fatal(err)
		}
		tool := &effectTool{name: "effect"}
		steps := []scriptStep{
			{calls: []provider.ToolCallBlock{call("c1", "effect", `{"n":1}`), call("c2", "effect", `{"n":2}`)}},
			{text: "done"},
		}
		h := newHarness(t, store, steps, tool)
		c := h.root(t)
		h.r.Submit(ctx, c.ID, "actor", "req", "hello")
		crashed := errors.New("synthetic crash")
		h.r.store = &crashStore{Store: store, at: crashAt, err: crashed}
		_, _, err = h.svc.Step(ctx, c.ID)
		storeCrashed := errors.Is(err, crashed)
		if err != nil && !storeCrashed {
			t.Fatalf("crash %d: unexpected error %v", crashAt, err)
		}
		h.r.store = store
		h.r.Close()
		if !storeCrashed {
			// The run finished before the crash point: every boundary covered.
			if crashAt == 1 {
				t.Fatal("matrix never crashed")
			}
			return
		}
		// Reopen and recover with a fresh process state.
		store, err = openCrashStore(ctx, backend, path)
		if err != nil {
			t.Fatalf("crash %d: reopen %v", crashAt, err)
		}
		recovered := &effectTool{name: "effect"}
		h2 := newHarness(t, store, []scriptStep{{text: "done"}, {text: "done"}}, recovered)
		if report, err := h2.r.CheckIntegrity(ctx); err != nil || !report.Valid {
			t.Fatalf("crash %d: integrity before recovery %+v %v", crashAt, report, err)
		}
		// ok is false only when the crash landed after the final commit, in
		// which case the recovered process has nothing to do.
		run, ok, err := h2.svc.Step(ctx, c.ID)
		if err == nil && !ok {
			// The crash landed after the final commit: nothing to recover.
			run, _, err = h2.r.Run(ctx, c.ID)
		}
		if err != nil || run.Outcome != "completed" {
			t.Fatalf("crash %d: recovery %+v %v %v", crashAt, run, ok, err)
		}
		total := int(tool.calls.Load() + recovered.calls.Load())
		if total > 2 {
			t.Fatalf("crash %d: tool executed %d times, effects repeated", crashAt, total)
		}
		// Every assistant tool call has exactly one result in context, and no
		// request was sent with a dangling call.
		for _, req := range h2.client.requests {
			if msgs := provider.RepairOrphanedToolResults(req.Messages); len(msgs) != len(req.Messages) {
				t.Fatalf("crash %d: dangling pairing sent to provider", crashAt)
			}
			for i, m := range req.Messages {
				if m.Role != provider.RoleAssistant {
					continue
				}
				for _, blk := range m.Content {
					tc, ok := blk.(provider.ToolCallBlock)
					if !ok {
						continue
					}
					found := false
					for _, later := range req.Messages[i+1:] {
						for _, b := range later.Content {
							if tr, ok := b.(provider.ToolResultBlock); ok && tr.CallID == tc.ID {
								found = true
							}
						}
					}
					if !found {
						t.Fatalf("crash %d: call %s without result", crashAt, tc.ID)
					}
				}
			}
		}
		sub, err := h2.r.Submit(ctx, c.ID, "actor", "req", "hello")
		if err != nil || sub.State != "answered" {
			t.Fatalf("crash %d: submission %+v %v", crashAt, sub, err)
		}
		if report, err := h2.r.CheckIntegrity(ctx); err != nil || !report.Valid || report.ActiveRuns != 0 {
			t.Fatalf("crash %d: integrity after recovery %+v %v", crashAt, report, err)
		}
		h2.r.Close()
	}
}

// crashStore fails the n-th commit after the real store persisted it, which
// models a process that dies after the barrier but before observing the
// acknowledgement. Later commits are rejected like a dead process.
type crashStore struct {
	storage.Store
	at    int
	err   error
	mu    sync.Mutex
	count int
	dead  bool
}

func (s *crashStore) Commit(ctx context.Context, m storage.Mutation) (storage.Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return storage.Commit{}, s.err
	}
	s.count++
	c, err := s.Store.Commit(ctx, m)
	if err != nil {
		return c, err
	}
	if s.count == s.at {
		s.dead = true
		return storage.Commit{}, s.err
	}
	return c, nil
}

func (s *crashStore) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	s.mu.Lock()
	dead := s.dead
	s.mu.Unlock()
	if dead {
		return nil, s.err
	}
	return s.Store.Snapshot(ctx)
}

func (s *crashStore) Wait(ctx context.Context, after uint64) error {
	s.mu.Lock()
	dead := s.dead
	s.mu.Unlock()
	if dead {
		return s.err
	}
	return s.Store.Wait(ctx, after)
}
