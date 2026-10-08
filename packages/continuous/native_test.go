package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// nativeHarness wires a harness's engine into task-native execution.
type nativeHarness struct {
	*harness
	native *NativeExecutor
	reg    TaskRegistry
	sched  *TaskScheduler
	// nativeRootID is set by tests whose sink inspects the root.
	nativeRootID string
}

func newNativeHarness(t *testing.T, store storage.Store, steps []scriptStep, tools ...core.Tool) *nativeHarness {
	t.Helper()
	h := newHarness(t, store, steps, tools...)
	// Forward through the harness sink so tests can replace it later.
	x, err := NewNativeExecutor(h.r, h.svc.currentEngine, ExecutionOptions{RetryDelay: time.Millisecond, Sink: func(ev core.AgentEvent) { h.svc.opts.Sink(ev) }})
	if err != nil {
		t.Fatal(err)
	}
	reg := TaskRegistry{}
	if err := x.Register(reg); err != nil {
		t.Fatal(err)
	}
	return &nativeHarness{harness: h, native: x, reg: reg, sched: x.Scheduler(reg)}
}

func (h *nativeHarness) nativeRoot(t *testing.T) Conversation {
	t.Helper()
	return h.root(t)
}

func (h *nativeHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatalf("scheduler: %v", err)
	}
}

func checkValid(t *testing.T, r *Runtime) Integrity {
	t.Helper()
	report, err := r.CheckIntegrity(context.Background())
	if err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	return report
}

func TestNativeVerticalSlice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, backend := range []string{"journal", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(dir, backend)
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
			var events []string
			h.svc.opts.Sink = func(ev core.AgentEvent) { events = append(events, ev.Type()) }
			sub, err := h.r.Submit(ctx, c.ID, "actor", "req-1", "hello")
			if err != nil || sub.State != "running" {
				t.Fatalf("admission must start the generation in the same commit: %+v %v", sub, err)
			}
			if _, ok, _ := h.r.Chain(ctx, c.ID); !ok {
				t.Fatal("no chain after admission")
			}
			checkValid(t, h.r)
			// Service.Step is a compatibility adapter over the scheduler.
			run, did, err := h.svc.Step(ctx, c.ID)
			if err != nil || !did || run.Phase != "done" || run.Outcome != "completed" || run.Turn != 2 {
				t.Fatalf("step adapter: %+v %v %v", run, did, err)
			}
			if _, ok := mustSnap(t, h.r).Get(runKey(c.ID)); ok {
				t.Fatal("run record written: execution is task-native")
			}
			if tool.calls.Load() != 2 {
				t.Fatalf("tool executions: %d", tool.calls.Load())
			}
			settled, err := h.r.Submission(ctx, sub.ID)
			if err != nil || settled.State != "answered" {
				t.Fatalf("submission: %+v %v", settled, err)
			}
			if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result tool_result assistant" {
				t.Fatalf("entries: %s", got)
			}
			second := h.client.requests[1]
			if len(second.Messages) != 3 || second.Messages[2].Role != provider.RoleTool || len(second.Messages[2].Content) != 2 || second.SessionID != c.ProviderSessionID() {
				t.Fatalf("second request: %+v", second.Messages)
			}
			if !strings.Contains(strings.Join(events, ","), "tool_result") {
				t.Fatalf("events: %v", events)
			}
			if _, ok, _ := h.r.Chain(ctx, c.ID); ok {
				t.Fatal("chain survived settlement")
			}
			usage, err := h.r.Usage(ctx, c.ID)
			if err != nil || usage.Known != 2 || usage.Unknown != 0 {
				t.Fatalf("usage: %+v %v", usage, err)
			}
			report := checkValid(t, h.r)
			if report.ActiveRuns != 0 {
				t.Fatalf("active: %+v", report)
			}
			again, err := h.r.Submit(ctx, c.ID, "actor", "req-1", "hello")
			if err != nil || again.ID != sub.ID || again.State != "answered" {
				t.Fatalf("dedup: %+v %v", again, err)
			}
			if err := h.r.Close(); err != nil {
				t.Fatal(err)
			}
			// Reopen and inspect committed state only.
			store, err = openCrashStore(ctx, backend, path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := New(store)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			checkValid(t, r)
			tasks, err := r.Tasks(ctx, c.ID)
			// One generation task spans both turns; each call is a task.
			if err != nil || len(tasks) != 3 {
				t.Fatalf("tasks: %d %v", len(tasks), err)
			}
			for _, task := range tasks {
				if task.State != "terminal" || task.Outcome != "completed" || task.Effect != "" {
					t.Fatalf("task: %+v", task)
				}
			}
			var exported strings.Builder
			if err := r.ExportSession(ctx, c.ID, &exported); err != nil || !strings.Contains(exported.String(), `"call_id":"c2"`) {
				t.Fatalf("export: %v %s", err, exported.String())
			}
		})
	}
}

func TestNativeQueuedInputsStartNextChain(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{text: "one"}, {text: "two"}})
	defer h.r.Close()
	c := h.nativeRoot(t)
	first, _ := h.r.Submit(ctx, c.ID, "actor", "", "first")
	second, err := h.r.Submit(ctx, c.ID, "actor", "", "second")
	if err != nil || second.State != "queued" {
		t.Fatalf("second admission must queue behind the active chain: %+v %v", second, err)
	}
	if _, err := h.r.SubmitWith(ctx, c.ID, "actor", "", "third", SubmitOptions{RejectBusy: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("reject busy: %v", err)
	}
	h.run(t)
	for _, id := range []string{first.ID, second.ID} {
		if s, _ := h.r.Submission(ctx, id); s.State != "answered" {
			t.Fatalf("submission %s: %+v", id, s)
		}
	}
	// The queued input is placed after the first answer, where it joined.
	if got := entryTypes(h.entries(t, c.ID)); got != "user user assistant steer assistant" {
		t.Fatalf("entries: %s", got)
	}
	if msgs := h.client.requests[1].Messages; len(msgs) != 3 || core.MessageText(msgs[1]) != "one" || core.MessageText(msgs[2]) != "second" {
		t.Fatalf("second chain context: %+v", msgs)
	}
	if n := len(h.client.requests[0].Messages); n != 1 {
		t.Fatalf("queued input leaked into the active chain: %d", n)
	}
	checkValid(t, h.r)
}

func TestNativeRetriesAreDurable(t *testing.T) {
	ctx := context.Background()
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{err: errors.New("HTTP 503 service unavailable")},
		{text: "recovered"},
	})
	defer h.r.Close()
	c := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "hello")
	h.run(t)
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "answered" {
		t.Fatalf("submission: %+v", s)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user attempt assistant" {
		t.Fatalf("entries: %s", got)
	}
	if len(h.client.requests[1].Messages) != 1 {
		t.Fatalf("failed attempt leaked into context: %+v", h.client.requests[1].Messages)
	}
	checkValid(t, h.r)
}

func TestNativeAbortDuringToolSettlesPaired(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	releaseTool := sync.OnceFunc(func() { close(tool.block) })
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`), call("c2", "effect", `{}`)}},
	}, tool)
	defer h.r.Close()
	c := h.nativeRoot(t)
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "hello")
	done := make(chan error, 1)
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer func() { cancel(); releaseTool(); _ = h.sched.Join(context.Background()) }()
	// Keep c2 pending until abort, then release the hold so the skipped
	// result can deterministically commit before c1 returns.
	h.sched.Hold = func(task Task) bool {
		if task.Kind != TaskKindTool {
			return false
		}
		in, _, _ := decodeTool(task)
		return in.CallID == "c2" && !task.AbortRequested
	}
	// Drive up to the blocked effect without an idle Run loop, so abort
	// is already committed when Run starts selecting the skipped call.
started:
	for {
		if _, _, err := h.sched.Tick(runCtx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-tool.started:
			break started
		case res := <-h.sched.done:
			h.sched.done <- res
		case <-runCtx.Done():
			t.Fatal("tool never started")
		}
	}
	run, err := h.r.Abort(ctx, c.ID)
	if err != nil || !run.AbortRequested || run.Phase != "tools" || len(run.Tools) != 2 || run.Tools[0].State != "running" {
		t.Fatalf("abort projection: %+v %v", run, err)
	}
	go func() { done <- h.sched.Run(runCtx) }()
	// Let the skipped call settle before the started effect is released.
	// Abort handlers may finish ahead of an earlier call even when effects
	// execute sequentially, so transcript entries are in completion order.
	tasks, err := h.r.Tasks(runCtx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var skippedID string
	for _, task := range tasks {
		if task.Kind != TaskKindTool {
			continue
		}
		in, _, err := decodeTool(task)
		if err != nil {
			t.Fatal(err)
		}
		if in.CallID == "c2" {
			skippedID = task.ID
		}
	}
	if skippedID == "" {
		t.Fatal("skipped call task not found")
	}
	if skipped, err := h.r.WaitTask(runCtx, skippedID); err != nil || skipped.Outcome != "aborted" {
		t.Fatalf("skipped task: %+v %v", skipped, err)
	}
	// The started effect is joined, not cancelled, then the abort settles.
	releaseTool()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("the second call executed after abort: %d", tool.calls.Load())
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "aborted" {
		t.Fatalf("submission: %+v", s)
	}
	entries := h.entries(t, c.ID)
	if got := entryTypes(entries); got != "user assistant tool_result tool_result" {
		t.Fatalf("entries: %s", got)
	}
	results := map[string]provider.ToolResultBlock{}
	for _, entry := range entries[2:] {
		msg, err := core.DecodeMessage(entry.Message)
		if err != nil || msg.Role != provider.RoleTool || len(msg.Content) != 1 {
			t.Fatalf("invalid result message: %+v %v", msg, err)
		}
		result, ok := msg.Content[0].(provider.ToolResultBlock)
		if !ok {
			t.Fatalf("invalid result block: %+v", msg.Content[0])
		}
		if _, duplicate := results[result.CallID]; duplicate {
			t.Fatalf("duplicate result for %s", result.CallID)
		}
		results[result.CallID] = result
	}
	if result, ok := results["c1"]; !ok || result.IsError || core.MessageText(provider.Message{Content: result.Content}) != "effect:{}" {
		t.Fatalf("joined effect result: %+v", result)
	}
	if result, ok := results["c2"]; !ok || !result.IsError || !strings.Contains(core.MessageText(provider.Message{Content: result.Content}), "aborted") {
		t.Fatalf("skipped call result: %+v", result)
	}
	snap, _ := h.r.Snapshot(ctx)
	messages, err := ModelContext(ctx, snap, c.ID, 0)
	if err != nil || len(messages) != 3 || len(messages[2].Content) != 2 {
		t.Fatalf("context not paired: %+v %v", messages, err)
	}
	// Persisted results follow completion order, but model context must
	// still pair them in the original assistant call order.
	for i, id := range []string{"c1", "c2"} {
		result, ok := messages[2].Content[i].(provider.ToolResultBlock)
		if !ok || result.CallID != id || result.IsError != results[id].IsError {
			t.Fatalf("model result %d: %+v", i, messages[2].Content[i])
		}
	}
	checkValid(t, h.r)
}

func TestNativeAbortDuringRequestKeepsIntent(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect"}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
	}, tool)
	defer h.r.Close()
	c := h.nativeRoot(t)
	h.client.onStream = func(provider.Request) {
		// Abort lands while the response is in flight. It must survive the
		// response commit and prevent the tool from executing.
		if _, err := h.r.Abort(ctx, c.ID); err != nil {
			t.Error(err)
		}
	}
	sub, _ := h.r.Submit(ctx, c.ID, "actor", "", "hello")
	h.run(t)
	if tool.calls.Load() != 0 {
		t.Fatal("tool executed after abort")
	}
	if s, _ := h.r.Submission(ctx, sub.ID); s.State != "aborted" {
		t.Fatalf("submission: %+v", s)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result" {
		t.Fatalf("entries: %s", got)
	}
	checkValid(t, h.r)
}

func TestTaskStartEffectFencing(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	var sawInterrupted []bool
	stale := make(chan TaskContext, 1)
	crashCtx, crash := context.WithCancel(ctx)
	reg := TaskRegistry{}
	reg.Register(TaskDefinition{
		Kind: "effect", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "go"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"go": func(ctx context.Context, tc TaskContext) (Next, error) {
				sawInterrupted = append(sawInterrupted, tc.Interrupted)
				if len(sawInterrupted) == 1 {
					if err := tc.StartEffect(ctx, map[string]int{"n": 1}, nil); err != nil {
						return Next{}, err
					}
					stale <- tc
					// Simulate a crash after the intent commit: the host
					// stops before the phase records its outcome.
					crash()
					<-ctx.Done()
					return Next{}, ctx.Err()
				}
				return Next{Outcome: "completed"}, nil
			},
		},
	})
	task, err := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "effect"})
	if err != nil {
		t.Fatal(err)
	}
	s := NewTaskScheduler(r, reg)
	s.Tick(crashCtx)
	if err := s.Join(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Task(ctx, task.ID)
	if got.Effect != effectStarted || got.State != "running" {
		t.Fatalf("effect intent not committed: %+v", got)
	}
	checkValid(t, r)
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sawInterrupted) != 2 || sawInterrupted[0] || !sawInterrupted[1] {
		t.Fatalf("interrupted flags: %v", sawInterrupted)
	}
	got, _ = r.Task(ctx, task.ID)
	if got.Outcome != "completed" || got.Effect != "" {
		t.Fatalf("task: %+v", got)
	}
	// The first invocation is stale: it can no longer commit.
	old := <-stale
	if err := old.StartEffect(ctx, nil, nil); !errors.Is(err, ErrStaleInvocation) {
		t.Fatalf("stale invocation committed: %v", err)
	}
}

func TestTaskStartEffectRefusedAfterAbort(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	effects := 0
	reg := TaskRegistry{}
	var id string
	reg.Register(TaskDefinition{
		Kind: "effect", Version: 1, JoinOnAbort: true,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "go"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"go": func(ctx context.Context, tc TaskContext) (Next, error) {
				if err := r.AbortTask(ctx, id, false); err != nil {
					return Next{}, err
				}
				if err := tc.StartEffect(ctx, nil, nil); err != nil {
					return Next{}, err
				}
				effects++
				return Next{Outcome: "completed"}, nil
			},
		},
	})
	task, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "effect"})
	id = task.ID
	if err := NewTaskScheduler(r, reg).Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Task(ctx, id)
	if effects != 0 || got.Outcome != "aborted" {
		t.Fatalf("effect started after abort: %d %+v", effects, got)
	}
}

func TestTaskWaitingParentReleasesWorker(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	reg := TaskRegistry{}
	reg.Register(TaskDefinition{
		Kind: "leaf", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "go"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"go": func(context.Context, TaskContext) (Next, error) { return Next{Outcome: "completed"}, nil },
		},
	})
	reg.Register(TaskDefinition{
		Kind: "parent", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "spawn"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"spawn": func(context.Context, TaskContext) (Next, error) {
				return Next{Commit: func(tx *TaskTx) (Next, error) {
					a, err := tx.CreateChild(TaskSpec{Kind: "parent2"})
					if err != nil {
						return Next{}, err
					}
					return Next{Phase: "done", WaitOn: []string{a}}, nil
				}}, nil
			},
			"done": func(context.Context, TaskContext) (Next, error) { return Next{Outcome: "completed"}, nil },
		},
	})
	reg.Register(TaskDefinition{
		Kind: "parent2", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "spawn"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"spawn": func(context.Context, TaskContext) (Next, error) {
				return Next{Commit: func(tx *TaskTx) (Next, error) {
					a, err := tx.CreateChild(TaskSpec{Kind: "leaf"})
					if err != nil {
						return Next{}, err
					}
					return Next{Phase: "done", WaitOn: []string{a}}, nil
				}}, nil
			},
			"done": func(context.Context, TaskContext) (Next, error) { return Next{Outcome: "completed"}, nil },
		},
	})
	root, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "parent"})
	s := NewTaskScheduler(r, reg)
	s.Workers = 1
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.Run(runCtx); err != nil {
		t.Fatalf("nested waits exhausted a single worker: %v", err)
	}
	if got, _ := r.Task(ctx, root.ID); got.Outcome != "completed" {
		t.Fatalf("root: %+v", got)
	}
	checkValid(t, r)
}

// TestNativeProcessCrashInsideTool kills the process inside a tool after its
// effect intent committed, on both durable backends. The unsafe effect is
// reported, never repeated; the replay-safe one is rerun exactly once.
func TestNativeProcessCrashInsideTool(t *testing.T) {
	if path := os.Getenv("ZOT_CONTINUOUS_NATIVE_CRASH_STORE"); path != "" {
		ctx := context.Background()
		store, err := openCrashStore(ctx, os.Getenv("ZOT_CONTINUOUS_EXEC_CRASH_BACKEND"), path)
		if err != nil {
			t.Fatal(err)
		}
		marker := os.Getenv("ZOT_CONTINUOUS_EXEC_CRASH_MARKER")
		// crashingTool exits only when this variable is set.
		os.Setenv("ZOT_CONTINUOUS_EXEC_CRASH_STORE", path)
		h := newNativeHarness(t, store, []scriptStep{
			{calls: []provider.ToolCallBlock{call("s1", "safe", `{}`), call("u1", "unsafe", `{}`)}},
		}, &crashingTool{name: "unsafe", marker: marker}, &crashingTool{name: "safe", marker: marker, replay: core.ReplaySafe})
		c := h.nativeRoot(t)
		h.r.Submit(ctx, c.ID, "actor", "req", "hello")
		h.sched.Run(ctx)
		t.Fatal("child survived the crash point")
	}
	for _, backend := range []string{"journal", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "store")
			marker := filepath.Join(dir, "effects")
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeProcessCrashInsideTool$")
			cmd.Env = append(os.Environ(), "ZOT_CONTINUOUS_NATIVE_CRASH_STORE="+path, "ZOT_CONTINUOUS_EXEC_CRASH_MARKER="+marker, "ZOT_CONTINUOUS_EXEC_CRASH_BACKEND="+backend)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("child did not crash: %s", out)
			}
			if effects := readEffects(t, marker); effects != "safe unsafe" {
				t.Fatalf("child effects: %q", effects)
			}
			ctx := context.Background()
			store, err := openCrashStore(ctx, backend, path)
			if err != nil {
				t.Fatal(err)
			}
			h := newNativeHarness(t, store, []scriptStep{{text: "done"}}, &crashingTool{name: "unsafe", marker: marker}, &crashingTool{name: "safe", marker: marker, replay: core.ReplaySafe})
			defer h.r.Close()
			c, err := h.r.OpenRoot(ctx, "workspace", AgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			checkValid(t, h.r)
			if _, ok, err := h.r.Chain(ctx, c.ID); err != nil || !ok {
				t.Fatalf("interrupted chain not persisted: %v", err)
			}
			h.run(t)
			if effects := readEffects(t, marker); effects != "safe unsafe" {
				t.Fatalf("effects after recovery: %q", effects)
			}
			results := h.client.requests[0].Messages[2].Content
			if len(results) != 2 {
				t.Fatalf("results: %+v", results)
			}
			safe, unsafe := results[0].(provider.ToolResultBlock), results[1].(provider.ToolResultBlock)
			if safe.IsError || !unsafe.IsError || !strings.Contains(core.MessageText(provider.Message{Content: unsafe.Content}), "interrupted") {
				t.Fatalf("results: safe=%+v unsafe=%+v", safe, unsafe)
			}
			sub, err := h.r.Submit(ctx, c.ID, "actor", "req", "hello")
			if err != nil || sub.State != "answered" {
				t.Fatalf("submission: %+v %v", sub, err)
			}
			checkValid(t, h.r)
		})
	}
}
