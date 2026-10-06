package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// withOptions rebuilds the harness scheduler with execution options.
func (h *nativeHarness) withOptions(t *testing.T, opts ExecutionOptions) {
	t.Helper()
	if opts.RetryDelay == 0 {
		opts.RetryDelay = time.Millisecond
	}
	h.svc.opts = opts.normalized()
	x, err := NewNativeExecutor(h.r, h.svc.currentEngine, h.svc.opts)
	if err != nil {
		t.Fatal(err)
	}
	reg := TaskRegistry{}
	if err := x.Register(reg); err != nil {
		t.Fatal(err)
	}
	h.native, h.reg, h.sched = x, reg, x.Scheduler(reg)
}

func TestNativeApprovalParksWithoutWorkerAndBindsArgs(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect"}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{"path":"/tmp/x"}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{Approver: func(ctx context.Context, c Conversation, call provider.ToolCallBlock, args json.RawMessage) (ApprovalRequest, bool) {
		return ApprovalRequest{Summary: "write a file"}, true
	}})
	c := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "go")
	h.run(t) // returns: the only live task waits on a human
	if tool.calls.Load() != 0 {
		t.Fatal("executed without approval")
	}
	if h.sched.InFlight() != 0 {
		t.Fatal("approval wait holds a worker")
	}
	pending, err := h.r.PendingApprovals(ctx, c.ID)
	if err != nil || len(pending) != 1 || string(pending[0].Args) != `{"path":"/tmp/x"}` {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	snap, _ := h.r.ConversationSnapshot(ctx, c.ID, 10)
	if snap.Recovery == nil || snap.Recovery.Action != "approve" {
		t.Fatalf("recovery view: %+v", snap.Recovery)
	}
	checkValid(t, h.r)
	// The legacy-style Step adapter reports the parked state honestly.
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("step adapter: %v", err)
	}
	if _, err := h.r.Decide(ctx, pending[0].ID, "human", true, "call", "ok"); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	if tool.calls.Load() != 1 {
		t.Fatalf("approved call executions: %d", tool.calls.Load())
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("submission: %+v", s)
	}
	checkValid(t, h.r)
}

func TestNativeApprovalDenyAndAbortExpires(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect"}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "understood"},
		{calls: []provider.ToolCallBlock{call("c2", "effect", `{}`)}},
	}, tool)
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{Approver: func(context.Context, Conversation, provider.ToolCallBlock, json.RawMessage) (ApprovalRequest, bool) {
		return ApprovalRequest{}, true
	}})
	c := h.nativeRoot(t)
	h.r.Submit(ctx, c.ID, "actor", "", "one")
	h.run(t)
	pending, _ := h.r.PendingApprovals(ctx, c.ID)
	h.r.Decide(ctx, pending[0].ID, "human", false, "call", "no")
	h.run(t)
	entries := h.entries(t, c.ID)
	if tool.calls.Load() != 0 || !strings.Contains(entries[2].Content, "denied: no") {
		t.Fatalf("deny: calls=%d %q", tool.calls.Load(), entries[2].Content)
	}
	// Abort while parked on a second approval expires it.
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "two")
	h.run(t)
	pending, _ = h.r.PendingApprovals(ctx, c.ID)
	if len(pending) != 1 {
		t.Fatalf("pending: %+v", pending)
	}
	if _, err := h.r.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "aborted" {
		t.Fatalf("submission: %+v", s)
	}
	if a, _ := h.r.Approval(ctx, pending[0].ID); a.State != approvalExpired {
		t.Fatalf("approval after abort: %+v", a)
	}
	if _, err := h.r.Decide(ctx, pending[0].ID, "human", true, "call", ""); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("late decision: %v", err)
	}
	checkValid(t, h.r)
}

// nativeCompaction builds a task-native harness whose client answers
// summary requests.
func nativeCompaction(t *testing.T, steps []scriptStep) (*nativeHarness, *summarizingClient) {
	t.Helper()
	h := newNativeHarness(t, newMemoryStore(), steps)
	sc := &summarizingClient{scriptedClient: h.client, summary: "SUMMARY of earlier turns"}
	gen := &engineGeneration{Engine: EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(sc, "scripted", "You are synthetic.", h.tools)
		a.MaxRetries = 0
		return a, nil
	}), Generation: 1}
	h.svc.engine.Store(gen)
	return h, sc
}

func TestNativeThresholdAndOverflowCompaction(t *testing.T) {
	ctx := context.Background()
	h, sc := nativeCompaction(t, []scriptStep{
		{text: long(200)}, {text: long(200)},
		{err: errors.New("maximum context length exceeded")},
		{text: "fits now"},
	})
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{Compaction: CompactionPolicy{ContextWindow: 100000, KeepRecentTokens: 150}})
	c := h.nativeRoot(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.run(t)
	}
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "overflow me")
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" || sc.summaries != 1 {
		t.Fatalf("overflow recovery: %+v summaries=%d", s, sc.summaries)
	}
	if got := entryTypes(h.entries(t, c.ID)); !strings.Contains(got, "attempt compaction assistant") {
		t.Fatalf("entries: %s", got)
	}
	// The compaction ran as its own task with the summary as its result.
	tasks, _ := h.r.Tasks(ctx, c.ID)
	compactions := 0
	for _, task := range tasks {
		if task.Kind == TaskKindCompaction {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("compaction tasks: %d", compactions)
	}
	// A second overflow in the same turn fails honestly.
	h.client.steps = []scriptStep{{text: long(200)}, {text: long(200)}}
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.run(t)
	}
	h.client.steps = []scriptStep{{err: errors.New("maximum context length exceeded")}, {err: errors.New("maximum context length exceeded")}}
	sub, _ = h.r.Submit(ctx, c.ID, "actor", "", "again")
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "failed" || sc.summaries != 2 {
		t.Fatalf("second overflow: %+v summaries=%d", s, sc.summaries)
	}
	checkValid(t, h.r)

}

func TestNativeThresholdCompaction(t *testing.T) {
	ctx := context.Background()
	h, sc := nativeCompaction(t, []scriptStep{{text: long(200)}, {text: long(200)}, {text: "small"}})
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{Compaction: CompactionPolicy{ContextWindow: 400, ReserveTokens: 50, KeepRecentTokens: 150}})
	c := h.nativeRoot(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.run(t)
	}
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "third")
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" || sc.summaries != 1 {
		t.Fatalf("threshold: %+v summaries=%d", s, sc.summaries)
	}
	last := h.client.requests[len(h.client.requests)-1]
	if !strings.Contains(core.MessageText(last.Messages[0]), "SUMMARY") || estimateTokens(last.Messages) > 400 {
		t.Fatalf("request not compacted: %d tokens", estimateTokens(last.Messages))
	}
	checkValid(t, h.r)
}

func TestNativeBackgroundCompactionPublishesAtBoundary(t *testing.T) {
	ctx := context.Background()
	h, sc := nativeCompaction(t, []scriptStep{{text: long(200)}, {text: long(200)}, {text: "third"}})
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{Compaction: CompactionPolicy{ContextWindow: 100000, ReserveTokens: 1000, KeepRecentTokens: 300, BackgroundTokens: 300}})
	c := h.nativeRoot(t)
	for _, q := range []string{long(200), long(200)} {
		h.r.Submit(ctx, c.ID, "actor", "", q)
		h.run(t)
	}
	// The second request exceeded the background threshold: a durable
	// summary task ran, but nothing was published without a boundary.
	if sc.summaries != 1 {
		t.Fatalf("summaries: %d", sc.summaries)
	}
	if got := entryTypes(h.entries(t, c.ID)); strings.Contains(got, "compaction") {
		t.Fatalf("published without a boundary: %s", got)
	}
	if _, ok, _ := read[string](mustSnap(t, h.r), backgroundKey(c.ID)); !ok {
		t.Fatal("finished summary not retained durably")
	}
	checkValid(t, h.r)
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "third")
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("submission: %+v", s)
	}
	last := h.client.requests[len(h.client.requests)-1]
	if !strings.Contains(core.MessageText(last.Messages[0]), "SUMMARY") {
		t.Fatalf("request without summary: %d messages", len(last.Messages))
	}
	if chainNotices := entryTypes(h.entries(t, c.ID)); !strings.Contains(chainNotices, "user compaction assistant") {
		t.Fatalf("entries: %s", chainNotices)
	}
	checkValid(t, h.r)
}

func mustSnap(t *testing.T, r *Runtime) storage.Snapshot {
	t.Helper()
	snap, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestNativeHandoffAdmitsOneContinuation(t *testing.T) {
	ctx := context.Background()
	other := &effectTool{name: "effect"}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "handoff", `{"note":"state so far","continue":"finish the job"}`), call("c2", "effect", `{}`)}},
		{text: "continued"},
	}, HandoffTool{}, other)
	defer h.r.Close()
	c := h.nativeRoot(t)
	first, _ := h.r.Submit(ctx, c.ID, "a", "", "start")
	h.run(t)
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result tool_result reset user assistant" {
		t.Fatalf("entries: %s", got)
	}
	if other.calls.Load() != 0 {
		t.Fatal("sibling after handoff executed")
	}
	second := h.client.requests[1].Messages
	if len(second) != 2 || core.MessageText(second[0]) != "state so far" {
		t.Fatalf("continuation context: %+v", second)
	}
	if s, _ := h.r.Submission(ctx, first.ID); s.State != "answered" {
		t.Fatalf("original: %+v", s)
	}
	if cur, _ := h.r.Conversation(ctx, c.ID); cur.QueueSequence != 2 {
		t.Fatalf("continuations: %d", cur.QueueSequence)
	}
	checkValid(t, h.r)
}

func TestNativeSubagentWaitsWithoutWorker(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("s1", "subagent", `{"task":"count the files"}`)}},
		{text: "there are 3 files"},
		{text: "parent done: 3 files"},
	})
	h.tools["subagent"] = &SubagentTool{Runtime: h.r, Service: h.svc}
	defer h.r.Close()
	h.sched.Workers = 1
	parent := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, parent.ID, "actor", "", "how many files")
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("parent: %+v", s)
	}
	chainRunID := ""
	if tasks, _ := h.r.Tasks(ctx, parent.ID); len(tasks) > 0 {
		var in generationInput
		json.Unmarshal(tasks[0].Input, &in)
		chainRunID = in.RunID
	}
	children, err := h.r.OwnedConversations(ctx, ToolCallIdentity{RunID: chainRunID, ConversationID: parent.ID, CallID: "s1"}.OwnerID())
	if err != nil || len(children) != 1 {
		t.Fatalf("owned: %v %v", children, err)
	}
	result := h.client.requests[2].Messages[2].Content[0].(provider.ToolResultBlock)
	if result.IsError || core.MessageText(provider.Message{Content: result.Content}) != "there are 3 files" {
		t.Fatalf("tool result: %+v", result)
	}
	checkValid(t, h.r)
}

func TestNativeSubagentAbortCascades(t *testing.T) {
	ctx := context.Background()
	block := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("s1", "subagent", `{"task":"work"}`)}},
		{calls: []provider.ToolCallBlock{call("e1", "effect", `{}`)}},
	}, block)
	h.tools["subagent"] = &SubagentTool{Runtime: h.r, Service: h.svc}
	defer h.r.Close()
	parent := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, parent.ID, "actor", "", "go")
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.sched.Run(runCtx) }()
	<-block.started
	if _, err := h.r.Abort(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	close(block.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "aborted" {
		t.Fatalf("parent: %+v", s)
	}
	snap, _ := h.r.Snapshot(ctx)
	rows, _ := snap.Page("chain/", "", 100)
	if len(rows) != 0 {
		t.Fatalf("chains left active: %d", len(rows))
	}
	checkValid(t, h.r)
}

func TestNativePartialIsFencedAndReplaced(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "streamed answer"}})
	defer h.r.Close()
	h.withOptions(t, ExecutionOptions{PartialFlushInterval: time.Millisecond})
	c := h.nativeRoot(t)
	seen := make(chan Partial, 1)
	h.client.onStream = func(provider.Request) {}
	go func() {
		_ = h.r.Watch(ctx, c.ID, 0, func(cm storage.Commit) error {
			if p, ok := PartialFromCommit(cm, c.ID); ok {
				select {
				case seen <- p:
				default:
				}
			}
			return nil
		})
	}()
	h.r.Submit(ctx, c.ID, "actor", "", "hi")
	h.run(t)
	if _, ok, _ := h.r.Partial(ctx, c.ID); ok {
		t.Fatal("live partial outlived the response commit")
	}
	checkValid(t, h.r)
	// A stale invocation cannot write a partial.
	p := &taskPartial{tc: TaskContext{inv: &taskInvocation{s: h.sched, task: Task{ID: "missing", Invocation: "x"}}}, base: Partial{ConversationID: c.ID, RunID: "r", Turn: 1, Attempt: 1}, dirty: true}
	p.buf.WriteString("stale")
	p.flush()
	if _, ok, _ := h.r.Partial(ctx, c.ID); ok {
		t.Fatal("stale partial committed")
	}
	// Truncation is bounded.
	big := &taskPartial{}
	big.add(strings.Repeat("x", MaxPartialBytes+10))
	if text, trunc := big.text(); len(text) != MaxPartialBytes || !trunc {
		t.Fatalf("truncation: %d %v", len(text), trunc)
	}
}

// legacyInterrupted leaves a run of the earlier executor in its tools phase
// with one done, one running, and one pending intent, like a crash inside
// the second tool.
func legacyInterrupted(t *testing.T) (*nativeHarness, Conversation, *effectTool, *effectTool) {
	t.Helper()
	done := &effectTool{name: "done"}
	stuck := &effectTool{name: "stuck"}
	h := newNativeHarness(t, newMemoryStore(), nil, done, stuck)
	c := h.root(t)
	queueOnly(t, h.r, c.ID, "hello")
	writeLegacyRun(t, h.r, c.ID, "tools",
		legacyIntent{call: call("d1", "done", `{}`), state: "done"},
		legacyIntent{call: call("s1", "stuck", `{"a":1}`), state: "running"},
		legacyIntent{call: call("p1", "done", `{}`), state: "pending"},
	)
	return h, c, done, stuck
}

func TestMigratesInterruptedLegacyRun(t *testing.T) {
	ctx := context.Background()
	h, c, done, stuck := legacyInterrupted(t)
	defer h.r.Close()
	legacy, _, _ := read[Run](mustSnap(t, h.r), runKey(c.ID))
	migrated, blocked, err := h.r.MigrateLegacy(ctx)
	if err != nil || migrated != 1 || len(blocked) != 0 {
		t.Fatalf("migrate: %d %v %v", migrated, blocked, err)
	}
	// Idempotent: a second pass commits nothing.
	before := mustSnap(t, h.r).Revision()
	if n, _, err := h.r.MigrateLegacy(ctx); err != nil || n != 0 || mustSnap(t, h.r).Revision() != before {
		t.Fatalf("repeated migration committed: %d %v", n, err)
	}
	chain, ok, _ := h.r.Chain(ctx, c.ID)
	if !ok || chain.RunID != legacy.ID || len(chain.Submissions) != 1 {
		t.Fatalf("chain keeps the run identity: %+v", chain)
	}
	if f, _ := h.r.Format(ctx); f.Version != 2 {
		t.Fatalf("format after migration: %+v", f)
	}
	plan, _ := h.r.RecoveryPreview(ctx)
	if len(plan.Actions) != 1 || plan.Actions[0].Action != "report" || plan.Actions[0].Executor != "tasks" {
		t.Fatalf("preview: %+v", plan)
	}
	checkValid(t, h.r)
	h.client.steps = []scriptStep{{text: "recovered"}}
	h.run(t)
	if stuck.calls.Load() != 0 || done.calls.Load() != 1 {
		t.Fatalf("effects: stuck=%d done=%d (the interrupted call must not repeat, the pending one runs once)", stuck.calls.Load(), done.calls.Load())
	}
	results := h.client.requests[0].Messages[2].Content
	if len(results) != 3 || !results[1].(provider.ToolResultBlock).IsError {
		t.Fatalf("results: %+v", results)
	}
	tasks, _ := h.r.Tasks(ctx, c.ID)
	for _, task := range tasks {
		if task.Kind == TaskKindTool {
			if in, _, _ := decodeTool(task); in.RunID != legacy.ID {
				t.Fatalf("operation identity changed: %+v", in)
			}
		}
	}
	if s, _ := h.r.Submission(ctx, chain.Submissions[0]); s.State != "answered" {
		t.Fatalf("submission: %+v", s)
	}
	checkValid(t, h.r)
}

func TestMigrationBlockedRunHoldsUntilAborted(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "after abort"}}, &effectTool{name: "stuck"})
	defer h.r.Close()
	c := h.root(t)
	queueOnly(t, h.r, c.ID, "hello")
	run := writeLegacyRun(t, h.r, c.ID, "tools", legacyIntent{call: call("s1", "stuck", `{}`), state: "running"})
	// A running intent without committed arguments cannot be converted
	// without guessing what the call was.
	snap := mustSnap(t, h.r)
	run.Tools[0].Args = nil
	if err := h.r.commit(ctx, snap, "test", record(runKey(c.ID), run)); err != nil {
		t.Fatal(err)
	}
	_, blocked, err := h.r.MigrateLegacy(ctx)
	if err != nil || !errors.Is(blocked[c.ID], ErrMigrationBlocked) {
		t.Fatalf("migration: %v %v", blocked, err)
	}
	notices, _ := h.r.Outbox(ctx)
	if len(notices) != 1 || notices[0].Kind != "migration.blocked" {
		t.Fatalf("outbox: %+v", notices)
	}
	// New input queues behind the blocked run; nothing executes.
	next, _ := h.r.Submit(ctx, c.ID, "actor", "", "next")
	if next.State != "queued" {
		t.Fatalf("admission while blocked: %+v", next)
	}
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("step on blocked conversation: %v", err)
	}
	h.run(t)
	if h.client.requestCount() != 0 {
		t.Fatal("blocked conversation executed")
	}
	// Abort settles the run with paired results; the queue then proceeds.
	settled, err := h.r.Abort(ctx, c.ID)
	if err != nil || settled.Outcome != "aborted" {
		t.Fatalf("abort: %+v %v", settled, err)
	}
	h.run(t)
	if s, _ := h.r.Submission(ctx, next.ID); s.State != "answered" {
		t.Fatalf("queued input after abort: %+v", s)
	}
	snapAfter := mustSnap(t, h.r)
	messages, _ := ModelContext(ctx, snapAfter, c.ID, 0)
	if msgs := provider.RepairOrphanedToolResults(messages); len(msgs) != len(messages) {
		t.Fatal("abort left a dangling call")
	}
	checkValid(t, h.r)
}

// Migration runs at commit granularity per conversation: a crash between
// two conversations leaves one converted and one untouched, and the next
// pass finishes the second without touching the first again.
func TestMigrationInterruptedAndRepeated(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), nil)
	defer h.r.Close()
	var ids []string
	for _, w := range []string{"a", "b", "c"} {
		c, _ := h.r.OpenRoot(ctx, w, AgentConfig{})
		queueOnly(t, h.r, c.ID, "hello")
		writeLegacyRun(t, h.r, c.ID, "request")
		ids = append(ids, c.ID)
	}
	real := h.r.store
	crashed := errors.New("synthetic crash")
	h.r.store = &crashStore{Store: real, at: 2, err: crashed}
	if _, _, err := h.r.MigrateLegacy(ctx); !errors.Is(err, crashed) {
		t.Fatalf("crash: %v", err)
	}
	h.r.store = real
	converted := 0
	for _, id := range ids {
		if _, ok, _ := h.r.Chain(ctx, id); ok {
			converted++
		}
	}
	if converted != 2 {
		t.Fatalf("converted before the crash point: %d", converted)
	}
	checkValid(t, h.r)
	n, _, err := h.r.MigrateLegacy(ctx)
	if err != nil || n != 1 {
		t.Fatalf("resume: %d %v", n, err)
	}
	for _, id := range ids {
		if _, ok, _ := h.r.Chain(ctx, id); !ok {
			t.Fatalf("conversation %s not migrated", id)
		}
	}
	checkValid(t, h.r)
}

// Crash at every commit of a task-native round, on both durable backends:
// recovery never repeats an unsafe effect, never duplicates results or
// settlements, and keeps tool calls paired.
func TestNativeCrashAtEveryCommit(t *testing.T) {
	for _, backend := range []string{"journal", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			for crashAt := 1; ; crashAt++ {
				path := t.TempDir() + "/store"
				store, err := openCrashStore(ctx, backend, path)
				if err != nil {
					t.Fatal(err)
				}
				tool := &effectTool{name: "effect"}
				h := newNativeHarness(t, store, []scriptStep{
					{calls: []provider.ToolCallBlock{call("c1", "effect", `{"n":1}`), call("c2", "effect", `{"n":2}`)}},
					{text: "done"},
				}, tool)
				c := h.nativeRoot(t)
				h.r.Submit(ctx, c.ID, "actor", "req", "hello")
				crashed := errors.New("synthetic crash")
				h.r.store = &crashStore{Store: store, at: crashAt, err: crashed}
				runErr := h.sched.Run(ctx)
				h.sched.Join(ctx)
				storeCrashed := runErr != nil && errors.Is(runErr, crashed)
				cs := h.r.store.(*crashStore)
				cs.mu.Lock()
				if cs.dead {
					storeCrashed = true
				}
				cs.mu.Unlock()
				h.r.store = store
				h.r.Close()
				if !storeCrashed {
					if crashAt == 1 {
						t.Fatal("matrix never crashed")
					}
					return
				}
				store, err = openCrashStore(ctx, backend, path)
				if err != nil {
					t.Fatalf("crash %d: reopen %v", crashAt, err)
				}
				recovered := &effectTool{name: "effect"}
				h2 := newNativeHarness(t, store, []scriptStep{{text: "done"}, {text: "done"}, {text: "done"}}, recovered)
				if report, err := h2.r.CheckIntegrity(ctx); err != nil || !report.Valid {
					t.Fatalf("crash %d: integrity before recovery %+v %v", crashAt, report, err)
				}
				h2.run(t)
				if total := tool.calls.Load() + recovered.calls.Load(); total > 2 {
					t.Fatalf("crash %d: tool executed %d times", crashAt, total)
				}
				for _, req := range h2.client.requests {
					if msgs := provider.RepairOrphanedToolResults(req.Messages); len(msgs) != len(req.Messages) {
						t.Fatalf("crash %d: dangling pairing sent", crashAt)
					}
				}
				sub, err := h2.r.Submit(ctx, c.ID, "actor", "req", "hello")
				if err != nil || sub.State != "answered" {
					t.Fatalf("crash %d: submission %+v %v", crashAt, sub, err)
				}
				// Exactly one result per committed call: no duplicates, none
				// missing, whatever the crash point.
				calls, results := 0, 0
				for _, e := range h2.entries(t, c.ID) {
					switch e.Type {
					case entryToolResult:
						results++
					case entryAssistant:
						msg, _ := core.DecodeMessage(e.Message)
						for _, b := range msg.Content {
							if _, ok := b.(provider.ToolCallBlock); ok {
								calls++
							}
						}
					}
				}
				if results != calls {
					t.Fatalf("crash %d: %d calls, %d results", crashAt, calls, results)
				}
				if report, err := h2.r.CheckIntegrity(ctx); err != nil || !report.Valid || report.ActiveRuns != 0 {
					t.Fatalf("crash %d: integrity after %+v %v", crashAt, report, err)
				}
				h2.r.Close()
			}
		})
	}
}

func TestHostMigratesAndHoldsInterruptedRun(t *testing.T) {
	ctx := context.Background()
	h, c, _, stuck := legacyInterrupted(t)
	defer h.r.Close()
	h.client.steps = []scriptStep{{text: "recovered"}}
	host, err := NewHost(h.r, h.svc.currentEngine(), HostOptions{Execution: ExecutionOptions{RetryDelay: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- host.Run(runCtx) }()
	// The safe policy holds the interrupted unsafe call for a human.
	deadline := time.Now().Add(5 * time.Second)
	for len(host.Blocked()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("interrupted chain not held")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok, _ := h.r.Chain(ctx, c.ID); !ok {
		t.Fatal("not migrated")
	}
	if h.client.requestCount() != 0 {
		t.Fatal("held chain progressed")
	}
	host.Unblock(host.Blocked()[0])
	chain, _, _ := h.r.Chain(ctx, c.ID)
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	if s, err := h.r.WaitSubmission(waitCtx, chain.Submissions[0]); err != nil || s.State != "answered" {
		t.Fatalf("after unblock: %+v %v", s, err)
	}
	if stuck.calls.Load() != 0 {
		t.Fatal("unsafe call repeated")
	}
	cancel()
	<-done
	checkValid(t, h.r)
}

// A lost remote outcome on the task path keeps the effect intent and
// reconciles after reconnect: the effect is not repeated and its result is
// recovered.
func TestNativeRemoteWorkerUnknownOutcomeReconciles(t *testing.T) {
	ctx := context.Background()
	effect := &remoteEffect{block: make(chan struct{}), started: make(chan struct{}, 1)}
	_, dial := startWorker(t, effect)
	first, err := DialWorker(ctx, dial(), "worker-secret")
	if err != nil {
		t.Fatal(err)
	}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "deploy", `{"env":"staging"}`)}},
		{text: "done"},
	})
	defer h.r.Close()
	var mu sync.Mutex
	remote := &RemoteTool{Worker: first, ToolName: "deploy", Environment: "staging", Epoch: h.r.Epoch}
	// The engine builds a fresh agent per invocation; reconnecting swaps in
	// a new tool value instead of mutating one a running call reads.
	h.svc.engine.Store(&engineGeneration{Engine: EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		mu.Lock()
		tool := remote
		mu.Unlock()
		a := core.NewAgent(h.client, "scripted", "", core.NewRegistry(tool))
		a.MaxRetries = 0
		return a, nil
	}), Generation: 1})
	c := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, c.ID, "a", "", "deploy it")
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.sched.Run(runCtx) }()
	<-effect.started
	first.Close()
	// The intent stays started while the outcome is unknown.
	deadline := time.Now().Add(5 * time.Second)
	for {
		view, _ := h.r.TaskView(ctx, c.ID)
		parked := false
		for _, task := range view.Tasks {
			if task.Kind == TaskKindTool && task.Effect == effectStarted && task.State == "waiting" {
				parked = true
			}
		}
		if parked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unknown outcome not parked for reconciliation")
		}
		time.Sleep(time.Millisecond)
	}
	checkValid(t, h.r)
	close(effect.block)
	second, err := DialWorker(ctx, dial(), "worker-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	mu.Lock()
	remote = &RemoteTool{Worker: second, ToolName: "deploy", Environment: "staging", Epoch: h.r.Epoch}
	mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("submission: %+v", s)
	}
	if effect.calls.Load() != 1 {
		t.Fatalf("effect executed %d times", effect.calls.Load())
	}
	result := h.client.requests[1].Messages[2].Content[0].(provider.ToolResultBlock)
	if result.IsError || !strings.Contains(core.MessageText(provider.Message{Content: result.Content}), "deployed with zot-op-") {
		t.Fatalf("recovered result: %+v", result)
	}
	checkValid(t, h.r)
}

// Host shutdown and observer cancellation never abort admitted work: the
// next host resumes it.
func TestNativeHostShutdownResumes(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "lookup", replay: core.ReplaySafe, block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "lookup", `{}`)}},
		{text: "done"},
	}, tool)
	defer h.r.Close()
	c := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, c.ID, "a", "", "go")
	host1, err := NewHost(h.r, h.svc.currentEngine(), HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// An observer subscribes and disconnects; that cancels nothing.
	_, unsubscribe := host1.Events(4)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- host1.Run(runCtx) }()
	<-tool.started
	unsubscribe()
	stop()
	<-done
	if host1.tasks.InFlight() != 0 {
		t.Fatal("shutdown returned with invocations in flight")
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "running" {
		t.Fatalf("shutdown changed admitted work: %+v", s)
	}
	checkValid(t, h.r)
	close(tool.block)
	host2, _ := NewHost(h.r, h.svc.currentEngine(), HostOptions{})
	runCtx, stop = context.WithCancel(ctx)
	defer stop()
	go host2.Run(runCtx)
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if s, err := h.r.WaitSubmission(waitCtx, sub.ID); err != nil || s.State != "answered" {
		t.Fatalf("resume: %+v %v", s, err)
	}
	if tool.calls.Load() != 2 {
		t.Fatalf("replay-safe call after shutdown: %d executions", tool.calls.Load())
	}
}

// A task whose definition is missing or newer stays blocked and inspectable;
// an older checkpoint is upgraded before dispatch.
func TestNativeMissingDefinitionAndCheckpointUpgrade(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "done"}})
	defer h.r.Close()
	c := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, c.ID, "a", "", "hi")
	// A host without task definitions leaves the generation untouched.
	bare := NewTaskScheduler(h.r, TaskRegistry{})
	if n, _, err := bare.Tick(ctx); err != nil || n != 0 {
		t.Fatalf("bare tick: %d %v", n, err)
	}
	// A record from a newer build is refused, not reinterpreted.
	snap := mustSnap(t, h.r)
	chain, _, _ := h.r.Chain(ctx, c.ID)
	gen, _, _ := read[Task](snap, taskKey(chain.Task))
	gen.Version = generationVersion + 1
	if err := h.r.commit(ctx, snap, "test", record(taskKey(gen.ID), gen)); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "running" || h.client.requestCount() != 0 {
		t.Fatalf("newer record executed: %+v", s)
	}
	// An older version is migrated by a registered upgrade.
	snap = mustSnap(t, h.r)
	gen.Version = 0
	h.r.commit(ctx, snap, "test", record(taskKey(gen.ID), gen))
	def := h.reg[TaskKindGeneration]
	def.Migrate = func(from int, phase string, cp json.RawMessage) (string, json.RawMessage, error) {
		return phase, cp, nil
	}
	h.reg[TaskKindGeneration] = def
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("upgraded record: %+v", s)
	}
	checkValid(t, h.r)
}

// The first chain raises the runtime record format to 2; a store from a newer
// build is refused instead of reinterpreted.
func TestNativeRuntimeFormat(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	h := newNativeHarness(t, store, nil)
	if f, _ := h.r.Format(ctx); f.Version != 1 {
		t.Fatalf("fresh store format: %+v", f)
	}
	c := h.nativeRoot(t)
	if f, _ := h.r.Format(ctx); f.Version != 1 {
		t.Fatalf("format before work: %+v", f)
	}
	h.r.Submit(ctx, c.ID, "a", "", "hi")
	if f, _ := h.r.Format(ctx); f.Version != 2 {
		t.Fatalf("format after the first chain: %+v", f)
	}
	checkValid(t, h.r)
	snap := mustSnap(t, h.r)
	if err := h.r.commit(ctx, snap, "test", record(runtimeFormatKey, RuntimeFormat{Version: RuntimeFormatVersion + 1, Revision: snap.Revision() + 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := New(store); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("newer format accepted: %v", err)
	}
}
