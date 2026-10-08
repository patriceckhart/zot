package continuous

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Arm the context hook once selection has skipped the active task and found
// no more rows. Its next Err call is Run's check after Tick drains results.
type idleScanStore struct {
	storage.Store
	armed *atomic.Bool
}

func (s idleScanStore) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	snap, err := s.Store.Snapshot(ctx)
	return idleScanSnapshot{Snapshot: snap, armed: s.armed}, err
}

type idleScanSnapshot struct {
	storage.Snapshot
	armed *atomic.Bool
}

func (s idleScanSnapshot) Page(prefix, after string, limit int) ([]storage.Record, error) {
	rows, err := s.Snapshot.Page(prefix, after, limit)
	if err == nil && prefix == "task/" && after != "" && len(rows) == 0 {
		s.armed.Store(true)
	}
	return rows, err
}

type idleCheckContext struct {
	context.Context
	armed *atomic.Bool
	once  sync.Once
	hook  func()
}

func (c *idleCheckContext) Err() error {
	if c.armed.CompareAndSwap(true, false) {
		c.once.Do(c.hook)
	}
	return c.Context.Err()
}

func TestTaskRunCollectsCompletionBeforeDeclaringIdle(t *testing.T) {
	base, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var armed atomic.Bool
	r, conv := newTaskRuntime(t, idleScanStore{Store: newMemoryStore(), armed: &armed})
	defer r.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var effects atomic.Int32
	def := counterDefinition(&effects, 1)
	prepare := def.Phases["prepare"]
	def.Phases["prepare"] = func(ctx context.Context, tc TaskContext) (Next, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return Next{}, ctx.Err()
		}
		return prepare(ctx, tc)
	}
	reg := TaskRegistry{}
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	task, err := r.CreateTask(base, reg, conv.ID, TaskSpec{Kind: def.Kind})
	if err != nil {
		t.Fatal(err)
	}
	sched := NewTaskScheduler(r, reg)
	sched.Workers = 1
	ctx := &idleCheckContext{Context: base, armed: &armed, hook: func() {
		// Finish precisely between Tick's empty result drain and Run's
		// idle check. Completion must cause another selection pass.
		releaseOnce()
		if err := sched.Join(base); err != nil {
			t.Fatal(err)
		}
	}}
	defer func() { cancel(); releaseOnce(); _ = sched.Join(context.Background()) }()
	if _, _, err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-base.Done():
		t.Fatal(base.Err())
	}
	// Leave a free worker so the next Tick scans past the active task.
	sched.Workers = 2
	if err := sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := r.Task(base, task.ID)
	if err != nil || final.State != "terminal" || final.Outcome != "completed" || effects.Load() != 1 {
		t.Fatalf("returned before completion: %+v, %v, effects=%d", final, err, effects.Load())
	}
}

func TestTaskTickReservesCompletionSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, conv := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	var effects atomic.Int32
	def := counterDefinition(&effects, 1)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	prepare := def.Phases["prepare"]
	def.Phases["prepare"] = func(ctx context.Context, tc TaskContext) (Next, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return Next{}, ctx.Err()
		}
		return prepare(ctx, tc)
	}
	reg := TaskRegistry{}
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	task, err := r.CreateTask(ctx, reg, conv.ID, TaskSpec{Kind: def.Kind})
	if err != nil {
		t.Fatal(err)
	}
	sched := NewTaskScheduler(r, reg)
	defer func() { cancel(); releaseOnce(); _ = sched.Join(context.Background()) }()
	// A full result queue must be drained before another invocation is
	// dispatched, so a completing worker never blocks while holding mu.
	sched.done = make(chan taskResult, 1)
	sched.done <- taskResult{}
	if n, _, err := sched.Tick(ctx); err != nil || n != 1 || sched.InFlight() != 0 {
		t.Fatalf("dispatched without a result slot: n=%d, in flight=%d, err=%v", n, sched.InFlight(), err)
	}
	releaseOnce()
	if err := sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := r.Task(ctx, task.ID)
	if err != nil || final.Outcome != "completed" || effects.Load() != 1 {
		t.Fatalf("task after draining results: %+v, %v", final, err)
	}
}
