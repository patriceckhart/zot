package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// A retry policy persists the next attempt deadline, backs off exponentially
// with a cap, and gives up after MaxAttempts or on permanent errors.
func TestTaskRetryPolicyPersistsDeadline(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	now := time.Now().UTC()
	clock := &now
	var attempts atomic.Int32
	var failUntil int32 = 3
	def := TaskDefinition{
		Kind: "flaky", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "work"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"work": func(ctx context.Context, tc TaskContext) (Next, error) {
				n := attempts.Add(1)
				if n <= failUntil {
					return Next{}, fmt.Errorf("transient %d", n)
				}
				return Next{Outcome: "completed", Result: map[string]int{"attempts": int(n)}}, nil
			},
		},
		Retry: RetryPolicy{MaxAttempts: 5, Backoff: time.Minute, MaxBackoff: 3 * time.Minute},
	}
	reg := TaskRegistry{}
	reg.Register(def)
	task, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "flaky"})
	sched := NewTaskScheduler(r, reg)
	sched.now = func() time.Time { return *clock }
	// First pass: attempt 1 fails, retry at +1m.
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	stored, _ := r.Task(ctx, task.ID)
	if stored.RetryAt == nil || !stored.RetryAt.Equal(now.Add(time.Minute)) || stored.Attempt != 1 || stored.LastError != "transient 1" || stored.State != "running" {
		t.Fatalf("after first failure: %+v", stored)
	}
	// Not yet due: nothing runs, the wake time is reported.
	if n, wake, err := sched.Tick(ctx); err != nil || n != 0 || !wake.Equal(*stored.RetryAt) {
		t.Fatalf("premature retry: n=%d wake=%v err=%v", n, wake, err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts before deadline: %d", attempts.Load())
	}
	// Advance the clock past each deadline. Attempt 2 waits 2m, attempt 3 is
	// capped at 3m, attempt 4 succeeds.
	for _, expect := range []time.Duration{2 * time.Minute, 3 * time.Minute} {
		cur, _ := r.Task(ctx, task.ID)
		*clock = cur.RetryAt.Add(time.Second)
		if err := runUntilIdle(ctx, sched); err != nil {
			t.Fatal(err)
		}
		cur, _ = r.Task(ctx, task.ID)
		if cur.RetryAt == nil || !cur.RetryAt.Equal(clock.Add(expect)) {
			t.Fatalf("backoff after attempt %d: %+v (want +%s)", cur.Attempt, cur, expect)
		}
	}
	cur, _ := r.Task(ctx, task.ID)
	*clock = cur.RetryAt.Add(time.Second)
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	final, _ := r.Task(ctx, task.ID)
	if final.Outcome != "completed" || attempts.Load() != 4 || final.RetryAt != nil || final.LastError != "" {
		t.Fatalf("final: %+v attempts=%d", final, attempts.Load())
	}
	// Permanent errors are not retried.
	attempts.Store(0)
	reg2 := TaskRegistry{}
	def.Phases["work"] = func(ctx context.Context, tc TaskContext) (Next, error) {
		attempts.Add(1)
		return Next{}, fmt.Errorf("%w: credentials rejected", ErrTaskPermanent)
	}
	reg2.Register(def)
	task2, _ := r.CreateTask(ctx, reg2, c.ID, TaskSpec{Kind: "flaky"})
	sched2 := NewTaskScheduler(r, reg2)
	sched2.now = func() time.Time { return *clock }
	if err := runUntilIdle(ctx, sched2); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Task(ctx, task2.ID)
	if got.Outcome != "failed" || attempts.Load() != 1 {
		t.Fatalf("permanent error retried: %+v attempts=%d", got, attempts.Load())
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// runUntilIdle ticks until no invocation is in flight and no task is due.
func runUntilIdle(ctx context.Context, s *TaskScheduler) error {
	for {
		n, wake, err := s.Tick(ctx)
		if err != nil {
			return err
		}
		if n == 0 && s.InFlight() == 0 {
			if wake.IsZero() || s.now().Before(wake) {
				return nil
			}
		}
		if s.InFlight() > 0 {
			if err := s.waitProgress(ctx, time.Time{}); err != nil {
				return err
			}
		}
	}
}

// Cleanup runs after a failed outcome and before the task becomes terminal.
// A cleanup that keeps failing leaves the task aborting and blocked, never
// deceptively terminal, until a human retries it.
func TestTaskCleanupBlocksAndRetries(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	now := time.Now().UTC()
	clock := &now
	var cleanups atomic.Int32
	var cleanupFails atomic.Bool
	cleanupFails.Store(true)
	def := TaskDefinition{
		Kind: "payment", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "charge"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"charge": func(ctx context.Context, tc TaskContext) (Next, error) {
				return Next{Outcome: "failed", Error: "declined"}, nil
			},
		},
		Cleanup: func(ctx context.Context, tc TaskContext) error {
			cleanups.Add(1)
			if cleanupFails.Load() {
				return errors.New("refund endpoint down")
			}
			return nil
		},
		Retry: RetryPolicy{MaxAttempts: 2, Backoff: time.Minute},
	}
	reg := TaskRegistry{}
	reg.Register(def)
	task, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "payment"})
	sched := NewTaskScheduler(r, reg)
	sched.now = func() time.Time { return *clock }
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	stored, _ := r.Task(ctx, task.ID)
	if stored.State != "aborting" || stored.Outcome != "" || stored.RetryAt == nil || cleanups.Load() != 1 {
		t.Fatalf("after first cleanup failure: %+v cleanups=%d", stored, cleanups.Load())
	}
	*clock = stored.RetryAt.Add(time.Second)
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	stored, _ = r.Task(ctx, task.ID)
	if stored.State != "aborting" || stored.Blocked == "" || stored.Outcome != "" || cleanups.Load() != 2 {
		t.Fatalf("not blocked after exhausting retries: %+v cleanups=%d", stored, cleanups.Load())
	}
	// Blocked tasks are left alone by the scheduler and visible as live.
	if err := runUntilIdle(ctx, sched); err != nil || cleanups.Load() != 2 {
		t.Fatalf("blocked task ran: %v cleanups=%d", err, cleanups.Load())
	}
	tasks, _ := r.Tasks(ctx, c.ID)
	if len(tasks) != 1 || tasks[0].Blocked == "" {
		t.Fatalf("tasks: %+v", tasks)
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity while blocked: %+v %v", report, err)
	}
	// A human fixes the cause and asks for another cleanup attempt.
	cleanupFails.Store(false)
	if _, err := r.RetryCleanup(ctx, task.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	final, _ := r.Task(ctx, task.ID)
	if final.State != "terminal" || final.Outcome != "failed" || final.Error != "declined" || final.Blocked != "" || cleanups.Load() != 3 {
		t.Fatalf("final: %+v cleanups=%d", final, cleanups.Load())
	}
	if _, err := r.RetryCleanup(ctx, task.ID, "operator"); err == nil {
		t.Fatal("retry of a settled task accepted")
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// Overdue periodic timers fire once after downtime. The skip policy records
// how many occurrences were coalesced instead of replaying each one.
func TestTaskTimerCatchUpCoalesces(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	now := time.Now().UTC()
	clock := &now
	var fired atomic.Int32
	def := TaskDefinition{
		Kind: "heartbeat", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "schedule"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"schedule": func(ctx context.Context, tc TaskContext) (Next, error) {
				return Next{Phase: "beat", WakeAt: clock.Add(time.Minute), Repeat: time.Minute}, nil
			},
			"beat": func(ctx context.Context, tc TaskContext) (Next, error) {
				if fired.Add(1) >= 2 {
					return Next{Outcome: "completed"}, nil
				}
				return Next{Phase: "beat", WakeAt: clock.Add(time.Minute), Repeat: time.Minute}, nil
			},
		},
	}
	reg := TaskRegistry{}
	reg.Register(def)
	task, _ := r.CreateTask(ctx, reg, c.ID, TaskSpec{Kind: "heartbeat"})
	sched := NewTaskScheduler(r, reg)
	sched.CatchUp = CatchUpSkip
	sched.now = func() time.Time { return *clock }
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	stored, _ := r.Task(ctx, task.ID)
	if stored.WakeAt == nil || stored.Repeat != time.Minute {
		t.Fatalf("periodic timer not persisted: %+v", stored)
	}
	// Downtime: ten periods pass before the scheduler runs again.
	*clock = clock.Add(10*time.Minute + time.Second)
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	stored, _ = r.Task(ctx, task.ID)
	if fired.Load() != 1 {
		t.Fatalf("catch-up fired %d times", fired.Load())
	}
	var saw bool
	for _, n := range stored.Notices {
		if n == "timer catch-up: 9 overdue occurrence(s) coalesced into one" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("coalescing not recorded: %v", stored.Notices)
	}
	*clock = stored.WakeAt.Add(time.Second)
	if err := runUntilIdle(ctx, sched); err != nil {
		t.Fatal(err)
	}
	final, _ := r.Task(ctx, task.ID)
	if final.Outcome != "completed" || fired.Load() != 2 {
		t.Fatalf("final: %+v fired=%d", final, fired.Load())
	}
}
