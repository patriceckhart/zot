package continuous

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

// A steered input joins the active run at the next request boundary, after the
// current tool calls settle. The provider sees the tool results before the
// new instruction, the original admission entry is not sent twice, and the
// steered submission settles with the run it joined.
func TestSteerJoinsActiveRunAtSafeBoundary(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "first"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	first, err := h.r.Submit(ctx, c.ID, "a", "", "one")
	if err != nil {
		t.Fatal(err)
	}
	var steered Submission
	steerErr := make(chan error, 1)
	go func() {
		// Wait until the tool is running, then steer and release it.
		<-tool.started
		var err error
		steered, err = h.r.SubmitWith(ctx, c.ID, "b", "steer-1", "two", SubmitOptions{Policy: PolicySteer})
		steerErr <- err
		close(tool.block)
	}()
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatalf("step: %v", err)
	}
	if err := <-steerErr; err != nil {
		t.Fatal(err)
	}
	if n := h.client.requestCount(); n != 2 {
		t.Fatalf("requests: %d", n)
	}
	second := h.client.requests[1].Messages
	roles := make([]string, len(second))
	for i, m := range second {
		roles[i] = string(m.Role)
	}
	if got := strings.Join(roles, " "); got != "user assistant tool user" {
		t.Fatalf("steered context order: %s", got)
	}
	if txt := second[3].Content[0].(provider.TextBlock).Text; txt != "two" {
		t.Fatalf("steered text: %q", txt)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant user tool_result steer assistant" {
		t.Fatalf("entries: %s", got)
	}
	for _, id := range []string{first.ID, steered.ID} {
		s, err := h.r.Submission(ctx, id)
		if err != nil || s.State != "answered" {
			t.Fatalf("submission %s: %+v %v", id, s, err)
		}
	}
	run, _, _ := h.r.Run(ctx, c.ID)
	if len(run.Submissions) != 2 {
		t.Fatalf("run submissions: %v", run.Submissions)
	}
	// Same request ID with another policy is a payload conflict.
	if _, err := h.r.SubmitWith(ctx, c.ID, "b", "steer-1", "two", SubmitOptions{}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("policy change accepted: %v", err)
	}
	report, err := h.r.CheckIntegrity(ctx)
	if err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	// Export places the steered input where it joined.
	var out strings.Builder
	if err := h.r.ExportSession(ctx, c.ID, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 || !strings.Contains(lines[4], `"two"`) || strings.Contains(lines[2], `"two"`) {
		t.Fatalf("export order:\n%s", out.String())
	}
}

// Without an active run a steer submission is an ordinary queued input, and
// reject-when-busy refuses admission while a run is active. Queue limits
// fail closed.
func TestSubmitPoliciesIdleBusyAndLimits(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "done"},
		{text: "later"},
	}, tool)
	defer h.r.Close()
	c := h.root(t)
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "", "idle steer", SubmitOptions{Policy: PolicySteer, RejectBusy: true}); err != nil {
		t.Fatalf("idle steer: %v", err)
	}
	// The idle submission started a chain; one more input fills a queue of
	// one, the next is refused.
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "", "x", SubmitOptions{MaxQueue: 1}); err != nil {
		t.Fatalf("first queued: %v", err)
	}
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "", "x", SubmitOptions{MaxQueue: 1}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queue limit: %v", err)
	}
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "", "x", SubmitOptions{Policy: "later"}); err == nil {
		t.Fatal("unknown policy accepted")
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(ctx, c.ID)
		done <- err
	}()
	<-tool.started
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "", "busy", SubmitOptions{RejectBusy: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("reject busy: %v", err)
	}
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "", "queued", SubmitOptions{}); err != nil {
		t.Fatalf("queue while busy: %v", err)
	}
	close(tool.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The queued input was not steered into the first run: it got its own run.
	if n := h.client.requestCount(); n != 3 {
		t.Fatalf("requests: %d", n)
	}
	// The queued "x" was admitted before the first response and is
	// answered by the second chain, together with "queued", both placed
	// after the first answer.
	if got := entryTypes(h.entries(t, c.ID)); got != "user user assistant user tool_result assistant steer steer assistant" {
		t.Fatalf("entries: %s", got)
	}
}
