package continuous

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// Retention removes auxiliary records past their age and nothing else:
// history, runs, usage, tasks, pending approvals, live memos, and the
// document values a fork point references all stay.
func TestRetentionSweepsOnlyAuxiliaryRecords(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect"}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "done"},
		{calls: []provider.ToolCallBlock{call("c2", "effect", `{}`)}},
	}, tool)
	defer h.r.Close()
	h.svc.opts.Approver = requireApproval
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "a", "req-1", "go")
	h.svc.Step(ctx, c.ID)
	pending, _ := h.r.PendingApprovals(ctx, c.ID)
	h.r.Decide(ctx, pending[0].ID, "human", true, "", "")
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	run, _, _ := h.r.Run(ctx, c.ID)
	if _, _, err := h.r.Memoize(ctx, "run/"+run.ID, "choice", "test", "x"); err != nil {
		t.Fatal(err)
	}
	// A pending approval on a second, parked run must survive.
	h.r.Submit(ctx, c.ID, "a", "", "again")
	h.svc.Step(ctx, c.ID)
	live, _ := h.r.PendingApprovals(ctx, c.ID)
	if len(live) != 1 {
		t.Fatalf("expected a pending approval, got %d", len(live))
	}
	// Backdate the decided approval, the dedup entry, and the memo by
	// rewriting their timestamps through the store.
	snap, _ := h.r.Snapshot(ctx)
	old := time.Now().Add(-48 * time.Hour)
	a, _, _ := read[Approval](snap, approvalKey(pending[0].ID))
	a.Decided = &old
	m, _, _ := read[Memo](snap, memoKey("run/"+run.ID, "choice"))
	m.Created = old
	e, _, _ := read[Entry](snap, entryKey(c.ID, 1))
	e.Time = old
	if err := h.r.commit(ctx, snap, "test.backdate", record(approvalKey(a.ID), a), record(memoKey(m.Scope, m.Key), m), record(entryKey(c.ID, 1), e)); err != nil {
		t.Fatal(err)
	}
	policy := RetentionPolicy{Dedup: time.Hour, Approvals: time.Hour, Partials: time.Hour, Memos: time.Hour}
	preview, err := h.r.Retain(ctx, policy, true)
	if err != nil || !preview.DryRun || preview.Dedup != 1 || preview.Approvals != 1 || preview.Memos != 1 {
		t.Fatalf("dry run: %+v %v", preview, err)
	}
	if _, ok, _ := h.r.Memo(ctx, "run/"+run.ID, "choice"); !ok {
		t.Fatal("dry run deleted")
	}
	report, err := h.r.Retain(ctx, policy, false)
	if err != nil || report.Dedup != 1 || report.Approvals != 1 || report.Memos != 1 {
		t.Fatalf("sweep: %+v %v", report, err)
	}
	if _, ok, _ := h.r.Memo(ctx, "run/"+run.ID, "choice"); ok {
		t.Fatal("memo survived")
	}
	if _, err := h.r.Approval(ctx, pending[0].ID); err == nil {
		t.Fatal("decided approval survived")
	}
	if still, _ := h.r.PendingApprovals(ctx, c.ID); len(still) != 1 {
		t.Fatalf("pending approval collected: %d", len(still))
	}
	// History and usage untouched; a request-ID retry after expiry is new work.
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result assistant user assistant" {
		// second run is parked on c2's approval: its assistant entry exists, no result yet
		t.Fatalf("entries: %s", got)
	}
	totals, _ := h.r.Usage(ctx, c.ID)
	if totals.Known != 3 {
		t.Fatalf("usage rows: %+v", totals)
	}
	sub, err := h.r.Submit(ctx, c.ID, "a", "req-1", "go")
	if err != nil || sub.State != "queued" {
		t.Fatalf("post-expiry retry: %+v %v", sub, err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	_ = json.Valid
}

// Document history retention keeps the newest values and the value each fork
// point depends on.
func TestRetentionKeepsForkReferencedDocumentVersions(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	docs := DocumentRegistry{}
	docs.Register(DocumentDefinition{Kind: "note", Version: 1, Scope: "conversation", Initial: func() any { return "" }, Fork: "as_of", History: true})
	rev := uint64(0)
	for i := 0; i < 5; i++ {
		queueOnly(t, r, c.ID, "entry")
		d, err := r.WriteDocument(ctx, docs, "note", c.ID, rev, i)
		if err != nil {
			t.Fatal(err)
		}
		rev = d.Revision
		if i == 1 {
			cur, _ := r.Conversation(ctx, c.ID)
			if _, err := r.Fork(ctx, c.ID, cur.EntrySequence, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	report, err := r.Retain(ctx, RetentionPolicy{DocumentHistory: 2}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Five versions, keep two newest plus the one the fork references (value 1).
	if report.Documents != 2 {
		t.Fatalf("collected %d versions", report.Documents)
	}
	forks, _, _ := r.Conversations(ctx, "", 10)
	for _, f := range forks {
		if f.Parent == nil {
			continue
		}
		d, err := r.ReadDocument(ctx, docs, "note", f.ID)
		if err != nil || string(d.Value) != "1" {
			t.Fatalf("fork document after retention: %s %v", d.Value, err)
		}
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}
