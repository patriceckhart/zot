package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Task is a persisted state machine. Each phase handler performs at most one
// external effect between two commits. The checkpoint is replaced whole on
// every commit; there is no saved stack. After a crash the task is found in
// its last committed state and the phase handler runs again, so handlers must
// be idempotent with respect to their checkpoint or record intent first.
type Task struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	// Kind names the TaskDefinition. Version pins the checkpoint schema.
	Kind    string          `json:"kind"`
	Version int             `json:"version"`
	Input   json.RawMessage `json:"input,omitempty"`
	// Owner is the owning task ID, empty when the conversation owns it.
	Owner string `json:"owner,omitempty"`
	// Background tasks are excluded from conversation abort and idle.
	Background bool `json:"background"`
	// State is pending, running, waiting, or terminal.
	State      string          `json:"state"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	// Phase is the name of the handler to run next.
	Phase string `json:"phase,omitempty"`
	// WaitOn lists task IDs that must be terminal before this task resumes.
	WaitOn []string `json:"wait_on,omitempty"`
	// WaitPolicy is all or fail_fast.
	WaitPolicy string `json:"wait_policy,omitempty"`
	// WaitApproval parks the task until the named approval is decided or
	// expires. It is the third wait kind next to WaitOn and WakeAt.
	WaitApproval string `json:"wait_approval,omitempty"`
	// Linked are tasks outside this task's ownership tree, usually in an
	// owned conversation, whose abort and settlement this task is
	// responsible for. Abort marks them and the task settles after them.
	Linked []string `json:"linked,omitempty"`
	// WakeAt is an absolute UTC deadline for a persisted timer. Repeat is
	// the period of a periodic timer, zero for one-shot.
	WakeAt *time.Time    `json:"wake_at,omitempty"`
	Repeat time.Duration `json:"repeat,omitempty"`
	// Attempt counts invocations of the current phase.
	Attempt int `json:"attempt"`
	// RetryAt is set while the task waits for its next retry after a
	// handler error. It is a persisted timer like WakeAt.
	RetryAt *time.Time `json:"retry_at,omitempty"`
	// LastError is the error of the most recent failed attempt of the
	// current phase, kept until the phase succeeds.
	LastError string `json:"last_error,omitempty"`
	// Outcome is completed, failed, or aborted once State is terminal.
	Outcome        string          `json:"outcome,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          string          `json:"error,omitempty"`
	AbortRequested bool            `json:"abort_requested,omitempty"`
	// Blocked is set when cleanup failed and the task cannot settle without
	// a human. State stays aborting; the task is not deceptively terminal.
	Blocked string   `json:"blocked,omitempty"`
	Notices []string `json:"notices,omitempty"`
	// Invocation identifies the reservation that currently runs the task.
	// Task-scoped commits from a handler are fenced on it, so a stale
	// invocation can never write after a newer one was reserved.
	Invocation string `json:"invocation,omitempty"`
	// Effect is "started" once a handler committed an effect intent with
	// BeginEffect and the phase has not committed its outcome since. A
	// reservation alone never sets it, so a task found running after a
	// crash distinguishes an effect that may have happened from one that
	// definitely did not start.
	Effect   string    `json:"effect,omitempty"`
	Created  time.Time `json:"created"`
	Revision uint64    `json:"revision"`
}

// RetryPolicy decides whether a handler error schedules another attempt. The
// zero policy never retries: a handler error fails the task.
type RetryPolicy struct {
	// MaxAttempts caps invocations of one phase, including the first.
	MaxAttempts int
	// Backoff is the base delay; attempt n waits Backoff * 2^(n-1), capped
	// by MaxBackoff when set. Jitter adds up to Jitter of random delay.
	Backoff    time.Duration
	MaxBackoff time.Duration
	Jitter     time.Duration
	// Deadline bounds the total elapsed time since the task was created.
	Deadline time.Duration
	// Retryable, when set, classifies errors. Nil retries every error
	// except ErrTaskPermanent.
	Retryable func(error) bool
}

// ErrTaskPermanent wraps handler errors that must not be retried, such as
// invalid credentials, denied permissions, or ambiguous unsafe effects.
var ErrTaskPermanent = errors.New("permanent task error")

// TimerCatchUp selects what happens to timers that were due while no host ran.
type TimerCatchUp string

const (
	// CatchUpFire runs every overdue timer once, in deadline order. Default.
	CatchUpFire TimerCatchUp = "fire"
	// CatchUpSkip advances a repeated timer to its next future deadline
	// without firing the missed occurrences. Non-repeating overdue timers
	// still fire once.
	CatchUpSkip TimerCatchUp = "skip"
)

// Next is what a phase handler commits for itself.
type Next struct {
	// Phase and Checkpoint continue the task. An empty Phase with no Outcome
	// is a programming error and faults the task.
	Phase      string
	Checkpoint any
	// WaitOn parks the task until the listed tasks are terminal.
	WaitOn     []string
	WaitPolicy string
	// WakeAt parks the task until the deadline. Repeat, when positive,
	// marks the timer as periodic so catch-up can coalesce missed
	// occurrences; the phase is still responsible for scheduling the next
	// one.
	WakeAt time.Time
	Repeat time.Duration
	// WaitApproval parks the task until the approval is decided or expires.
	WaitApproval string
	// Link adds tasks to Task.Linked in the same commit.
	Link []string
	// Outcome ends the task: completed, failed, or aborted.
	Outcome string
	Result  any
	Error   string
	// Ops are committed atomically with the task state.
	Ops []storage.Operation
	// Children are created in the same commit, owned by this task.
	Children []TaskSpec
	// Commit, when set, builds the final Next inside the transaction that
	// applies it, against the snapshot the commit is fenced on. It is
	// rebuilt on an unrelated storage conflict as long as the task record
	// is unchanged, so a phase whose effect already happened is never rerun
	// merely because another conversation committed first. Ops and
	// children added through the TaskTx commit with the returned Next.
	Commit func(tx *TaskTx) (Next, error)
	// KeepEffect keeps Task.Effect started across this commit. A phase
	// that could not learn whether its effect happened (a lost remote
	// outcome) parks with it, so the next invocation still sees
	// TaskContext.Interrupted and reconciles instead of assuming.
	KeepEffect bool
	// AfterCommit runs once after the Next committed. Observers that must
	// never see uncommitted state are notified here.
	AfterCommit func()
}

// TaskSpec creates a task.
type TaskSpec struct {
	// ID is optional. TaskTx.CreateChild assigns one so the caller can
	// reference the child (for example in WaitOn) in the same commit.
	ID         string
	Kind       string
	Input      any
	Background bool
}

// TaskContext is what a phase handler may use. All reads are of committed
// state; all writes go through the returned Next.
type TaskContext struct {
	Task     Task
	Snapshot storage.Snapshot
	// Outcomes of the tasks this task waited on, in WaitOn order. Nil unless
	// the task was waiting.
	Waited []Task
	// Interrupted is true when a previous invocation of this phase committed
	// an effect intent (TaskTx.StartEffect) and never settled it. The effect
	// may or may not have happened. A reservation alone never sets it.
	Interrupted bool

	inv *taskInvocation
}

// taskInvocation tracks the committed record an invocation last wrote, so
// task-scoped commits are fenced on it and runOnce continues from it.
type taskInvocation struct {
	s    *TaskScheduler
	mu   sync.Mutex
	task Task
}

func (inv *taskInvocation) current() Task {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.task
}

// TaskDefinition is the code for one task kind. Phases run in the order the
// task's committed state dictates. Abort runs once when the task is aborted
// while pending, running, or waiting, and must return an Outcome.
type TaskDefinition struct {
	Kind    string
	Version int
	// Initial returns the first phase and checkpoint for an input.
	Initial func(input json.RawMessage) (Next, error)
	Phases  map[string]func(ctx context.Context, tc TaskContext) (Next, error)
	Abort   func(ctx context.Context, tc TaskContext) (Next, error)
	// Cleanup runs after the task's outcome is decided as failed or aborted
	// and its children have settled, before the task becomes terminal. It
	// is compensation: durable, retried under Retry, and when it keeps
	// failing the task stays aborting with Blocked set instead of settling.
	Cleanup func(ctx context.Context, tc TaskContext) error
	// Migrate converts a checkpoint from an older version. Nil rejects.
	Migrate func(fromVersion int, phase string, checkpoint json.RawMessage) (string, json.RawMessage, error)
	// Retry governs handler errors of phases and Cleanup.
	Retry RetryPolicy
	// JoinOnAbort keeps a running invocation alive when abort is requested
	// instead of cancelling its context. The abort handler runs after the
	// invocation committed. Use it for phases whose external effect must
	// not be interrupted midway, such as a model request or a tool call.
	JoinOnAbort bool
}

// Registry maps task kinds to definitions. A task whose kind is missing or
// whose version is newer than the registered one is blocked, never dropped.
type TaskRegistry map[string]TaskDefinition

var ErrTaskBlocked = errors.New("continuous task blocked: definition missing or incompatible")

// ErrTaskAborting is returned by StartEffect when the task carries a
// committed abort request. The handler must not start its effect; the
// scheduler runs the abort handler at the next pass.
var ErrTaskAborting = errors.New("continuous task abort requested")

// ErrStaleInvocation is returned by task-scoped commits of an invocation that
// is no longer the one the committed record names.
var ErrStaleInvocation = errors.New("continuous task invocation is stale")
var ErrTaskNotFound = errors.New("continuous task not found")

func taskKey(id string) string { return "task/" + id }
func taskOwnerKey(owner, id string) string {
	return "task-owner/" + owner + "/" + id
}
func taskConversationKey(conversationID, id string) string {
	return "task-conversation/" + conversationID + "/" + id
}

// CreateTask admits a conversation-owned task. The initial checkpoint commits
// with the record so a crash before the first phase still finds a runnable
// task.
func (r *Runtime) CreateTask(ctx context.Context, registry TaskRegistry, conversationID string, spec TaskSpec) (Task, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Task{}, err
		}
		if _, err := conversation(snap, conversationID); err != nil {
			return Task{}, err
		}
		task, ops, err := buildTask(registry, conversationID, "", spec, snap.Revision()+1)
		if err != nil {
			return Task{}, err
		}
		err = r.commit(ctx, snap, "task.create", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return task, err
	}
}

func buildTask(registry TaskRegistry, conversationID, owner string, spec TaskSpec, revision uint64) (Task, []storage.Operation, error) {
	def, ok := registry[spec.Kind]
	if !ok {
		return Task{}, nil, fmt.Errorf("%w: unknown kind", ErrTaskBlocked)
	}
	input, err := json.Marshal(spec.Input)
	if err != nil {
		return Task{}, nil, err
	}
	next, err := def.Initial(input)
	if err != nil {
		return Task{}, nil, err
	}
	if next.Phase == "" || next.Outcome != "" || len(next.Ops) != 0 || len(next.Children) != 0 || len(next.WaitOn) != 0 {
		return Task{}, nil, fmt.Errorf("initial state must name a phase, optionally with a checkpoint and a timer, and nothing else")
	}
	if def.Phases[next.Phase] == nil {
		return Task{}, nil, fmt.Errorf("initial phase %q is not defined", next.Phase)
	}
	checkpoint, err := json.Marshal(next.Checkpoint)
	if err != nil {
		return Task{}, nil, err
	}
	id := spec.ID
	if id == "" {
		id = uuid.NewString()
	}
	task := Task{ID: id, ConversationID: conversationID, Kind: spec.Kind, Version: def.Version, Input: input, Owner: owner, Background: spec.Background, State: "pending", Phase: next.Phase, Checkpoint: checkpoint, Created: time.Now().UTC(), Revision: revision}
	if !next.WakeAt.IsZero() {
		// A task may start parked on a persisted timer (a reminder).
		wake := next.WakeAt.UTC()
		task.State, task.WakeAt, task.Repeat = "waiting", &wake, next.Repeat
	}
	ops := []storage.Operation{record(taskKey(task.ID), task), record(taskConversationKey(conversationID, task.ID), task.ID)}
	if owner != "" {
		ops = append(ops, record(taskOwnerKey(owner, task.ID), task.ID))
	}
	return task, ops, nil
}

// Task reads a committed task.
func (r *Runtime) Task(ctx context.Context, id string) (Task, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Task{}, err
	}
	t, ok, err := read[Task](snap, taskKey(id))
	if err != nil {
		return Task{}, err
	}
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	return t, nil
}

// WaitTask blocks until the task is terminal or ctx ends.
func (r *Runtime) WaitTask(ctx context.Context, id string) (Task, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Task{}, err
		}
		t, ok, err := read[Task](snap, taskKey(id))
		if err != nil {
			return Task{}, err
		}
		if !ok {
			return Task{}, ErrTaskNotFound
		}
		if t.State == "terminal" {
			return t, nil
		}
		if err := r.store.Wait(ctx, snap.Revision()); err != nil {
			return t, err
		}
	}
}

// AbortTask records abort intent for a task and, bottom-up, for every task it
// owns. Background children are skipped unless includeBackground is set. The
// scheduler runs abort handlers at the next boundary; children settle before
// their owner.
func (r *Runtime) AbortTask(ctx context.Context, id string, includeBackground bool) error {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		var ops []storage.Operation
		var mark func(id string) error
		seen := map[string]bool{}
		mark = func(id string) error {
			if seen[id] {
				return fmt.Errorf("%w: ownership cycle", storage.ErrCorrupt)
			}
			seen[id] = true
			t, ok, err := read[Task](snap, taskKey(id))
			if err != nil {
				return err
			}
			if !ok {
				return ErrTaskNotFound
			}
			if t.State == "terminal" {
				return nil
			}
			children, err := snap.Page("task-owner/"+id+"/", "", 1000)
			if err != nil {
				return err
			}
			for _, row := range children {
				var childID string
				if err := json.Unmarshal(row.Value, &childID); err != nil {
					return storage.ErrCorrupt
				}
				child, ok, err := read[Task](snap, taskKey(childID))
				if err != nil || !ok {
					return fmt.Errorf("%w: owned task missing", storage.ErrCorrupt)
				}
				if child.Background && !includeBackground {
					continue
				}
				if err := mark(childID); err != nil {
					return err
				}
			}
			for _, linked := range t.Linked {
				if err := mark(linked); err != nil && !errors.Is(err, ErrTaskNotFound) {
					return err
				}
			}
			if !t.AbortRequested {
				t.AbortRequested = true
				t.Revision = snap.Revision() + 1
				ops = append(ops, record(taskKey(t.ID), t))
			}
			return nil
		}
		if err := mark(id); err != nil {
			return err
		}
		if len(ops) == 0 {
			return nil
		}
		err = r.commit(ctx, snap, "task.abort.request", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return err
	}
}

// Tasks lists the tasks of a conversation at one snapshot, in creation order.
func (r *Runtime) Tasks(ctx context.Context, conversationID string) ([]Task, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := snap.Page("task-conversation/"+conversationID+"/", "", 1000)
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(rows))
	for _, row := range rows {
		var id string
		if err := json.Unmarshal(row.Value, &id); err != nil {
			return nil, storage.ErrCorrupt
		}
		t, ok, err := read[Task](snap, taskKey(id))
		if err != nil || !ok {
			return nil, fmt.Errorf("%w: listed task missing", storage.ErrCorrupt)
		}
		out = append(out, t)
	}
	sortTasks(out)
	return out, nil
}

func sortTasks(tasks []Task) {
	for i := 1; i < len(tasks); i++ {
		for j := i; j > 0 && (tasks[j].Created.Before(tasks[j-1].Created) || tasks[j].Created.Equal(tasks[j-1].Created) && tasks[j].ID < tasks[j-1].ID); j-- {
			tasks[j], tasks[j-1] = tasks[j-1], tasks[j]
		}
	}
}

// TaskScheduler runs tasks of one store. It is single-process. Handlers run
// concurrently, one goroutine per task invocation, bounded by Workers. Each
// invocation is reserved by a commit before it starts, so two schedulers on
// the same store (impossible with the writer lock) or two passes of this one
// cannot both run the same invocation.
type TaskScheduler struct {
	r        *Runtime
	registry TaskRegistry
	now      func() time.Time
	// Workers bounds concurrent handler invocations. Zero means 8.
	Workers int
	// CatchUp decides how overdue timers behave after downtime.
	CatchUp TimerCatchUp
	// Filter, when set, restricts the scheduler to the tasks it accepts.
	// Two schedulers on one store are safe, since every invocation is
	// reserved by a fenced commit, but a filter keeps them apart.
	Filter func(storage.Snapshot, Task) bool
	// Prepare runs at the start of every Tick, before tasks are selected.
	// It returns how many commits it made. The built-in executor uses
	// it to admit queued inputs into new generation chains.
	Prepare func(ctx context.Context) (int, error)
	// Hold, when set, keeps a task from being dispatched, for example a
	// task the recovery policy holds for a human. Abort marks are still
	// committed; the task runs once Hold releases it.
	Hold func(Task) bool

	mu     sync.Mutex
	active map[string]context.CancelFunc
	// aborting marks invocations that already run the abort handler; they
	// are never cancelled by a later abort mark.
	aborting map[string]bool
	done     chan taskResult
	running  sync.WaitGroup
}

func NewTaskScheduler(r *Runtime, registry TaskRegistry) *TaskScheduler {
	return &TaskScheduler{r: r, registry: registry, now: func() time.Time { return time.Now().UTC() }, active: map[string]context.CancelFunc{}, aborting: map[string]bool{}}
}

// Tick dispatches every runnable task that is not already running in this
// process, up to Workers in flight, collects results of invocations that have
// finished, and returns without waiting for long-running handlers. It returns
// the number of invocations that finished since the previous Tick and the
// earliest future wake time (zero when none). Callers that want to block use
// Run or WaitProgress.
func (s *TaskScheduler) Tick(ctx context.Context) (int, time.Time, error) {
	workers := s.Workers
	if workers <= 0 {
		workers = 8
	}
	s.mu.Lock()
	if s.done == nil {
		s.done = make(chan taskResult, 1024)
	}
	done := s.done
	s.mu.Unlock()
	if err := s.cancelAborting(ctx); err != nil {
		return 0, time.Time{}, err
	}
	prepared := 0
	if s.Prepare != nil {
		n, err := s.Prepare(ctx)
		if err != nil {
			return 0, time.Time{}, err
		}
		prepared = n
	}
	var wake time.Time
	for {
		if err := ctx.Err(); err != nil {
			break
		}
		s.mu.Lock()
		inflight := len(s.active)
		s.mu.Unlock()
		if inflight >= workers {
			break
		}
		snap, err := s.r.store.Snapshot(ctx)
		if err != nil {
			return 0, wake, err
		}
		runnable, nextWake, err := s.selectRunnable(ctx, snap)
		if err != nil {
			return 0, wake, err
		}
		if !nextWake.IsZero() && (wake.IsZero() || nextWake.Before(wake)) {
			wake = nextWake
		}
		if runnable.ID == "" {
			break
		}
		if _, taken := s.r.claims.LoadOrStore(runnable.ID, s); taken {
			// Another scheduler of this process claimed it between
			// selection and here; the next pass skips it.
			continue
		}
		invCtx, cancelInv := context.WithCancel(ctx)
		s.mu.Lock()
		s.active[runnable.ID] = cancelInv
		// aborting marks invocations a later abort mark must not cancel:
		// abort handlers themselves, and definitions that join on abort.
		def, _ := s.definition(runnable)
		s.aborting[runnable.ID] = (runnable.AbortRequested && runnable.Phase != "__complete__") || def.JoinOnAbort
		s.running.Add(1)
		s.mu.Unlock()
		go func(t Task) {
			defer s.running.Done()
			err := s.runOnce(invCtx, t)
			cancelInv()
			s.r.claims.Delete(t.ID)
			s.mu.Lock()
			delete(s.active, t.ID)
			delete(s.aborting, t.ID)
			s.mu.Unlock()
			done <- taskResult{id: t.ID, err: err}
		}(runnable)
	}
	progressed := 0
	var firstErr error
	for {
		select {
		case res := <-done:
			switch {
			case res.err == nil:
				progressed++
			case errors.Is(res.err, storage.ErrConflict), errors.Is(res.err, context.Canceled), errors.Is(res.err, context.DeadlineExceeded):
				// Lost a race or was cancelled; the next pass retries from
				// committed state. Count it so Run reselects immediately.
				if ctx.Err() == nil {
					progressed++
				}
			case firstErr == nil:
				firstErr = res.err
			}
			continue
		default:
		}
		break
	}
	return progressed + prepared, wake, firstErr
}

type taskResult struct {
	id  string
	err error
}

// Join waits until every invocation this scheduler started has returned, or
// ctx ends. Cancel the context passed to Tick or Run first; Join is how a
// host makes sure no handler still writes when it closes the store.
func (s *TaskScheduler) Join(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.running.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InFlight reports how many handler invocations this scheduler is running.
func (s *TaskScheduler) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// Run keeps ticking until ctx ends or no task can progress, nothing is in
// flight, and no timer is pending. Between passes it sleeps until an
// invocation finishes, a commit lands, or the earliest timer fires.
func (s *TaskScheduler) Run(ctx context.Context) error {
	for {
		n, wake, err := s.Tick(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.InFlight() == 0 && wake.IsZero() {
			return nil
		}
		if err := s.waitProgress(ctx, wake); err != nil {
			return err
		}
	}
}

// waitProgress blocks until an invocation finishes, a commit lands, the
// timer fires, or ctx ends. Finished results stay queued for the next Tick.
func (s *TaskScheduler) waitProgress(ctx context.Context, wake time.Time) error {
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	changed := make(chan error, 1)
	store, revision := s.r.store, snap.Revision()
	go func() { changed <- store.Wait(waitCtx, revision) }()
	var timer <-chan time.Time
	if !wake.IsZero() {
		d := wake.Sub(s.now())
		if d <= 0 {
			return nil
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	// A cancelled invocation whose task is marked for abort is immediately
	// runnable again (its abort handler), so do not sleep if any is pending.
	select {
	case res := <-done:
		done <- res
		return nil
	default:
	}
	select {
	case res := <-done:
		// Put it back for Tick to account. The channel is buffered and we
		// just took one slot, so this never blocks.
		done <- res
		return nil
	case <-timer:
		return nil
	case err := <-changed:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cancelAborting cancels in-flight invocations of tasks whose committed
// record now carries an abort mark, so their abort handlers can run at the
// next pass. It reads only the active tasks, not the whole table.
func (s *TaskScheduler) cancelAborting(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.active))
	for id := range s.active {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		t, ok, err := read[Task](snap, taskKey(id))
		if err != nil {
			return err
		}
		if ok && t.AbortRequested {
			s.mu.Lock()
			if cancel, active := s.active[id]; active && !s.aborting[id] {
				cancel()
			}
			s.mu.Unlock()
		}
	}
	return nil
}

// selectRunnable finds the first task that can make progress: aborting tasks
// whose children are settled, pending tasks, waiting tasks whose dependencies
// are terminal, and timers that have fired. Blocked tasks are skipped.
func (s *TaskScheduler) selectRunnable(ctx context.Context, snap storage.Snapshot) (Task, time.Time, error) {
	var wake time.Time
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return Task{}, wake, err
		}
		page, err := snap.Page("task/", after, 200)
		if err != nil {
			return Task{}, wake, err
		}
		if len(page) == 0 {
			return Task{}, wake, nil
		}
		for _, row := range page {
			after = row.Key
			var t Task
			if err := json.Unmarshal(row.Value, &t); err != nil {
				return Task{}, wake, storage.ErrCorrupt
			}
			if t.State == "terminal" {
				continue
			}
			if s.Filter != nil && !s.Filter(snap, t) {
				continue
			}
			s.mu.Lock()
			_, active := s.active[t.ID]
			s.mu.Unlock()
			if active {
				continue
			}
			if _, claimed := s.r.claims.Load(t.ID); claimed {
				continue
			}
			if _, blocked := s.definition(t); blocked != nil {
				continue
			}
			if t.Blocked != "" {
				// Blocked cleanup needs a human (Runtime.RetryCleanup).
				continue
			}
			if s.Hold != nil && s.Hold(t) {
				continue
			}
			if t.RetryAt != nil {
				if s.now().Before(*t.RetryAt) {
					if wake.IsZero() || t.RetryAt.Before(wake) {
						wake = *t.RetryAt
					}
					continue
				}
				return t, wake, nil
			}
			if t.AbortRequested {
				settled, err := childrenSettled(snap, t.ID)
				if err != nil {
					return Task{}, wake, err
				}
				if settled {
					return t, wake, nil
				}
				continue
			}
			switch t.State {
			case "pending", "running", "aborting":
				return t, wake, nil
			case "waiting":
				if t.WaitApproval != "" {
					a, ok, err := read[Approval](snap, approvalKey(t.WaitApproval))
					if err != nil {
						return Task{}, wake, err
					}
					// Parked until the decision commits. An approval that
					// expires undecided stays pending until Decide or abort
					// records the expiry, so a human always sees it.
					if !ok || a.State != approvalPending {
						return t, wake, nil
					}
					continue
				}
				if t.WakeAt != nil {
					if !s.now().Before(*t.WakeAt) {
						return t, wake, nil
					}
					if wake.IsZero() || t.WakeAt.Before(wake) {
						wake = *t.WakeAt
					}
					continue
				}
				ready, failed, err := dependenciesReady(snap, t)
				if err != nil {
					return Task{}, wake, err
				}
				if ready {
					return t, wake, nil
				}
				if failed {
					// fail_fast: the first failure requests abort of the
					// remaining awaited tasks this task owns. The wait then
					// completes once they settle, so the continuation sees
					// every outcome and no owned work is left running. Once
					// every unfinished owned dependency is already marked
					// there is nothing to do until they settle.
					needsMark := false
					for _, id := range t.WaitOn {
						dep, ok, err := read[Task](snap, taskKey(id))
						if err != nil {
							return Task{}, wake, err
						}
						if ok && dep.State != "terminal" && dep.Owner == t.ID && !dep.AbortRequested {
							needsMark = true
						}
					}
					if needsMark {
						return t, wake, nil
					}
				}
			}
		}
	}
}

func childrenSettled(snap storage.Snapshot, id string) (bool, error) {
	if t, ok, err := read[Task](snap, taskKey(id)); err != nil {
		return false, err
	} else if ok {
		for _, linked := range t.Linked {
			l, ok, err := read[Task](snap, taskKey(linked))
			if err != nil {
				return false, err
			}
			if ok && l.State != "terminal" {
				return false, nil
			}
		}
	}
	children, err := snap.Page("task-owner/"+id+"/", "", 1000)
	if err != nil {
		return false, err
	}
	for _, row := range children {
		var childID string
		if err := json.Unmarshal(row.Value, &childID); err != nil {
			return false, storage.ErrCorrupt
		}
		child, ok, err := read[Task](snap, taskKey(childID))
		if err != nil || !ok {
			return false, fmt.Errorf("%w: owned task missing", storage.ErrCorrupt)
		}
		if child.State != "terminal" && !child.Background {
			return false, nil
		}
	}
	return true, nil
}

// dependenciesReady reports whether every awaited task is terminal, or, under
// fail_fast, whether any has failed or aborted.
func dependenciesReady(snap storage.Snapshot, t Task) (ready, failed bool, err error) {
	ready = true
	for _, id := range t.WaitOn {
		dep, ok, err := read[Task](snap, taskKey(id))
		if err != nil {
			return false, false, err
		}
		if !ok {
			return false, false, fmt.Errorf("%w: awaited task missing", storage.ErrCorrupt)
		}
		if dep.State != "terminal" {
			ready = false
			continue
		}
		if t.WaitPolicy == "fail_fast" && dep.Outcome != "completed" {
			failed = true
		}
	}
	return ready, failed, nil
}

func (s *TaskScheduler) definition(t Task) (TaskDefinition, error) {
	def, ok := s.registry[t.Kind]
	if !ok {
		return TaskDefinition{}, fmt.Errorf("%w: kind %q not registered", ErrTaskBlocked, t.Kind)
	}
	if t.Version > def.Version {
		return TaskDefinition{}, fmt.Errorf("%w: stored version %d newer than %d", ErrTaskBlocked, t.Version, def.Version)
	}
	if t.Version < def.Version && def.Migrate == nil {
		return TaskDefinition{}, fmt.Errorf("%w: no migration from version %d", ErrTaskBlocked, t.Version)
	}
	return def, nil
}

// runOnce reserves a task (pending or waiting to running, with migration if
// needed), runs its phase or abort handler outside the transaction, and
// commits the handler's Next. Each handler invocation is one effect sandwich.
func (s *TaskScheduler) runOnce(ctx context.Context, t Task) error {
	def, err := s.definition(t)
	if err != nil {
		return err
	}
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	current, ok, err := read[Task](snap, taskKey(t.ID))
	if err != nil {
		return err
	}
	if !ok || current.Revision != t.Revision {
		return storage.ErrConflict
	}
	t = current
	// Reservation: migrate, mark running, bump attempt. One commit.
	if t.Version < def.Version {
		phase, checkpoint, err := def.Migrate(t.Version, t.Phase, t.Checkpoint)
		if err != nil {
			return fmt.Errorf("%w: migration failed: %v", ErrTaskBlocked, err)
		}
		t.Phase, t.Checkpoint, t.Version = phase, checkpoint, def.Version
		t.Notices = append(t.Notices, fmt.Sprintf("migrated checkpoint to version %d", def.Version))
	}
	var waited []Task
	if t.State == "waiting" && t.WakeAt == nil && t.WaitApproval == "" {
		var pendingOps []storage.Operation
		allTerminal := true
		for _, id := range t.WaitOn {
			dep, _, err := read[Task](snap, taskKey(id))
			if err != nil {
				return err
			}
			waited = append(waited, dep)
			if dep.State != "terminal" {
				allTerminal = false
				if dep.Owner == t.ID && !dep.AbortRequested {
					pendingOps = append(pendingOps, abortMarks(snap, dep)...)
				}
			}
		}
		if !allTerminal {
			// fail_fast woke us early. Mark the unfinished owned siblings for
			// abort and keep waiting; the phase runs once they settle.
			if len(pendingOps) == 0 {
				return storage.ErrConflict
			}
			t.Notices = append(t.Notices, "fail_fast: aborting remaining owned tasks")
			t.Revision = snap.Revision() + 1
			return s.r.commit(ctx, snap, "task.failfast", append(pendingOps, record(taskKey(t.ID), t))...)
		}
	}
	// Overdue periodic timer under the skip policy: fire once now, but note
	// how many occurrences were coalesced so the phase can decide.
	if t.WakeAt != nil && t.Repeat > 0 && s.CatchUp == CatchUpSkip {
		if missed := int(s.now().Sub(*t.WakeAt) / t.Repeat); missed > 0 {
			t.Notices = append(t.Notices, fmt.Sprintf("timer catch-up: %d overdue occurrence(s) coalesced into one", missed))
		}
	}
	cleanup := t.State == "aborting"
	if t.State != "running" && !cleanup {
		t.State = "running"
	}
	t.Attempt++
	t.WaitOn, t.WaitPolicy, t.WakeAt, t.Repeat, t.RetryAt, t.WaitApproval = nil, "", nil, 0, nil, ""
	// A new invocation identity fences task-scoped commits of any earlier
	// invocation. Effect is kept: it records that an earlier invocation may
	// have started its effect, which the reservation does not change.
	t.Invocation = uuid.NewString()
	t.Revision = snap.Revision() + 1
	if err := s.r.commit(ctx, snap, "task.reserve", record(taskKey(t.ID), t)); err != nil {
		return err
	}
	snap, err = s.r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	inv := &taskInvocation{s: s, task: t}
	tc := TaskContext{Task: t, Snapshot: snap, Waited: waited, Interrupted: t.Effect == effectStarted, inv: inv}
	if cleanup {
		// Compensation after a decided failed or aborted outcome.
		var held heldOutcome
		if err := json.Unmarshal(t.Checkpoint, &held); err != nil || held.Outcome == "" {
			return s.fault(ctx, snap, t, "cleanup without held outcome")
		}
		var cleanupErr error
		if def.Cleanup != nil {
			cleanupErr = def.Cleanup(ctx, tc)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cleanupErr != nil {
			return s.retryOrBlock(ctx, t, def, cleanupErr, true)
		}
		// Cleanup done: settle with the held outcome. Children settled
		// before cleanup started, so no further hold is needed.
		snap, err = s.r.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		current, ok, err := read[Task](snap, taskKey(t.ID))
		if err != nil {
			return err
		}
		if !ok || current.Revision != t.Revision {
			return storage.ErrConflict
		}
		t = current
		t.State, t.Outcome, t.Error, t.Result = "terminal", held.Outcome, held.Error, held.Result
		t.Phase, t.Checkpoint, t.WaitOn, t.WaitPolicy, t.WakeAt, t.RetryAt, t.LastError, t.Blocked = "", nil, nil, "", nil, nil, "", ""
		t.Notices = append(t.Notices, "cleanup completed")
		t.Revision = snap.Revision() + 1
		return s.r.commit(ctx, snap, "task.settle", record(taskKey(t.ID), t))
	}
	var next Next
	var handlerErr error
	if t.AbortRequested && t.Phase != "__complete__" {
		if def.Abort == nil {
			next = Next{Outcome: "aborted", Error: "abort requested"}
		} else {
			next, handlerErr = def.Abort(ctx, tc)
		}
		if handlerErr == nil && next.Outcome == "" && next.Commit == nil {
			handlerErr = fmt.Errorf("abort handler returned no outcome")
		}
		if handlerErr != nil {
			// A failing abort handler still aborts; it must not keep the
			// task alive forever.
			next = Next{Outcome: "aborted", Error: "abort handler failed: " + handlerErr.Error()}
			handlerErr = nil
		}
	} else {
		handler, ok := def.Phases[t.Phase]
		if !ok {
			handlerErr = fmt.Errorf("phase %q not defined", t.Phase)
		} else {
			next, handlerErr = handler(ctx, tc)
		}
	}
	if ctx.Err() != nil {
		// Cancelled mid-effect. Leave the task running; the next pass reruns
		// the phase from its committed checkpoint.
		return ctx.Err()
	}
	// The handler may have committed task-scoped state (StartEffect).
	t = inv.current()
	if errors.Is(handlerErr, ErrTaskAborting) || errors.Is(handlerErr, ErrStaleInvocation) {
		// Nothing started. The next pass runs the abort handler, or the
		// newer invocation owns the task.
		return storage.ErrConflict
	}
	if handlerErr != nil {
		return s.retryOrBlock(ctx, t, def, handlerErr, false)
	}
	return s.commitNext(ctx, t, next)
}

// retryOrBlock applies the definition's retry policy to a handler error. A
// retryable error with budget left schedules RetryAt; otherwise a phase error
// fails the task and a cleanup error blocks it.
func (s *TaskScheduler) retryOrBlock(ctx context.Context, t Task, def TaskDefinition, cause error, cleanup bool) error {
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	current, ok, err := read[Task](snap, taskKey(t.ID))
	if err != nil {
		return err
	}
	if !ok || !sameInvocationState(t, current) {
		return storage.ErrConflict
	}
	t = current
	t.LastError = cause.Error()
	if delay, ok := def.Retry.next(t, cause, s.now()); ok {
		at := s.now().Add(delay)
		t.RetryAt = &at
		t.Notices = append(t.Notices, fmt.Sprintf("attempt %d failed, retry at %s: %v", t.Attempt, at.Format(time.RFC3339), cause))
		t.Revision = snap.Revision() + 1
		return s.r.commit(ctx, snap, "task.retry", record(taskKey(t.ID), t))
	}
	if cleanup {
		t.Blocked = "cleanup failed after " + fmt.Sprint(t.Attempt) + " attempt(s): " + cause.Error()
		t.Notices = append(t.Notices, "cleanup blocked, human intervention required")
		t.Revision = snap.Revision() + 1
		notify := outboxOp("task-blocked-"+t.ID, "task.blocked", t.ConversationID, "task cleanup blocked: "+t.Kind, map[string]any{"task_id": t.ID, "error": cause.Error()})
		return s.r.commit(ctx, snap, "task.blocked", record(taskKey(t.ID), t), notify)
	}
	return s.commitNext(ctx, t, Next{Outcome: "failed", Error: cause.Error()})
}

// next reports whether another attempt is allowed and after what delay.
func (p RetryPolicy) next(t Task, cause error, now time.Time) (time.Duration, bool) {
	if p.MaxAttempts <= 1 || t.Attempt >= p.MaxAttempts {
		return 0, false
	}
	if errors.Is(cause, ErrTaskPermanent) || errors.Is(cause, ErrTaskBlocked) {
		return 0, false
	}
	if p.Retryable != nil && !p.Retryable(cause) {
		return 0, false
	}
	if p.Deadline > 0 && now.Sub(t.Created) >= p.Deadline {
		return 0, false
	}
	delay := p.Backoff
	if delay <= 0 {
		delay = time.Second
	}
	for i := 1; i < t.Attempt && delay < 24*time.Hour; i++ {
		delay *= 2
	}
	if p.MaxBackoff > 0 && delay > p.MaxBackoff {
		delay = p.MaxBackoff
	}
	if p.Jitter > 0 {
		delay += time.Duration(rand.Int64N(int64(p.Jitter)))
	}
	if p.Deadline > 0 && now.Add(delay).Sub(t.Created) > p.Deadline {
		return 0, false
	}
	return delay, true
}

// RetryCleanup clears a blocked task so the scheduler attempts its cleanup
// again, after a human addressed the cause. It is an audited commit.
func (r *Runtime) RetryCleanup(ctx context.Context, id, actor string) (Task, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Task{}, err
		}
		t, ok, err := read[Task](snap, taskKey(id))
		if err != nil {
			return Task{}, err
		}
		if !ok {
			return Task{}, ErrTaskNotFound
		}
		if t.Blocked == "" {
			return t, fmt.Errorf("task is not blocked")
		}
		t.Blocked, t.Attempt = "", 0
		t.Notices = append(t.Notices, "cleanup retry requested by "+actor)
		t.Revision = snap.Revision() + 1
		err = r.commit(ctx, snap, "task.retry-cleanup:"+actor, record(taskKey(t.ID), t))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return t, err
	}
}

// commitNext applies a handler's Next atomically with its ops and children.
// It rejects waits on missing tasks, on itself, or on its owner chain, and
// faults a continuation that names an undefined phase.
func (s *TaskScheduler) commitNext(ctx context.Context, t Task, next Next) error {
	for attempt := 0; ; attempt++ {
		snap, err := s.r.store.Snapshot(ctx)
		if err != nil {
			return err
		}
		current, ok, err := read[Task](snap, taskKey(t.ID))
		if err != nil {
			return err
		}
		if !ok || !sameInvocationState(t, current) {
			return storage.ErrConflict
		}
		n, created := next, map[string]bool(nil)
		if next.Commit != nil {
			tx := &TaskTx{Snapshot: snap, task: current, s: s}
			built, err := next.Commit(tx)
			if err != nil {
				if errors.Is(err, ErrTaskAborting) {
					return storage.ErrConflict
				}
				return s.retryOrBlock(ctx, current, s.registry[current.Kind], err, false)
			}
			if built.Commit != nil {
				return s.fault(ctx, snap, current, "commit builder returned another builder")
			}
			built.Ops = append(tx.ops, built.Ops...)
			if built.AfterCommit == nil {
				built.AfterCommit = next.AfterCommit
			}
			n, created = built, tx.created
		}
		err = s.applyNext(ctx, snap, current, n, created)
		if errors.Is(err, storage.ErrConflict) && next.Commit != nil && attempt < 64 {
			// Another writer committed first. The builder reruns on fresh
			// state as long as this task's record is unchanged.
			continue
		}
		if err == nil && n.AfterCommit != nil {
			n.AfterCommit()
		}
		return err
	}
}

// applyNext commits one Next for the current committed task. created names
// tasks created in this same commit, which may be waited on.
func (s *TaskScheduler) applyNext(ctx context.Context, snap storage.Snapshot, t Task, next Next, created map[string]bool) error {
	var err error
	def := s.registry[t.Kind]
	// Every applied Next settles the invocation's effect intent, unless it
	// explicitly keeps an unresolved effect for reconciliation.
	if !next.KeepEffect || next.Outcome != "" {
		t.Effect = ""
	}
	for _, id := range next.Link {
		if id == t.ID || slicesContains(t.Linked, id) {
			continue
		}
		if _, ok := snap.Get(taskKey(id)); !ok && !created[id] {
			return s.fault(ctx, snap, t, "task links a missing task")
		}
		t.Linked = append(t.Linked, id)
	}
	ops := append([]storage.Operation(nil), next.Ops...)
	for _, spec := range next.Children {
		child, childOps, err := buildTask(s.registry, t.ConversationID, t.ID, spec, snap.Revision()+1)
		if err != nil {
			next = Next{Outcome: "failed", Error: "create child: " + err.Error()}
			ops = nil
			break
		}
		if t.AbortRequested {
			// Abort intent survives the response that created the work.
			child.AbortRequested = true
			childOps[0] = record(taskKey(child.ID), child)
		}
		ops = append(ops, childOps...)
	}
	switch {
	case next.Outcome != "":
		if next.Outcome != "completed" && next.Outcome != "failed" && next.Outcome != "aborted" {
			next = Next{Outcome: "failed", Error: fmt.Sprintf("invalid outcome %q", next.Outcome)}
		}
		// A task whose ordinary children are still live cannot finish yet.
		// Record the outcome intent and let the children settle first.
		settled, err := childrenSettled(snap, t.ID)
		if err != nil {
			return err
		}
		if !settled {
			// Hold the decided outcome until the owned foreground work is
			// terminal. A failed or aborted outcome aborts that work first,
			// bottom-up, in the same commit as the hold.
			held := heldOutcome{Outcome: next.Outcome, Error: next.Error}
			if next.Result != nil {
				held.Result, err = json.Marshal(next.Result)
				if err != nil {
					return err
				}
			}
			t.Notices = append(t.Notices, fmt.Sprintf("%s outcome held until owned tasks settle", next.Outcome))
			t.State, t.WaitPolicy, t.Phase, t.WaitOn = "waiting", "all", "__complete__", nil
			children, err := snap.Page("task-owner/"+t.ID+"/", "", 1000)
			if err != nil {
				return err
			}
			for _, row := range children {
				var id string
				if err := json.Unmarshal(row.Value, &id); err != nil {
					return storage.ErrCorrupt
				}
				child, ok, err := read[Task](snap, taskKey(id))
				if err != nil || !ok {
					return fmt.Errorf("%w: owned task missing", storage.ErrCorrupt)
				}
				if child.Background || child.State == "terminal" {
					continue
				}
				t.WaitOn = append(t.WaitOn, id)
				if next.Outcome != "completed" && !child.AbortRequested {
					ops = append(ops, abortMarks(snap, child)...)
				}
			}
			t.Checkpoint, err = json.Marshal(held)
			if err != nil {
				return err
			}
			t.Revision = snap.Revision() + 1
			return s.r.commit(ctx, snap, "task.hold", mergeOps(append(ops, record(taskKey(t.ID), t)))...)
		}
		// Failed and aborted outcomes run Cleanup first. The held outcome
		// is committed with state aborting so a crash resumes the cleanup
		// instead of forgetting it.
		if def.Cleanup != nil && next.Outcome != "completed" {
			held := heldOutcome{Outcome: next.Outcome, Error: next.Error}
			if next.Result != nil {
				held.Result, err = json.Marshal(next.Result)
				if err != nil {
					return err
				}
			}
			t.State, t.Phase, t.WaitOn, t.WaitPolicy, t.WakeAt, t.Attempt = "aborting", "", nil, "", nil, 0
			t.Checkpoint, _ = json.Marshal(held)
			t.Notices = append(t.Notices, fmt.Sprintf("%s outcome decided, running cleanup", next.Outcome))
			t.Revision = snap.Revision() + 1
			return s.r.commit(ctx, snap, "task.cleanup", mergeOps(append(ops, record(taskKey(t.ID), t)))...)
		}
		t.State, t.Outcome, t.Error = "terminal", next.Outcome, next.Error
		t.Phase, t.Checkpoint, t.WaitOn, t.WaitPolicy, t.WakeAt, t.RetryAt, t.LastError = "", nil, nil, "", nil, nil, ""
		if next.Result != nil {
			t.Result, err = json.Marshal(next.Result)
			if err != nil {
				return err
			}
		}
	case next.WaitApproval != "":
		if next.Phase == "" || def.Phases[next.Phase] == nil {
			return s.fault(ctx, snap, t, "approval wait without a defined continuation phase")
		}
		t.State, t.WaitApproval, t.Phase = "waiting", next.WaitApproval, next.Phase
		t.Checkpoint, err = json.Marshal(next.Checkpoint)
		if err != nil {
			return err
		}
		t.Attempt = 0
	case len(next.WaitOn) > 0:
		for _, id := range next.WaitOn {
			if id == t.ID {
				return s.fault(ctx, snap, t, "task cannot wait on itself")
			}
			if created[id] {
				continue
			}
			dep, ok, err := read[Task](snap, taskKey(id))
			if err != nil {
				return err
			}
			if !ok {
				return s.fault(ctx, snap, t, "task waits on a missing task")
			}
			for owner := t.Owner; owner != ""; {
				if owner == id {
					return s.fault(ctx, snap, t, "task cannot wait on its owner")
				}
				o, ok, err := read[Task](snap, taskKey(owner))
				if err != nil || !ok {
					break
				}
				owner = o.Owner
			}
			_ = dep
		}
		if next.Phase == "" || def.Phases[next.Phase] == nil {
			return s.fault(ctx, snap, t, "wait without a defined continuation phase")
		}
		policy := next.WaitPolicy
		if policy == "" {
			policy = "all"
		}
		if policy != "all" && policy != "fail_fast" {
			return s.fault(ctx, snap, t, "invalid wait policy")
		}
		t.State, t.WaitOn, t.WaitPolicy, t.Phase = "waiting", next.WaitOn, policy, next.Phase
		t.Checkpoint, err = json.Marshal(next.Checkpoint)
		if err != nil {
			return err
		}
		t.Attempt = 0
	case !next.WakeAt.IsZero():
		if next.Phase == "" || def.Phases[next.Phase] == nil {
			return s.fault(ctx, snap, t, "timer without a defined continuation phase")
		}
		wake := next.WakeAt.UTC()
		t.State, t.WakeAt, t.Repeat, t.Phase = "waiting", &wake, next.Repeat, next.Phase
		t.Checkpoint, err = json.Marshal(next.Checkpoint)
		if err != nil {
			return err
		}
		t.Attempt = 0
	case next.Phase != "":
		if def.Phases[next.Phase] == nil {
			return s.fault(ctx, snap, t, fmt.Sprintf("undefined phase %q", next.Phase))
		}
		checkpoint, err := json.Marshal(next.Checkpoint)
		if err != nil {
			return err
		}
		if next.Phase == t.Phase && string(checkpoint) == string(t.Checkpoint) && len(ops) == 0 {
			return s.fault(ctx, snap, t, "phase made no durable progress")
		}
		t.State, t.Phase, t.Checkpoint, t.Attempt = "running", next.Phase, checkpoint, 0
	default:
		return s.fault(ctx, snap, t, "phase returned nothing")
	}
	t.Revision = snap.Revision() + 1
	return s.r.commit(ctx, snap, "task.step", mergeOps(append(ops, record(taskKey(t.ID), t)))...)
}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *TaskScheduler) fault(ctx context.Context, snap storage.Snapshot, t Task, reason string) error {
	t.State, t.Outcome, t.Error = "terminal", "failed", "faulted: "+reason
	t.Phase, t.Checkpoint, t.WaitOn, t.WaitPolicy, t.WakeAt, t.RetryAt, t.Blocked, t.WaitApproval, t.Effect = "", nil, nil, "", nil, nil, "", "", ""
	t.Revision = snap.Revision() + 1
	return s.r.commit(ctx, snap, "task.fault", record(taskKey(t.ID), t))
}

// sameInvocationState reports whether current is the record expected by the
// invocation, allowing only an abort mark committed meanwhile. The abort is
// honoured after the invocation records what already happened.
func sameInvocationState(expected, current Task) bool {
	if current.Revision == expected.Revision {
		return true
	}
	if expected.Invocation == "" || current.Invocation != expected.Invocation {
		return false
	}
	expected.AbortRequested, expected.Revision = current.AbortRequested, current.Revision
	return sameJSON(expected, current)
}

// heldOutcome is the checkpoint of the internal completion phase and of the
// aborting (cleanup) state.
type heldOutcome struct {
	Outcome string          `json:"outcome"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// completeHeld is the internal phase a task enters when its outcome was
// decided before its children settled. It is registered into every
// definition's phase map by the scheduler and replays the held outcome.
func completeHeld(ctx context.Context, tc TaskContext) (Next, error) {
	var held heldOutcome
	if err := json.Unmarshal(tc.Task.Checkpoint, &held); err != nil || held.Outcome == "" {
		return Next{}, fmt.Errorf("held outcome unreadable")
	}
	var result any
	if len(held.Result) > 0 {
		result = held.Result
	}
	return Next{Outcome: held.Outcome, Result: result, Error: held.Error}, nil
}

// abortMarks returns the operations that mark a task and its non-background
// descendants for abort, bottom-up. Terminal tasks are skipped.
func abortMarks(snap storage.Snapshot, t Task) []storage.Operation {
	var ops []storage.Operation
	if t.State == "terminal" {
		return nil
	}
	children, _ := snap.Page("task-owner/"+t.ID+"/", "", 1000)
	for _, row := range children {
		var id string
		if json.Unmarshal(row.Value, &id) != nil {
			continue
		}
		child, ok, err := read[Task](snap, taskKey(id))
		if err != nil || !ok || child.Background || child.AbortRequested {
			continue
		}
		ops = append(ops, abortMarks(snap, child)...)
	}
	for _, id := range t.Linked {
		linked, ok, err := read[Task](snap, taskKey(id))
		if err != nil || !ok || linked.AbortRequested {
			continue
		}
		ops = append(ops, abortMarks(snap, linked)...)
	}
	t.AbortRequested = true
	t.Revision = snap.Revision() + 1
	return append(ops, record(taskKey(t.ID), t))
}

// Register adds a definition and the internal completion phase.
func (reg TaskRegistry) Register(def TaskDefinition) error {
	if strings.TrimSpace(def.Kind) == "" || def.Version < 1 || def.Initial == nil || len(def.Phases) == 0 {
		return fmt.Errorf("task definition requires kind, version, initial, and phases")
	}
	phases := make(map[string]func(context.Context, TaskContext) (Next, error), len(def.Phases)+1)
	for name, fn := range def.Phases {
		if strings.HasPrefix(name, "__") {
			return fmt.Errorf("phase names starting with __ are reserved")
		}
		phases[name] = fn
	}
	phases["__complete__"] = completeHeld
	def.Phases = phases
	reg[def.Kind] = def
	return nil
}
