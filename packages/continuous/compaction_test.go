package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// summarizingClient answers summary requests (recognisable by their system
// prompt) with a fixed summary and everything else from the script.
type summarizingClient struct {
	*scriptedClient
	summary   string
	summaries int
	fail      bool
}

func (c *summarizingClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	if len(req.Messages) == 1 && strings.Contains(core.MessageText(req.Messages[0]), "<conversation>") {
		c.summaries++
		ch := make(chan provider.Event, 4)
		go func() {
			defer close(ch)
			if c.fail {
				ch <- provider.EventDone{Stop: provider.StopError, Err: errors.New("summary failed")}
				return
			}
			ch <- provider.EventTextDelta{Delta: c.summary}
			ch <- provider.EventUsage{Usage: provider.Usage{InputTokens: 50, OutputTokens: 5}}
			ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: c.summary}}}}
		}()
		return ch, nil
	}
	return c.scriptedClient.Stream(ctx, req)
}

func compactionHarness(t *testing.T, steps []scriptStep) (*harness, *summarizingClient) {
	t.Helper()
	h := newHarness(t, newMemoryStore(), steps)
	sc := &summarizingClient{scriptedClient: h.client, summary: "SUMMARY of earlier turns"}
	engine := EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(sc, "scripted", "You are synthetic.", h.tools)
		a.MaxRetries = 0
		return a, nil
	})
	svc, err := NewService(h.r, engine, ExecutionOptions{RetryDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h, sc
}

func long(n int) string { return strings.Repeat("word ", n) }

func TestManualCompactionKeepsTailAndHistory(t *testing.T) {
	ctx := context.Background()
	h, sc := compactionHarness(t, []scriptStep{{text: long(200)}, {text: long(200)}, {text: "after compaction"}})
	defer h.r.Close()
	c := h.root(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("run: %+v %v", run, err)
		}
	}
	// Entries 1..4: user assistant user assistant, about 250 tokens each.
	// Keeping about 300 tokens leaves entry 4 verbatim and summarizes 1..3.
	engine := h.svc.currentEngine()
	if _, err := h.r.Compact(ctx, engine, c.ID, "", 100000); err == nil || !strings.Contains(err.Error(), "nothing to compact") {
		t.Fatalf("keep everything: %v", err)
	}
	after, err := h.r.Compact(ctx, engine, c.ID, "focus on numbers", 300)
	if err != nil || after.EntrySequence != 5 || sc.summaries != 1 {
		t.Fatalf("compact: %+v %v summaries=%d", after, err, sc.summaries)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant user assistant compaction" {
		t.Fatalf("history not retained: %s", got)
	}
	snap, _ := h.r.Snapshot(ctx)
	msgs, err := ModelContext(ctx, snap, c.ID, after.EntrySequence)
	if err != nil || len(msgs) != 2 || !strings.Contains(core.MessageText(msgs[0]), "SUMMARY") || msgs[1].Role != provider.RoleAssistant {
		t.Fatalf("context after compaction: %d %v", len(msgs), err)
	}
	// The next run sees the summary plus tail plus the new input.
	h.r.Submit(ctx, c.ID, "actor", "", "next")
	if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("run after compaction: %+v %v", run, err)
	}
	req := h.client.requests[len(h.client.requests)-1]
	if len(req.Messages) != 3 || !strings.Contains(core.MessageText(req.Messages[0]), "SUMMARY") {
		t.Fatalf("request after compaction: %d", len(req.Messages))
	}
	totals, _ := h.r.Usage(ctx, c.ID)
	if totals.Known != 4 {
		t.Fatalf("summary usage not charged: %+v", totals)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	// Legacy export: a compaction row carrying summary plus tail; the legacy
	// view ends with summary, tail, and the post-compaction turn.
	var exported strings.Builder
	if err := h.r.ExportSession(ctx, c.ID, &exported); err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyMessages(exported.String())
	if err != nil || len(legacy) != 4 || !strings.Contains(core.MessageText(legacy[0]), "SUMMARY") || core.MessageText(legacy[3]) != "after compaction" {
		t.Fatalf("legacy view: %d %v", len(legacy), err)
	}
}

func TestCompactionRejectedWhileBusyAndStale(t *testing.T) {
	ctx := context.Background()
	h, _ := compactionHarness(t, []scriptStep{{text: long(200)}, {text: long(200)}})
	defer h.r.Close()
	c := h.root(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.svc.Step(ctx, c.ID)
	}
	h.r.Submit(ctx, c.ID, "actor", "", "busy")
	if _, _, err := h.svc.start(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Compact(ctx, h.svc.currentEngine(), c.ID, "", 300); !errors.Is(err, ErrBusy) {
		t.Fatalf("compact while busy: %v", err)
	}
	if _, err := h.r.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.Step(ctx, c.ID)
	// Stale: a reset lands while summarizing. Use an engine that resets the
	// conversation during the summary request.
	staleEngine := EngineFunc(func(ctx context.Context, cv Conversation) (*core.Agent, error) {
		if _, err := h.r.Reset(ctx, cv.ID, "boundary moved"); err != nil {
			return nil, err
		}
		return h.svc.currentEngine().Build(ctx, cv)
	})
	if _, err := h.r.Compact(ctx, staleEngine, c.ID, "", 300); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale compaction published: %v", err)
	}
	for _, e := range h.entries(t, c.ID) {
		if e.Type == entryCompaction {
			t.Fatal("stale summary was committed")
		}
	}
}

func TestThresholdCompactionInsideRun(t *testing.T) {
	ctx := context.Background()
	h, sc := compactionHarness(t, []scriptStep{{text: long(200)}, {text: long(200)}, {text: "small"}})
	defer h.r.Close()
	h.svc.opts.Compaction = CompactionPolicy{ContextWindow: 400, ReserveTokens: 50, KeepRecentTokens: 150}
	c := h.root(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("run: %+v %v", run, err)
		}
	}
	// By the third input the context (~800 tokens) exceeds 350: the run
	// compacts before sending, then sends with the summary.
	h.r.Submit(ctx, c.ID, "actor", "", "third")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("third run: %+v %v", run, err)
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "threshold compaction") {
		t.Fatalf("notices: %v", run.Notices)
	}
	last := h.client.requests[len(h.client.requests)-1]
	if !strings.Contains(core.MessageText(last.Messages[0]), "SUMMARY") || estimateTokens(last.Messages) > 400 {
		t.Fatalf("request not compacted: %d messages, %d tokens", len(last.Messages), estimateTokens(last.Messages))
	}
	if got := entryTypes(h.entries(t, c.ID)); !strings.Contains(got, "compaction user assistant") && !strings.Contains(got, "user compaction assistant") {
		t.Fatalf("entries: %s", got)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	_ = sc
}

func TestOverflowCompactsOnceThenFails(t *testing.T) {
	ctx := context.Background()
	h, sc := compactionHarness(t, []scriptStep{
		{text: long(200)}, {text: long(200)},
		{err: errors.New("maximum context length exceeded")},
		{text: "fits now"},
	})
	defer h.r.Close()
	h.svc.opts.Compaction = CompactionPolicy{ContextWindow: 100000, KeepRecentTokens: 150}
	c := h.root(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.svc.Step(ctx, c.ID)
	}
	h.r.Submit(ctx, c.ID, "actor", "", "overflow me")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" || sc.summaries != 1 || run.Attempt != 1 {
		t.Fatalf("overflow recovery: %+v %v summaries=%d", run, err, sc.summaries)
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "rejected the context size") {
		t.Fatalf("notices: %v", run.Notices)
	}
	if got := entryTypes(h.entries(t, c.ID)); !strings.Contains(got, "attempt compaction assistant") {
		t.Fatalf("entries: %s", got)
	}
	// Build up context again so there is something to compact, then make
	// the provider reject twice: one compaction, then an honest failure.
	h.client.steps = []scriptStep{{text: long(200)}, {text: long(200)}}
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.svc.Step(ctx, c.ID)
	}
	h.client.steps = []scriptStep{{err: errors.New("maximum context length exceeded")}, {err: errors.New("maximum context length exceeded")}}
	h.r.Submit(ctx, c.ID, "actor", "", "again")
	run, _, err = h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "failed" || sc.summaries != 2 || !strings.Contains(run.Error, "context length") {
		t.Fatalf("second overflow: %+v %v summaries=%d", run, err, sc.summaries)
	}
	// 2 setup + overflow + retry + 2 setup + overflow + retry (rejected, and
	// not compacted again in the same turn) = 8 requests.
	if h.client.requestCount() != 8 {
		t.Fatalf("requests: %d", h.client.requestCount())
	}
	// A failed summary during overflow ends the run honestly.
	h.client.steps = []scriptStep{{text: long(200)}, {text: long(200)}}
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.svc.Step(ctx, c.ID)
	}
	sc.fail = true
	h.client.steps = []scriptStep{{err: errors.New("maximum context length exceeded")}}
	h.r.Submit(ctx, c.ID, "actor", "", "once more")
	run, _, err = h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "failed" || !strings.Contains(run.Error, "compaction after overflow failed") {
		t.Fatalf("failed summary: %+v %v", run, err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestSelectCutNeverSplitsToolRound(t *testing.T) {
	ctx := context.Background()
	h, _ := compactionHarness(t, []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: long(100)},
	})
	defer h.r.Close()
	h.tools["effect"] = &effectTool{name: "effect"}
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", long(100))
	if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	// Entries: user(1) assistant+call(2) tool_result(3) assistant(4).
	// Keeping ~5 tokens wants a cut at entry 4 (safe, plain assistant);
	// keeping ~120 tokens would land inside the tool round and must move
	// back to entry 1 or forward to 4, never to 2 or 3.
	snap, _ := h.r.Snapshot(ctx)
	for _, keep := range []int{5, 120} {
		head, err := selectCut(ctx, snap, c.ID, 4, keep)
		if err != nil {
			t.Fatal(err)
		}
		if head == 2 || head == 3 {
			t.Fatalf("keep %d: cut inside tool round at %d", keep, head)
		}
	}
}

// Background compaction starts above its threshold without blocking the
// request, and the summary is published at the next request boundary. A
// reset between summarization and publication makes the summary stale: it
// is discarded and counted, never published over the newer context.
func TestBackgroundCompactionPublishesAtNextBoundary(t *testing.T) {
	ctx := context.Background()
	h, sc := compactionHarness(t, []scriptStep{{text: long(200)}, {text: long(200)}, {text: "third"}, {text: "fourth"}})
	defer h.r.Close()
	// Blocking threshold far away, background threshold low: the summary
	// runs while the run continues.
	h.svc.opts.Compaction = CompactionPolicy{ContextWindow: 100000, ReserveTokens: 1000, KeepRecentTokens: 300, BackgroundTokens: 300}
	c := h.root(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("run: %+v %v", run, err)
		}
	}
	// The second request exceeded 300 tokens: a background summary started
	// and the request was not blocked (no compaction entry yet).
	h.svc.WaitCompactions(ctx)
	if m := h.svc.CompactionMetrics(); m.Started != 1 || m.Finished != 1 || m.Pending != 1 || sc.summaries != 1 {
		t.Fatalf("metrics after background summary: %+v summaries=%d", m, sc.summaries)
	}
	if got := entryTypes(h.entries(t, c.ID)); strings.Contains(got, "compaction") {
		t.Fatalf("summary published before a boundary: %s", got)
	}
	// Next request boundary publishes it; the request carries the summary.
	h.r.Submit(ctx, c.ID, "actor", "", "third")
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("third run: %+v %v", run, err)
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "background compaction published") {
		t.Fatalf("notices: %v", run.Notices)
	}
	last := h.client.requests[len(h.client.requests)-1]
	if !strings.Contains(core.MessageText(last.Messages[0]), "SUMMARY") {
		t.Fatalf("request without summary: %d messages", len(last.Messages))
	}
	if got := entryTypes(h.entries(t, c.ID)); !strings.Contains(got, "compaction") {
		t.Fatalf("entries: %s", got)
	}
	var info CompactionInfo
	for _, e := range h.entries(t, c.ID) {
		if e.Type == entryCompaction {
			if err := json.Unmarshal(e.Data, &info); err != nil {
				t.Fatal(err)
			}
		}
	}
	if info.Reason != "background" || info.Head == 0 {
		t.Fatalf("compaction info: %+v", info)
	}
	if m := h.svc.CompactionMetrics(); m.Pending != 0 || m.Stale != 0 {
		t.Fatalf("metrics after publication: %+v", m)
	}

	// Stale path: fill the context again so another background summary
	// starts, then reset before the next boundary.
	h.client.steps = []scriptStep{{text: long(200)}, {text: long(200)}, {text: "after reset"}}
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("run: %+v %v", run, err)
		}
	}
	h.svc.WaitCompactions(ctx)
	if m := h.svc.CompactionMetrics(); m.Pending != 1 {
		t.Fatalf("second background summary not pending: %+v", m)
	}
	if _, err := h.r.Reset(ctx, c.ID, "handoff"); err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, c.ID, "actor", "", "after")
	run, _, err = h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run after reset: %+v %v", run, err)
	}
	if m := h.svc.CompactionMetrics(); m.Stale != 1 || m.Pending != 0 {
		t.Fatalf("stale summary not discarded: %+v", m)
	}
	compactions := 0
	for _, e := range h.entries(t, c.ID) {
		if e.Type == entryCompaction {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("stale summary was published: %d compaction entries", compactions)
	}
	last = h.client.requests[len(h.client.requests)-1]
	if !strings.Contains(core.MessageText(last.Messages[0]), "handoff") || len(last.Messages) != 2 {
		t.Fatalf("context after reset: %d messages, first %q", len(last.Messages), core.MessageText(last.Messages[0]))
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}
