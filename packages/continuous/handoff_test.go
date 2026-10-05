package continuous

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// A handoff closes the current run, resets the context with the note, and
// admits exactly one continuation. The next run starts from the note and the
// continuation only. Re-stepping or replaying the round admits nothing more.
func TestHandoffSchedulesExactlyOneContinuation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "handoff", `{"note":"state so far","continue":"finish the job"}`), call("c2", "effect", `{}`)}},
		{text: "continued"},
	}, HandoffTool{}, &effectTool{name: "effect"})
	defer h.r.Close()
	c := h.root(t)
	first, _ := h.r.Submit(ctx, c.ID, "a", "", "start")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result tool_result reset user assistant" {
		t.Fatalf("entries: %s", got)
	}
	entries := h.entries(t, c.ID)
	if entries[4].Content != "state so far" || entries[5].Content != "finish the job" {
		t.Fatalf("handoff entries: %q %q", entries[4].Content, entries[5].Content)
	}
	// The skipped sibling call received a paired error result.
	if !strings.Contains(entries[3].Content, "skipped") {
		t.Fatalf("sibling result: %q", entries[3].Content)
	}
	// The continuation request saw only the note and the continuation.
	if n := h.client.requestCount(); n != 2 {
		t.Fatalf("requests: %d", n)
	}
	second := h.client.requests[1].Messages
	if len(second) != 2 || second[0].Role != provider.RoleUser || second[1].Role != provider.RoleUser {
		t.Fatalf("continuation context: %+v", second)
	}
	if s, _ := h.r.Submission(ctx, first.ID); s.State != "answered" {
		t.Fatalf("original submission: %+v", s)
	}
	// Exactly one continuation was admitted: the conversation has two
	// submissions, and a second Step does nothing.
	cur, _ := h.r.Conversation(ctx, c.ID)
	if cur.QueueSequence != 2 {
		t.Fatalf("queue sequence: %d", cur.QueueSequence)
	}
	if _, did, err := h.svc.Step(ctx, c.ID); err != nil || did {
		t.Fatalf("idle step: did=%v err=%v", did, err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	// Outside a continuous run the tool refuses.
	var tool HandoffTool
	res, err := tool.Execute(ctx, []byte(`{"note":"n","continue":"c"}`), nil)
	if err != nil || !res.IsError {
		t.Fatalf("handoff outside run: %+v %v", res, err)
	}
	if _, err := tool.Execute(ctx, []byte(`{"note":"n"}`), nil); err == nil {
		t.Fatal("handoff without continuation accepted")
	}
}

// Search scans the full history including entries before the reset, filters
// by type and text, follows fork ancestry on request, and pages by cursor.
func TestSearchHistoryAcrossResetsAndForks(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "alpha answer"}, {text: "beta answer"}, {text: "gamma answer"}})
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "a", "", "alpha question")
	h.svc.Step(ctx, c.ID)
	if _, err := h.r.Reset(ctx, c.ID, "fresh start"); err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, c.ID, "a", "", "beta question")
	h.svc.Step(ctx, c.ID)
	fork, err := h.r.Fork(ctx, c.ID, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, fork.ID, "a", "", "gamma question")
	h.svc.Step(ctx, fork.ID)
	all, err := h.r.Search(ctx, SearchQuery{ConversationID: c.ID})
	if err != nil || len(all.Hits) != 5 || all.More {
		t.Fatalf("all: %d hits %v", len(all.Hits), err)
	}
	before, _ := h.r.Search(ctx, SearchQuery{ConversationID: c.ID, Text: "ALPHA"})
	if len(before.Hits) != 2 || before.Hits[0].Sequence != 1 {
		t.Fatalf("text match before reset: %+v", before.Hits)
	}
	assistants, _ := h.r.Search(ctx, SearchQuery{ConversationID: c.ID, Types: []string{"assistant"}})
	if len(assistants.Hits) != 2 {
		t.Fatalf("type filter: %+v", assistants.Hits)
	}
	// Pagination by cursor.
	page1, _ := h.r.Search(ctx, SearchQuery{ConversationID: c.ID, Limit: 2})
	if len(page1.Hits) != 2 || !page1.More || page1.Next.Sequence != 2 {
		t.Fatalf("page1: %+v", page1)
	}
	page2, _ := h.r.Search(ctx, SearchQuery{ConversationID: c.ID, Limit: 2, Cursor: page1.Next})
	if len(page2.Hits) != 2 || page2.Hits[0].Sequence != 3 || !page2.More {
		t.Fatalf("page2: %+v", page2)
	}
	page3, _ := h.r.Search(ctx, SearchQuery{ConversationID: c.ID, Limit: 2, Cursor: page2.Next})
	if len(page3.Hits) != 1 || page3.More {
		t.Fatalf("page3: %+v", page3)
	}
	// Forks: own entries only by default, ancestry on request.
	own, _ := h.r.Search(ctx, SearchQuery{ConversationID: fork.ID})
	if len(own.Hits) != 2 || own.Hits[0].Owner != fork.ID {
		t.Fatalf("fork own: %+v", own.Hits)
	}
	inherited, _ := h.r.Search(ctx, SearchQuery{ConversationID: fork.ID, Ancestry: true})
	if len(inherited.Hits) != 7 || inherited.Hits[0].Owner != c.ID {
		t.Fatalf("fork ancestry: %d", len(inherited.Hits))
	}
	// Time range: everything is recent; a window in the past matches nothing.
	past, _ := h.r.Search(ctx, SearchQuery{ConversationID: c.ID, Before: time.Now().Add(-time.Hour)})
	if len(past.Hits) != 0 {
		t.Fatalf("past: %+v", past.Hits)
	}
	recent, _ := h.r.Search(ctx, SearchQuery{ConversationID: c.ID, After: time.Now().Add(-time.Hour)})
	if len(recent.Hits) != 5 {
		t.Fatalf("recent: %d", len(recent.Hits))
	}
	if _, err := h.r.Search(ctx, SearchQuery{ConversationID: "missing"}); err == nil {
		t.Fatal("missing conversation searched")
	}
}
