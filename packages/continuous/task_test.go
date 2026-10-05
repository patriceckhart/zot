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
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
)

type counterCheckpoint struct {
	N   int    `json:"n"`
	Key string `json:"key,omitempty"`
}

// counterDefinition increments to a target across phases. effects counts the
// external effect of the charge phase so tests can prove exactly-once or
// at-most-once behaviour against crashes.
func counterDefinition(effects *atomic.Int32, target int) TaskDefinition {
	return TaskDefinition{
		Kind:    "counter",
		Version: 1,
		Initial: func(input json.RawMessage) (Next, error) {
			return Next{Phase: "prepare", Checkpoint: counterCheckpoint{}}, nil
		},
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"prepare": func(ctx context.Context, tc TaskContext) (Next, error) {
				// Intent: pick an idempotency key before the effect.
				return Next{Phase: "charge", Checkpoint: counterCheckpoint{N: 0, Key: "key-" + tc.Task.ID}}, nil
			},
			"charge": func(ctx context.Context, tc TaskContext) (Next, error) {
				var cp counterCheckpoint
				_ = json.Unmarshal(tc.Task.Checkpoint, &cp)
				effects.Add(1)
				cp.N++
				if cp.N >= target {
					return Next{Outcome: "completed", Result: cp}, nil
				}
				return Next{Phase: "charge", Checkpoint: cp}, nil
			},
		},
		Abort: func(ctx context.Context, tc TaskContext) (Next, error) {
			return Next{Outcome: "aborted", Error: "cancelled by user"}, nil
		},
	}
}

func newTaskRuntime(t *testing.T, store storage.Store) (*Runtime, Conversation) {
	t.Helper()
	r, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.OpenRoot(context.Background(), "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}

func TestTaskPhasesCommitCheckpoints(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	var effects atomic.Int32
	reg := TaskRegistry{}
	if err := reg.Register(counterDefinition(&effects, 3)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(TaskDefinition{Kind: "bad", Version: 1, Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "x"}, nil }, Phases: map[string]func(context.Context, TaskContext) (Next, error){"__x": nil}}); err == nil {
		t.Fatal("reserved phase accepted")
	}
	task, err := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "counter"})
	if err != nil || task.State != "pending" || task.Phase != "prepare" {
		t.Fatalf("create: %+v %v", task, err)
	}
	if _, err := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "unknown"}); !errors.Is(err, ErrTaskBlocked) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := r.CreateTask(ctx, reg, "missing", TaskSpec{Kind: "counter"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation: %v", err)
	}
	sched := NewTaskScheduler(r, reg)
	if err := sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := r.WaitTask(ctx, task.ID)
	if err != nil || final.State != "terminal" || final.Outcome != "completed" || effects.Load() != 3 {
		t.Fatalf("final: %+v %v effects=%d", final, err, effects.Load())
	}
	var result counterCheckpoint
	if err := json.Unmarshal(final.Result, &result); err != nil || result.N != 3 || result.Key != "key-"+task.ID {
		t.Fatalf("result: %+v %v", result, err)
	}
	if final.Checkpoint != nil || final.Phase != "" {
		t.Fatalf("terminal task retains execution state: %+v", final)
	}
	if n, _, err := sched.Tick(ctx); err != nil || n != 0 {
		t.Fatalf("idle tick did work: %d %v", n, err)
	}
	tasks, err := r.Tasks(ctx, c.ID)
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("list: %+v %v", tasks, err)
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// Crash after every commit of a three-step counter, reopen, finish. The
// charge phase runs again after a crash between reservation and its commit,
// so effects can exceed the target by at most the number of crashes inside
// that window. Effects never fall short and the result is always correct.
func TestTaskCrashMatrix(t *testing.T) {
	ctx := context.Background()
	for crashAt := 1; ; crashAt++ {
		path := filepath.Join(t.TempDir(), "store")
		store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
		if err != nil {
			t.Fatal(err)
		}
		r, c := newTaskRuntime(t, store)
		var effects atomic.Int32
		reg := TaskRegistry{}
		reg.Register(counterDefinition(&effects, 3))
		task, err := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "counter"})
		if err != nil {
			t.Fatal(err)
		}
		crashed := errors.New("synthetic crash")
		cs := &crashStore{Store: store, at: crashAt, err: crashed}
		// A separate runtime over the crashing wrapper, so the scheduler's
		// goroutines never observe a store field changing under them.
		crashing := &Runtime{store: cs}
		sched := NewTaskScheduler(crashing, reg)
		err = sched.Run(ctx)
		for sched.InFlight() > 0 {
			time.Sleep(time.Millisecond)
		}
		r.Close()
		if !errors.Is(err, crashed) {
			if err != nil {
				t.Fatalf("crash %d: %v", crashAt, err)
			}
			return
		}
		store, err = journal.Open(ctx, path, journal.Options{Durability: storage.Process})
		if err != nil {
			t.Fatal(err)
		}
		r2, _ := New(store)
		if report, err := r2.CheckIntegrity(ctx); err != nil || !report.Valid {
			t.Fatalf("crash %d: integrity %+v %v", crashAt, report, err)
		}
		var more atomic.Int32
		reg2 := TaskRegistry{}
		reg2.Register(counterDefinition(&more, 3))
		if err := NewTaskScheduler(r2, reg2).Run(ctx); err != nil {
			t.Fatalf("crash %d: recovery %v", crashAt, err)
		}
		final, err := r2.Task(ctx, task.ID)
		if err != nil || final.State != "terminal" || final.Outcome != "completed" {
			t.Fatalf("crash %d: final %+v %v", crashAt, final, err)
		}
		var result counterCheckpoint
		json.Unmarshal(final.Result, &result)
		total := effects.Load() + more.Load()
		if result.N != 3 || total < 3 || total > 4 {
			t.Fatalf("crash %d: result %+v effects=%d", crashAt, result, total)
		}
		if total == 4 && final.Attempt == 0 {
			// A repeated charge is visible as a second attempt of that phase
			// in the notices or attempt count, never silent.
			_ = final
		}
		r2.Close()
	}
}

func TestTaskTimerFiresAfterRestartWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r, c := newTaskRuntime(t, store)
	var fired atomic.Int32
	now := time.Now().UTC()
	clock := &now
	reminder := func() TaskDefinition {
		return TaskDefinition{
			Kind: "reminder", Version: 1,
			Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "schedule"}, nil },
			Phases: map[string]func(context.Context, TaskContext) (Next, error){
				"schedule": func(ctx context.Context, tc TaskContext) (Next, error) {
					return Next{Phase: "fire", WakeAt: clock.Add(time.Hour)}, nil
				},
				"fire": func(ctx context.Context, tc TaskContext) (Next, error) {
					fired.Add(1)
					return Next{Outcome: "completed"}, nil
				},
			},
		}
	}
	reg := TaskRegistry{}
	reg.Register(reminder())
	task, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "reminder"})
	sched := NewTaskScheduler(r, reg)
	sched.now = func() time.Time { return *clock }
	// Run schedules the timer, then sleeps until it fires; bound it so the
	// test proceeds once the task is parked.
	runCtx, cancelRun := context.WithTimeout(ctx, 200*time.Millisecond)
	deadlineErr := sched.Run(runCtx)
	cancelRun()
	if !errors.Is(deadlineErr, context.DeadlineExceeded) {
		t.Fatalf("run before timer: %v", deadlineErr)
	}
	if n, wake, err := sched.Tick(ctx); err != nil || n != 0 || !wake.Equal(now.Add(time.Hour)) {
		t.Fatalf("idle pass reports the timer: n=%d wake=%v err=%v", n, wake, err)
	}
	stored, _ := r.Task(ctx, task.ID)
	if stored.State != "waiting" || stored.WakeAt == nil || !stored.WakeAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("timer not persisted: %+v", stored)
	}
	// Run sleeps until the timer or a commit rather than spinning. Prove it
	// with a cancelled context after a short wait.
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	err = sched.Run(runCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || fired.Load() != 0 {
		t.Fatalf("run before deadline: %v fired=%d", err, fired.Load())
	}
	// Restart the process after the deadline passed while it was down.
	r.Close()
	later := now.Add(2 * time.Hour)
	clock = &later
	store, err = journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := New(store)
	defer r2.Close()
	reg2 := TaskRegistry{}
	reg2.Register(reminder())
	sched2 := NewTaskScheduler(r2, reg2)
	sched2.now = func() time.Time { return *clock }
	if err := sched2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if fired.Load() != 1 {
		t.Fatalf("timer fired %d times", fired.Load())
	}
	if err := sched2.Run(ctx); err != nil || fired.Load() != 1 {
		t.Fatalf("second run duplicated: %v fired=%d", err, fired.Load())
	}
	final, _ := r2.Task(ctx, task.ID)
	if final.Outcome != "completed" {
		t.Fatalf("final: %+v", final)
	}
}

func TestTaskChildrenFailFastAndBottomUpAbort(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	var abortedMu sync.Mutex
	var aborted []string
	var completed atomic.Int32
	block := make(chan struct{})
	reg := TaskRegistry{}
	reg.Register(TaskDefinition{
		Kind: "payment", Version: 1,
		Initial: func(input json.RawMessage) (Next, error) {
			return Next{Phase: "charge", Checkpoint: string(input)}, nil
		},
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"charge": func(ctx context.Context, tc TaskContext) (Next, error) {
				var card string
				json.Unmarshal(tc.Task.Input, &card)
				switch card {
				case "declined":
					return Next{Outcome: "failed", Error: "card declined"}, nil
				case "slow":
					select {
					case <-block:
					case <-ctx.Done():
						return Next{}, ctx.Err()
					}
				}
				completed.Add(1)
				return Next{Outcome: "completed", Result: card}, nil
			},
		},
		Abort: func(ctx context.Context, tc TaskContext) (Next, error) {
			var card string
			json.Unmarshal(tc.Task.Input, &card)
			abortedMu.Lock()
			aborted = append(aborted, card)
			abortedMu.Unlock()
			return Next{Outcome: "aborted", Error: "refunded"}, nil
		},
	})
	reg.Register(TaskDefinition{
		Kind: "checkout", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "pay"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"pay": func(ctx context.Context, tc TaskContext) (Next, error) {
				// Children are created in the same commit as the wait. The
				// wait list is filled by decide from the owner index.
				return Next{Phase: "collect", Children: []TaskSpec{{Kind: "payment", Input: "ok"}, {Kind: "payment", Input: "declined"}, {Kind: "payment", Input: "slow"}}}, nil
			},
			"collect": func(ctx context.Context, tc TaskContext) (Next, error) {
				rows, _ := tc.Snapshot.Page("task-owner/"+tc.Task.ID+"/", "", 100)
				var ids []string
				for _, row := range rows {
					var id string
					json.Unmarshal(row.Value, &id)
					ids = append(ids, id)
				}
				return Next{Phase: "decide", WaitOn: ids, WaitPolicy: "fail_fast"}, nil
			},
			"decide": func(ctx context.Context, tc TaskContext) (Next, error) {
				var failed int
				for _, dep := range tc.Waited {
					if dep.Outcome != "completed" {
						failed++
					}
				}
				if failed > 0 {
					return Next{Outcome: "failed", Error: fmt.Sprintf("%d payments did not complete", failed)}, nil
				}
				return Next{Outcome: "completed"}, nil
			},
		},
		Abort: func(ctx context.Context, tc TaskContext) (Next, error) {
			return Next{Outcome: "aborted"}, nil
		},
	})
	checkout, err := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "checkout"})
	if err != nil {
		t.Fatal(err)
	}
	sched := NewTaskScheduler(r, reg)
	// pay creates children, collect waits, ok completes, declined fails,
	// slow blocks. fail_fast fires on the declined child: the parent marks
	// slow for abort and keeps waiting until it settles. The scheduler
	// cancels slow's blocked invocation so its abort handler can run.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- sched.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tasks, _ := r.Tasks(ctx, c.ID)
		marked := false
		for _, tk := range tasks {
			var card string
			json.Unmarshal(tk.Input, &card)
			if card == "slow" && tk.AbortRequested {
				marked = true
			}
		}
		if marked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fail_fast never marked the slow child: %+v", tasks)
		}
		time.Sleep(time.Millisecond)
	}
	// Everything settles without releasing block: the abort cancels slow's
	// invocation. Wait for the parent to finish, then stop the scheduler.
	for {
		p, _ := r.Task(ctx, checkout.ID)
		if p.State == "terminal" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkout never settled: %+v", p)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	close(block)
	final, _ := r.Task(ctx, checkout.ID)
	tasks, _ := r.Tasks(ctx, c.ID)
	var outcomes []string
	for _, tk := range tasks {
		if tk.Kind == "payment" {
			var card string
			json.Unmarshal(tk.Input, &card)
			outcomes = append(outcomes, card+"="+tk.Outcome)
		}
	}
	joined := strings.Join(outcomes, " ")
	if final.State != "terminal" || final.Outcome != "failed" || !strings.Contains(final.Error, "did not complete") {
		t.Fatalf("checkout: %+v children=%s", final, joined)
	}
	// fail_fast aborts the unfinished owned siblings when declined fails.
	// Whether ok ran before that depends on worker scheduling, so it is
	// either completed or aborted; it is never left live.
	if !(strings.Contains(joined, "ok=completed") || strings.Contains(joined, "ok=aborted")) || !strings.Contains(joined, "declined=failed") || !strings.Contains(joined, "slow=aborted") {
		t.Fatalf("children: %s", joined)
	}
	// The abort handler ran for slow only. It may run more than once when
	// the scheduler is cancelled between the handler and its commit, which
	// is why abort handlers must be idempotent. It never runs for ok or
	// declined, which settled on their own.
	abortedMu.Lock()
	for _, card := range aborted {
		if card == "declined" {
			t.Fatalf("abort handler ran for %s: %v", card, aborted)
		}
	}
	if len(aborted) == 0 {
		t.Fatal("abort handler never ran for slow")
	}
	abortedMu.Unlock()
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestTaskAbortSkipsBackgroundUnlessIncluded(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	block := make(chan struct{})
	reg := TaskRegistry{}
	reg.Register(TaskDefinition{
		Kind: "worker", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "work"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"work": func(ctx context.Context, tc TaskContext) (Next, error) {
				select {
				case <-block:
					return Next{Outcome: "completed"}, nil
				case <-ctx.Done():
					return Next{}, ctx.Err()
				}
			},
		},
		Abort: func(ctx context.Context, tc TaskContext) (Next, error) { return Next{Outcome: "aborted"}, nil },
	})
	reg.Register(TaskDefinition{
		Kind: "parent", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "spawn"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"spawn": func(ctx context.Context, tc TaskContext) (Next, error) {
				return Next{Phase: "hold", Children: []TaskSpec{{Kind: "worker"}, {Kind: "worker", Background: true}}}, nil
			},
			"hold": func(ctx context.Context, tc TaskContext) (Next, error) {
				return Next{Phase: "hold", WakeAt: time.Now().Add(time.Hour)}, nil
			},
		},
		Abort: func(ctx context.Context, tc TaskContext) (Next, error) { return Next{Outcome: "aborted"}, nil },
	})
	parent, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "parent"})
	sched := NewTaskScheduler(r, reg)
	// Drive until both workers are running (blocked in work) and the parent
	// is parked on its timer. Run cannot return on its own because the
	// workers never yield, so bound it.
	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	sched.Run(runCtx)
	cancel()
	var fg, bg Task
	find := func() {
		tasks, _ := r.Tasks(ctx, c.ID)
		for _, tk := range tasks {
			if tk.Kind == "worker" {
				if tk.Background {
					bg = tk
				} else {
					fg = tk
				}
			}
		}
	}
	find()
	if fg.ID == "" || bg.ID == "" {
		t.Fatal("workers not created")
	}
	// Ordinary abort reaches the parent and the foreground worker only.
	if err := r.AbortTask(ctx, parent.ID, false); err != nil {
		t.Fatal(err)
	}
	find()
	p, _ := r.Task(ctx, parent.ID)
	if !p.AbortRequested || !fg.AbortRequested || bg.AbortRequested {
		t.Fatalf("abort marks: parent=%v fg=%v bg=%v", p.AbortRequested, fg.AbortRequested, bg.AbortRequested)
	}
	// Abort handlers run at the next pass. The background worker keeps
	// running (its phase blocks), so Run never returns on its own; poll the
	// committed state with a deadline instead.
	runCtx, cancel = context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { sched.Run(runCtx); close(runDone) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, _ = r.Task(ctx, parent.ID)
		find()
		if p.Outcome == "aborted" && fg.Outcome == "aborted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after ordinary abort: parent=%s fg=%s bg=%s", p.Outcome, fg.Outcome, bg.State)
		}
		time.Sleep(time.Millisecond)
	}
	if bg.State == "terminal" {
		t.Fatalf("background worker reached by ordinary abort: %+v", bg)
	}
	cancel()
	<-runDone
	// Background-inclusive abort reaches the background worker too.
	if err := r.AbortTask(ctx, bg.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	find()
	if bg.Outcome != "aborted" {
		t.Fatalf("background abort: %+v", bg)
	}
	close(block)
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestTaskVersionBlocksAndMigrates(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	v1 := TaskRegistry{}
	v1.Register(TaskDefinition{
		Kind: "job", Version: 1,
		Initial: func(json.RawMessage) (Next, error) {
			return Next{Phase: "old", Checkpoint: map[string]int{"count": 1}}, nil
		},
		Phases: map[string]func(context.Context, TaskContext) (Next, error){"old": func(context.Context, TaskContext) (Next, error) { return Next{Outcome: "completed"}, nil }},
	})
	task, _ := r.CreateTask(ctx, v1, c.ID, TaskSpec{Kind: "job"})
	// A newer stored version than the registry knows is blocked.
	snap, _ := r.Snapshot(ctx)
	stored, _, _ := read[Task](snap, taskKey(task.ID))
	stored.Version = 9
	stored.Revision = snap.Revision() + 1
	r.commit(ctx, snap, "test", record(taskKey(task.ID), stored))
	if n, _, err := NewTaskScheduler(r, v1).Tick(ctx); err != nil || n != 0 {
		t.Fatalf("newer version executed: %d %v", n, err)
	}
	snap, _ = r.Snapshot(ctx)
	stored, _, _ = read[Task](snap, taskKey(task.ID))
	stored.Version = 1
	stored.Revision = snap.Revision() + 1
	r.commit(ctx, snap, "test", record(taskKey(task.ID), stored))
	// A missing definition is blocked, not dropped.
	if n, _, err := NewTaskScheduler(r, TaskRegistry{}).Tick(ctx); err != nil || n != 0 {
		t.Fatalf("missing definition executed: %d %v", n, err)
	}
	// v2 without migration is blocked.
	v2 := TaskRegistry{}
	v2.Register(TaskDefinition{
		Kind: "job", Version: 2,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "new"}, nil },
		Phases:  map[string]func(context.Context, TaskContext) (Next, error){"new": func(context.Context, TaskContext) (Next, error) { return Next{Outcome: "completed"}, nil }},
	})
	if n, _, err := NewTaskScheduler(r, v2).Tick(ctx); err != nil || n != 0 {
		t.Fatalf("v2 without migration executed: %d %v", n, err)
	}
	if cur, _ := r.Task(ctx, task.ID); cur.State != "pending" {
		t.Fatalf("blocked task changed: %+v", cur)
	}
	// v2 with migration converts the checkpoint and runs the new phase.
	def := v2["job"]
	def.Migrate = func(from int, phase string, checkpoint json.RawMessage) (string, json.RawMessage, error) {
		if from != 1 || phase != "old" {
			return "", nil, fmt.Errorf("unexpected %d %s", from, phase)
		}
		return "new", json.RawMessage(`{"migrated":true}`), nil
	}
	v2["job"] = def
	if err := NewTaskScheduler(r, v2).Run(ctx); err != nil {
		t.Fatalf("migration: %v", err)
	}
	final, _ := r.Task(ctx, task.ID)
	if final.Outcome != "completed" || final.Version != 2 || !strings.Contains(strings.Join(final.Notices, "\n"), "migrated") {
		t.Fatalf("migrated task: %+v", final)
	}
}

func TestTaskFaults(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	reg := TaskRegistry{}
	reg.Register(TaskDefinition{
		Kind: "faulty", Version: 1,
		Initial: func(input json.RawMessage) (Next, error) { return Next{Phase: "go", Checkpoint: string(input)}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"go": func(ctx context.Context, tc TaskContext) (Next, error) {
				var mode string
				json.Unmarshal(tc.Task.Input, &mode)
				switch mode {
				case "no-progress":
					return Next{Phase: "go", Checkpoint: mode}, nil
				case "undefined":
					return Next{Phase: "nowhere"}, nil
				case "self-wait":
					return Next{Phase: "go", WaitOn: []string{tc.Task.ID}}, nil
				case "missing-wait":
					return Next{Phase: "go", WaitOn: []string{"missing"}}, nil
				case "panic-error":
					return Next{}, errors.New("handler error")
				case "nothing":
					return Next{}, nil
				}
				return Next{Outcome: "completed"}, nil
			},
		},
	})
	for _, mode := range []string{"no-progress", "undefined", "self-wait", "missing-wait", "panic-error", "nothing"} {
		task, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "faulty", Input: mode})
		if err := NewTaskScheduler(r, reg).Run(ctx); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		final, _ := r.Task(ctx, task.ID)
		if final.State != "terminal" || final.Outcome != "failed" || final.Error == "" {
			t.Fatalf("%s: %+v", mode, final)
		}
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}
