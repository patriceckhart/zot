package continuous

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

// A conversation's configuration narrows the host's tools. It can remove
// tools from the request and refuse calls at authorization, never add.
func TestConversationToolPermissions(t *testing.T) {
	ctx := context.Background()
	safe := &effectTool{name: "lookup"}
	danger := &effectTool{name: "destroy"}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "destroy", `{}`), call("c2", "lookup", `{}`)}},
		{text: "done"},
	}, safe, danger)
	defer h.r.Close()
	c, err := h.r.OpenRoot(ctx, "restricted", AgentConfig{Provider: "synthetic", Model: "scripted", DenyTools: []string{"destroy"}})
	if err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, c.ID, "a", "", "go")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	// The request did not offer the denied tool.
	names := []string{}
	for _, tl := range h.client.requests[0].Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "lookup" {
		t.Fatalf("offered tools: %v", names)
	}
	// The model called it anyway: refused before authorization, not executed.
	if danger.calls.Load() != 0 || safe.calls.Load() != 1 {
		t.Fatalf("calls: destroy=%d lookup=%d", danger.calls.Load(), safe.calls.Load())
	}
	results := h.client.requests[1].Messages[2].Content
	if !results[0].(provider.ToolResultBlock).IsError || !strings.Contains(core_text(results[0]), "not permitted") {
		t.Fatalf("denied result: %+v", results[0])
	}
	// An allowlist works the same way, and unknown names grant nothing.
	cur, _ := h.r.Conversation(ctx, c.ID)
	if _, err := h.r.Configure(ctx, c.ID, cur.Revision, AgentConfig{Provider: "synthetic", Model: "scripted", Tools: []string{"lookup", "not-a-host-tool"}}); err != nil {
		t.Fatal(err)
	}
	h.client.steps = append(h.client.steps, scriptStep{text: "again"})
	h.r.Submit(ctx, c.ID, "a", "", "again")
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	names = names[:0]
	for _, tl := range h.client.requests[2].Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "lookup" {
		t.Fatalf("allowlisted tools: %v", names)
	}
}

func core_text(c provider.Content) string {
	if tr, ok := c.(provider.ToolResultBlock); ok {
		var sb strings.Builder
		for _, b := range tr.Content {
			if tb, ok := b.(provider.TextBlock); ok {
				sb.WriteString(tb.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// Queued submissions can be withdrawn (settling as withdrawn, history kept,
// request-ID retry returns the withdrawn record) and reordered to the front.
func TestQueueWithdrawAndReorder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "busy answer"}, {text: "first answer"}, {text: "second answer"}})
	defer h.r.Close()
	c := h.root(t)
	// The first input starts the chain at admission; the next ones queue.
	busy, _ := h.r.Submit(ctx, c.ID, "u", "", "busy")
	a, _ := h.r.Submit(ctx, c.ID, "u", "req-a", "a")
	b, _ := h.r.Submit(ctx, c.ID, "u", "", "b")
	d, _ := h.r.Submit(ctx, c.ID, "u", "", "d")
	queue, err := h.r.Reorder(ctx, d.ID, "operator")
	if err != nil || len(queue) != 3 || queue[0].ID != d.ID || queue[1].ID != a.ID || queue[2].ID != b.ID {
		t.Fatalf("reorder: %+v %v", queue, err)
	}
	withdrawn, err := h.r.Withdraw(ctx, a.ID, "operator")
	if err != nil || withdrawn.State != "withdrawn" {
		t.Fatalf("withdraw: %+v %v", withdrawn, err)
	}
	if _, err := h.r.Withdraw(ctx, a.ID, "operator"); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("double withdraw: %v", err)
	}
	if again, err := h.r.Submit(ctx, c.ID, "u", "req-a", "a"); err != nil || again.ID != a.ID || again.State != "withdrawn" {
		t.Fatalf("retry after withdraw: %+v %v", again, err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity after queue edits: %+v %v", report, err)
	}
	// After the busy chain, one chain answers d then b, in the reordered
	// order.
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" || len(run.Submissions) != 2 || run.Submissions[0] != d.ID || run.Submissions[1] != b.ID {
		t.Fatalf("run: %+v %v", run, err)
	}
	if s, _ := h.r.Submission(ctx, busy.ID); s.State != "answered" {
		t.Fatalf("busy input: %+v", s)
	}
	// The withdrawn input's entry remains in history but was not sent.
	if got := entryTypes(h.entries(t, c.ID)); got != "user user user user assistant steer steer assistant" {
		t.Fatalf("entries: %s", got)
	}
	for _, m := range h.client.requests[0].Messages {
		if strings.Contains(core_textMsg(m), "a") && !strings.Contains(core_textMsg(m), "answer") && core_textMsg(m) == "a" {
			t.Fatal("withdrawn input was sent to the model")
		}
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func core_textMsg(m provider.Message) string {
	var sb strings.Builder
	for _, b := range m.Content {
		if tb, ok := b.(provider.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// Outbox notifications commit with the state they announce, are delivered at
// least once with backoff on failure, and are acknowledged only on success.
func TestOutboxDeliversApprovalsAtLeastOnce(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect"}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}, {text: "done"}}, tool)
	defer h.r.Close()
	h.svc.opts.Approver = requireApproval
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "a", "", "go")
	h.svc.Step(ctx, c.ID)
	pending, _ := h.r.Outbox(ctx)
	if len(pending) != 1 || pending[0].Kind != "approval.pending" || pending[0].ConversationID != c.ID {
		t.Fatalf("outbox: %+v", pending)
	}
	// A failing notifier records the attempt and backs off; nothing is lost.
	attempts := 0
	failing := func(ctx context.Context, n Notification) error { attempts++; return errors.New("webhook down") }
	if n, err := h.r.Deliver(ctx, failing); err != nil || n != 0 {
		t.Fatalf("failing delivery: %d %v", n, err)
	}
	after, _ := h.r.Outbox(ctx)
	if len(after) != 1 || after[0].Attempts != 1 || after[0].LastError == "" || after[0].NextAttempt.IsZero() {
		t.Fatalf("backoff not recorded: %+v", after)
	}
	// Not due yet: a second pass does not call the notifier.
	h.r.Deliver(ctx, failing)
	if attempts != 1 {
		t.Fatalf("retried before backoff: %d", attempts)
	}
	next, _ := h.r.NextDelivery(ctx)
	if next.IsZero() {
		t.Fatal("no retry scheduled")
	}
	// Force the retry due and deliver successfully.
	snap, _ := h.r.Snapshot(ctx)
	n, _, _ := read[Notification](snap, outboxKey(after[0].ID))
	n.NextAttempt = n.NextAttempt.Add(-2 * n.NextAttempt.Sub(n.Created))
	h.r.commit(ctx, snap, "test", record(outboxKey(n.ID), n))
	var seen []string
	ok := func(ctx context.Context, n Notification) error { seen = append(seen, n.ID); return nil }
	if n, err := h.r.Deliver(ctx, ok); err != nil || n != 1 {
		t.Fatalf("delivery: %d %v", n, err)
	}
	if rest, _ := h.r.Outbox(ctx); len(rest) != 0 {
		t.Fatalf("outbox not acknowledged: %+v", rest)
	}
	if acked, _ := h.r.Acknowledge(ctx, seen[0]); acked {
		t.Fatal("acknowledged twice")
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}
