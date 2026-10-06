package continuous

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestUsageLedgerKnownAndUnknown(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{err: errors.New("HTTP 503 service unavailable")},
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "done"},
	}, &effectTool{name: "effect"})
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "hello")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	totals, err := h.r.Usage(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// One failed attempt without usage, two successful attempts with 10/5 each.
	if totals.Known != 2 || totals.Unknown != 1 || totals.Usage.InputTokens != 20 || totals.Usage.OutputTokens != 10 {
		t.Fatalf("totals: %+v", totals)
	}
	cur, _ := h.r.Conversation(ctx, c.ID)
	if cur.UsageSequence != 3 {
		t.Fatalf("usage sequence: %d", cur.UsageSequence)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	if _, err := h.r.Usage(ctx, "missing"); err != nil {
		t.Fatalf("empty ledger: %v", err)
	}
}

func TestAbortDuringToolRound(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", block: make(chan struct{})}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`), call("c2", "effect", `{}`)}},
		{text: "never reached"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	if _, err := h.r.Abort(ctx, c.ID); !errors.Is(err, ErrNoRun) {
		t.Fatalf("abort without run: %v", err)
	}
	s, _ := h.r.Submit(ctx, c.ID, "actor", "", "hello")
	stepCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(stepCtx, c.ID)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for tool.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("tool never started")
		}
		time.Sleep(time.Millisecond)
	}
	// Abort intent commits while the tool is still running.
	run, err := h.r.Abort(ctx, c.ID)
	if err != nil || !run.AbortRequested {
		t.Fatalf("abort: %+v %v", run, err)
	}
	again, err := h.r.Abort(ctx, c.ID)
	if err != nil || again.Revision != run.Revision {
		t.Fatalf("second abort changed state: %+v %v", again, err)
	}
	plan, err := h.r.RecoveryPreview(ctx)
	if err != nil || len(plan.Actions) != 1 || plan.Actions[0].Action != "abort" || len(plan.Actions[0].Interrupted) != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	// Let the running tool finish; the stepper must then honour the abort
	// instead of starting the second call.
	close(tool.block)
	if err := <-done; err != nil {
		t.Fatalf("step: %v", err)
	}
	cancel()
	final, _, _ := h.r.Run(ctx, c.ID)
	if final.Phase != "done" || final.Outcome != "aborted" || tool.calls.Load() != 1 {
		t.Fatalf("final: %+v calls=%d", final, tool.calls.Load())
	}
	if sub, _ := h.r.Submission(ctx, s.ID); sub.State != "aborted" {
		t.Fatalf("submission: %+v", sub)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result tool_result" {
		t.Fatalf("entries: %s", got)
	}
	if h.client.requestCount() != 1 {
		t.Fatal("aborted run sent another request")
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid || report.ActiveRuns != 0 {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	// Model context after the abort is paired, so a new run can continue.
	h.client.steps = []scriptStep{{text: "continuing"}}
	h.r.Submit(ctx, c.ID, "actor", "", "next")
	if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("after abort: %+v %v", run, err)
	}
	msgs := h.client.requests[1].Messages
	if len(msgs) != 4 || len(msgs[2].Content) != 2 {
		t.Fatalf("context after abort: %+v", msgs)
	}
}

func TestAbortBeforeStepHonoured(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "never"}})
	defer h.r.Close()
	c := h.root(t)
	s, _ := h.r.Submit(ctx, c.ID, "actor", "", "hello")
	// Admission starts the chain without executing; abort before any model
	// request.
	run, ok, _ := h.r.Run(ctx, c.ID)
	if !ok || run.Phase != "request" {
		t.Fatalf("run: %+v", run)
	}
	if _, err := h.r.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	final, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || final.Outcome != "aborted" || h.client.requestCount() != 0 {
		t.Fatalf("abort before request: %+v %v requests=%d", final, err, h.client.requestCount())
	}
	if sub, _ := h.r.Submission(ctx, s.ID); sub.State != "aborted" {
		t.Fatalf("submission: %+v", sub)
	}
	if totals, _ := h.r.Usage(ctx, c.ID); totals.Known != 0 || totals.Unknown != 0 {
		t.Fatalf("no attempt should be charged: %+v", totals)
	}
}

func TestRecoveryPreviewClassifiesRuns(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), nil)
	defer h.r.Close()
	plan, err := h.r.RecoveryPreview(ctx)
	if err != nil || len(plan.Actions) != 0 || plan.Blocked != 0 {
		t.Fatalf("empty plan: %+v %v", plan, err)
	}
	cases := []struct {
		run       Run
		action    string
		automatic bool
	}{
		{Run{Phase: "request", Turn: 2, Attempt: 1}, "resend", true},
		{Run{Phase: "tools", Tools: []ToolIntent{{CallID: "a", State: "pending"}}}, "continue", true},
		{Run{Phase: "tools", Tools: []ToolIntent{{CallID: "a", State: "running", Replay: core.ReplaySafe}}}, "replay", true},
		{Run{Phase: "tools", Tools: []ToolIntent{{CallID: "a", State: "running", Replay: core.ReplayNever}, {CallID: "b", State: "pending"}}}, "report", false},
		{Run{Phase: "tools", AbortRequested: true, Tools: []ToolIntent{{CallID: "a", State: "running"}}}, "abort", true},
		{Run{Phase: "weird"}, "report", false},
	}
	for _, tc := range cases {
		a := planRun(tc.run, false)
		if a.Action != tc.action || a.Automatic != tc.automatic || a.Detail == "" {
			t.Fatalf("%+v -> %+v", tc.run, a)
		}
		if strings.Contains(a.Detail, "{") {
			t.Fatalf("detail is not user language: %q", a.Detail)
		}
	}
}
