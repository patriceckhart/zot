package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// effectStarted is the Task.Effect value committed by StartEffect.
const effectStarted = "started"

// TaskTx is the task-scoped transaction of one commit. It is built against
// the snapshot the commit is fenced on: the task revision, the invocation
// identity, and the store's writer epoch. Everything added through it
// commits atomically with the task record, or not at all.
type TaskTx struct {
	Snapshot storage.Snapshot
	task     Task
	s        *TaskScheduler
	ops      []storage.Operation
	created  map[string]bool
}

// View is the snapshot with this transaction's operations applied, so later
// reads in the same commit see earlier writes.
func (tx *TaskTx) View() storage.Snapshot { return pendingSnapshot(tx.Snapshot, tx.ops) }

// declare marks a task created by raw operations in this commit, so the
// returned Next may wait on or link it.
func (tx *TaskTx) declare(id string) {
	if tx.created == nil {
		tx.created = map[string]bool{}
	}
	tx.created[id] = true
}

// Task is the committed task this transaction updates.
func (tx *TaskTx) Task() Task { return tx.task }

// Put writes a record in the same commit.
func (tx *TaskTx) Put(key string, value any) { tx.ops = append(tx.ops, record(key, value)) }

// Delete removes a record in the same commit.
func (tx *TaskTx) Delete(key string) {
	tx.ops = append(tx.ops, storage.Operation{Key: key, Delete: true})
}

// Ops appends raw operations to the commit.
func (tx *TaskTx) Ops(ops ...storage.Operation) { tx.ops = append(tx.ops, ops...) }

// CreateChild creates a task owned by this task in the same commit and
// returns its ID, which the returned Next may wait on. A child created while
// abort is requested carries the abort mark from the start, so the abort
// survives the commit that created the work.
func (tx *TaskTx) CreateChild(spec TaskSpec) (string, error) {
	if spec.ID == "" {
		spec.ID = uuid.NewString()
	}
	child, ops, err := buildTask(tx.s.registry, tx.task.ConversationID, tx.task.ID, spec, tx.Snapshot.Revision()+1)
	if err != nil {
		return "", fmt.Errorf("create child: %w", err)
	}
	if tx.task.AbortRequested {
		child.AbortRequested = true
		ops[0] = record(taskKey(child.ID), child)
	}
	if tx.created == nil {
		tx.created = map[string]bool{}
	}
	tx.created[child.ID] = true
	tx.ops = append(tx.ops, ops...)
	return child.ID, nil
}

// StartEffect commits the intent of the phase's external effect before the
// handler performs it: the task records that its effect started, with an
// optional replacement checkpoint and operations, in one commit. After a
// crash the next invocation sees TaskContext.Interrupted and must not assume
// the effect did not happen.
//
// The commit is fenced on the current invocation and refused with
// ErrTaskAborting when abort was requested, so an effect never starts after
// a committed abort. A stale invocation gets ErrStaleInvocation.
func (tc TaskContext) StartEffect(ctx context.Context, checkpoint any, build func(tx *TaskTx) error) error {
	return tc.commitScoped(ctx, "task.effect", func(tx *TaskTx, t *Task) error {
		if t.AbortRequested && t.Phase != "__complete__" {
			return ErrTaskAborting
		}
		if checkpoint != nil {
			raw, err := json.Marshal(checkpoint)
			if err != nil {
				return err
			}
			t.Checkpoint = raw
		}
		t.Effect = effectStarted
		if build != nil {
			return build(tx)
		}
		return nil
	})
}

// Commit writes task-scoped records from inside a handler without settling
// the phase, for example bounded progress. It is fenced like StartEffect but
// is allowed while abort is requested.
func (tc TaskContext) Commit(ctx context.Context, build func(tx *TaskTx) error) error {
	return tc.commitScoped(ctx, "task.progress", func(tx *TaskTx, t *Task) error { return build(tx) })
}

func (tc TaskContext) commitScoped(ctx context.Context, actor string, build func(tx *TaskTx, t *Task) error) error {
	inv := tc.inv
	if inv == nil {
		return fmt.Errorf("task-scoped commit outside a scheduler invocation")
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	for attempt := 0; attempt < 64; attempt++ {
		snap, err := inv.s.r.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		current, ok, err := read[Task](snap, taskKey(inv.task.ID))
		if err != nil {
			return err
		}
		if !ok || current.Invocation != inv.task.Invocation || current.State == "terminal" {
			return ErrStaleInvocation
		}
		if current.Revision != inv.task.Revision {
			// Only an abort mark may change the task under a live
			// invocation. Anything else means another writer owns it.
			expected := inv.task
			expected.AbortRequested, expected.Revision = current.AbortRequested, current.Revision
			if !sameJSON(expected, current) {
				return ErrStaleInvocation
			}
		}
		t := current
		tx := &TaskTx{Snapshot: snap, task: current, s: inv.s}
		if err := build(tx, &t); err != nil {
			return err
		}
		if len(tx.created) > 0 {
			return fmt.Errorf("children must be created in the phase's final commit")
		}
		t.Revision = snap.Revision() + 1
		err = inv.s.r.commit(ctx, snap, actor, append(tx.ops, record(taskKey(t.ID), t))...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		inv.task = t
		return nil
	}
	return fmt.Errorf("%w: task-scoped commit kept conflicting", storage.ErrConflict)
}
