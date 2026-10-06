package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// receiverTool models an external receiver that enforces an idempotency key:
// repeated executions with the same key have one effect. It optionally
// reconciles by key.
type receiverTool struct {
	mu       sync.Mutex
	effects  map[string]int
	keys     []string
	policy   core.ToolReplayPolicy
	block    chan struct{}
	started  chan struct{}
	outcome  core.ReconcileOutcome
	recErr   error
	recCalls int
}

func (t *receiverTool) Name() string                        { return "send" }
func (t *receiverTool) Description() string                 { return "synthetic external effect" }
func (t *receiverTool) Schema() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (t *receiverTool) ReplayPolicy() core.ToolReplayPolicy { return t.policy }
func (t *receiverTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	key := core.ToolOperationKey(ctx)
	t.mu.Lock()
	if t.effects == nil {
		t.effects = map[string]int{}
	}
	t.keys = append(t.keys, key)
	// The receiver deduplicates on the key: a second delivery is a no-op.
	t.effects[key]++
	t.mu.Unlock()
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
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "sent with key " + key}}}, nil
}
func (t *receiverTool) Reconcile(ctx context.Context, key string, args json.RawMessage) (core.ReconcileOutcome, core.ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recCalls++
	if t.recErr != nil {
		return core.ReconcileUnknown, core.ToolResult{}, t.recErr
	}
	if t.outcome != "" {
		return t.outcome, core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "reconciled " + key}}}, nil
	}
	if t.effects[key] > 0 {
		return core.ReconcileCompleted, core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "already sent with key " + key}}}, nil
	}
	return core.ReconcileNotStarted, core.ToolResult{}, nil
}

func (t *receiverTool) distinctKeys() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.effects)
}

func interruptDuringTool(t *testing.T, h *harness, c Conversation, started chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(ctx, c.ID)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted step: %v", err)
	}
}

// An idempotent external action has two attempts but one effect: the replay
// carries the same operation key, and the receiver deduplicates on it.
func TestReplayIdempotentUsesStableOperationKey(t *testing.T) {
	ctx := context.Background()
	tool := &receiverTool{policy: core.ReplayIdempotent, block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "send", `{"to":"x"}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "send it")
	interruptDuringTool(t, h, c, tool.started)
	plan, _ := h.r.RecoveryPreview(ctx)
	if len(plan.Actions) != 1 || plan.Actions[0].Action != "replay" || !plan.Actions[0].Automatic {
		t.Fatalf("plan: %+v", plan)
	}
	tool.block = nil
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	if len(tool.keys) != 2 || tool.keys[0] != tool.keys[1] || !strings.HasPrefix(tool.keys[0], "zot-op-") {
		t.Fatalf("operation keys: %v", tool.keys)
	}
	if tool.distinctKeys() != 1 {
		t.Fatalf("effects: %d", tool.distinctKeys())
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "policy idempotent") {
		t.Fatalf("notices: %v", run.Notices)
	}
}

// A reconciling tool is asked before anything runs: completed recovers the
// result without re-execution, not started executes once, unknown and errors
// are reported to the model and never retried.
func TestReplayReconcileOutcomes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		outcome   core.ReconcileOutcome
		recErr    error
		wantCalls int
		wantText  string
		isError   bool
		// wantRec is how often reconciliation is asked. An unanswered
		// reconciliation is asked again with backoff, then reported.
		wantRec int
	}{
		{"completed", "", nil, 1, "already sent", false, 1},
		{"not started", core.ReconcileNotStarted, nil, 2, "sent with key", false, 1},
		{"unknown", core.ReconcileUnknown, nil, 1, "interrupted", true, maxUnresolved + 1},
		{"error", "", errors.New("lookup failed"), 1, "interrupted", true, maxUnresolved + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := &receiverTool{policy: core.ReplayReconcile, block: make(chan struct{}), started: make(chan struct{}, 1), outcome: tc.outcome, recErr: tc.recErr}
			h := newHarness(t, newMemoryStore(), []scriptStep{
				{calls: []provider.ToolCallBlock{call("c1", "send", `{}`)}},
				{text: "done"},
			}, tool)
			defer h.r.Close()
			c := h.root(t)
			h.r.Submit(ctx, c.ID, "actor", "", "send it")
			interruptDuringTool(t, h, c, tool.started)
			plan, _ := h.r.RecoveryPreview(ctx)
			if plan.Actions[0].Action != "reconcile" {
				t.Fatalf("plan: %+v", plan)
			}
			tool.block = nil
			// An unanswered reconciliation returns ErrToolOutcomeUnknown and
			// is asked again on the next Step, until the limit reports it.
			var run Run
			var err error
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				run, _, err = h.svc.Step(ctx, c.ID)
				if !errors.Is(err, core.ErrToolOutcomeUnknown) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err != nil || run.Outcome != "completed" {
				t.Fatalf("run: %+v %v", run, err)
			}
			if tool.recCalls != tc.wantRec || len(tool.keys) != tc.wantCalls {
				t.Fatalf("reconcile=%d executions=%d", tool.recCalls, len(tool.keys))
			}
			result := h.client.requests[1].Messages[2].Content[0].(provider.ToolResultBlock)
			text := core.MessageText(provider.Message{Content: result.Content})
			if result.IsError != tc.isError || !strings.Contains(text, tc.wantText) {
				t.Fatalf("result: error=%v %q", result.IsError, text)
			}
		})
	}
}

// Policies never widen: a tool declaring reconcile without a reconciler, or
// whose live policy differs from the stored one, is reported, not replayed.
func TestReplayPolicyMismatchIsReported(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", replay: core.ReplayIdempotent, block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "go")
	interruptDuringTool(t, h, c, tool.started)
	// The tool was redeployed with a stricter policy before recovery.
	tool.replay = core.ReplayNever
	tool.block = nil
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" || tool.calls.Load() != 1 {
		t.Fatalf("mismatch replayed: %+v %v calls=%d", run, err, tool.calls.Load())
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "stored policy idempotent, live policy never") {
		t.Fatalf("notices: %v", run.Notices)
	}
	// Declaring reconcile without implementing it is never.
	plain := &effectTool{name: "x", replay: core.ReplayReconcile}
	if core.ReplayPolicyOf(plain) != core.ReplayNever {
		t.Fatal("reconcile without reconciler widened replay")
	}
	if core.ToolOperationKey(context.Background()) != "" {
		t.Fatal("operation key outside a durable call")
	}
}
