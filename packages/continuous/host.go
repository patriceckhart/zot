package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
)

// Host runs the store's work independently of connected clients. It owns one
// Service and a scheduler loop that steps every conversation with queued or
// interrupted work. Clients attach to observe and steer through committed
// state. The host is single-process; the writer lock keeps it exclusive.
type Host struct {
	r      *Runtime
	svc    *Service
	tasks  *TaskScheduler
	events *eventFanout

	mu       sync.Mutex
	stepping map[string]bool
	settled  map[string]uint64
	blocked  map[string]bool
	wake     chan struct{}
	closed   bool
	policy   RecoveryPolicy
	started  time.Time
	metrics  HostMetrics
	notifier Notifier
}

// HostMetrics are operational counters: IDs and transitions, never content.
type HostMetrics struct {
	Started        time.Time `json:"started"`
	Generation     uint64    `json:"generation"`
	StepsStarted   uint64    `json:"steps_started"`
	StepsFailed    uint64    `json:"steps_failed"`
	ActiveSteps    int       `json:"active_steps"`
	BlockedRuns    int       `json:"blocked_runs"`
	TasksInFlight  int       `json:"tasks_in_flight"`
	Watchers       int       `json:"watchers"`
	QueuedInputs   int       `json:"queued_inputs"`
	ActiveRuns     int       `json:"active_runs"`
	PendingApprove int       `json:"pending_approvals"`
	Revision       uint64    `json:"revision"`
	WriterEpoch    uint64    `json:"writer_epoch"`
	// Compaction reports background summaries started, finished, waiting
	// for publication, and discarded as stale.
	Compaction CompactionMetrics `json:"compaction"`
}

// HostOptions configures a Host.
type HostOptions struct {
	Execution ExecutionOptions
	// Tasks is the task registry for the scheduler. Nil disables tasks.
	Tasks TaskRegistry
	// Policy decides which recovery actions run automatically on start.
	// Default: automatic actions only; blocked actions wait for a human.
	Policy RecoveryPolicy
	// Notifier delivers outbox notifications (pending approvals, blocked
	// recovery, blocked cleanup). Nil leaves them in the outbox for a
	// client to read through the protocol.
	Notifier Notifier
}

// RecoveryPolicy selects how a host treats interrupted runs at startup.
type RecoveryPolicy string

const (
	// RecoverSafe resumes automatic actions and leaves blocked runs alone
	// until a human steps them.
	RecoverSafe RecoveryPolicy = "safe"
	// RecoverAll resumes everything, reporting interrupted unsafe tools to
	// the model as errors. Nothing is replayed that the policy forbids; this
	// only removes the human pause.
	RecoverAll RecoveryPolicy = "all"
	// RecoverNone performs no recovery; only new submissions run.
	RecoverNone RecoveryPolicy = "none"
)

// NewHost attaches a service to a runtime. Call Run to start work.
func NewHost(r *Runtime, engine Engine, opts HostOptions) (*Host, error) {
	h := &Host{r: r, stepping: map[string]bool{}, settled: map[string]uint64{}, blocked: map[string]bool{}, wake: make(chan struct{}, 1), events: newEventFanout(), started: time.Now().UTC(), notifier: opts.Notifier}
	sink := opts.Execution.Sink
	opts.Execution.Sink = func(ev core.AgentEvent) {
		h.events.publish(ev)
		if sink != nil {
			sink(ev)
		}
	}
	svc, err := NewService(r, engine, opts.Execution)
	if err != nil {
		return nil, err
	}
	h.svc = svc
	if opts.Tasks != nil {
		h.tasks = NewTaskScheduler(r, opts.Tasks)
	}
	if opts.Policy == "" {
		opts.Policy = RecoverSafe
	}
	h.policy = opts.Policy
	return h, nil
}

// Service exposes the host's service for tools that step owned conversations.
func (h *Host) Service() *Service { return h.svc }

// Runtime exposes the host's runtime.
func (h *Host) Runtime() *Runtime { return h.r }

// Run performs recovery according to the policy, then loops: step every
// conversation with work, run tasks, and sleep until a commit or timer. It
// returns when ctx ends. Work interrupted by cancellation stays recoverable.
func (h *Host) Run(ctx context.Context) error {
	if err := h.recover(ctx); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snap, err := h.r.Snapshot(ctx)
		if err != nil {
			return err
		}
		busy, err := h.dispatch(ctx, snap)
		if err != nil {
			return err
		}
		var wake time.Time
		if h.tasks != nil {
			n, next, err := h.tasks.Tick(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			if n > 0 || h.tasks.InFlight() > 0 {
				busy = true
			}
			wake = next
		}
		if h.notifier != nil {
			if n, err := h.r.Deliver(ctx, h.notifier); err != nil && !errors.Is(err, context.Canceled) {
				return err
			} else if n > 0 {
				busy = true
			}
			if next, err := h.r.NextDelivery(ctx); err == nil && !next.IsZero() && (wake.IsZero() || next.Before(wake)) {
				wake = next
			}
		}
		if busy {
			continue
		}
		if err := h.sleep(ctx, snap.Revision(), wake); err != nil {
			return err
		}
	}
}

func (h *Host) sleep(ctx context.Context, revision uint64, wake time.Time) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waitErr := make(chan error, 1)
	go func() { waitErr <- h.r.Wait(waitCtx, revision) }()
	var timer <-chan time.Time
	if !wake.IsZero() {
		d := time.Until(wake)
		if d <= 0 {
			return nil
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.wake:
		return nil
	case <-timer:
		return nil
	case err := <-waitErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
}

// dispatch starts a step for every conversation that has queued submissions
// or an unfinished run and is not already being stepped. Steps run in their
// own goroutines so conversations progress independently. It reports whether
// any step was started or is in flight.
func (h *Host) dispatch(ctx context.Context, snap storage.Snapshot) (bool, error) {
	want := map[string]bool{}
	after := ""
	for {
		page, err := snap.Page("queue/", after, 500)
		if err != nil {
			return false, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.Key
			parts := strings.SplitN(strings.TrimPrefix(row.Key, "queue/"), "/", 2)
			if len(parts) == 2 {
				want[parts[0]] = true
			}
		}
	}
	after = ""
	for {
		page, err := snap.Page("run/", after, 500)
		if err != nil {
			return false, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.Key
			var run Run
			if json.Unmarshal(row.Value, &run) != nil {
				return false, storage.ErrCorrupt
			}
			if run.Phase != "done" && h.allowed(run) {
				// A run parked on a human decision is not work until the
				// decision commits, which wakes the host through Wait.
				pending, err := pendingApprovals(snap, run.ConversationID)
				if err != nil {
					return false, err
				}
				if len(pending) == 0 {
					want[run.ConversationID] = true
				}
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false, nil
	}
	started := false
	for id := range want {
		if h.stepping[id] {
			started = true
			continue
		}
		// A step that finished after this snapshot was taken already covered
		// whatever the snapshot shows. Skip it; the next pass sees fresh state.
		if h.settled[id] > snap.Revision() {
			continue
		}
		h.stepping[id] = true
		h.metrics.StepsStarted++
		started = true
		go h.step(ctx, id)
	}
	return started || len(h.stepping) > 0, nil
}

// allowed applies the recovery policy to an interrupted run found at dispatch.
// Runs started by this host are always allowed; the policy gates only runs
// that were interrupted before this host started.
func (h *Host) allowed(run Run) bool {
	h.mu.Lock()
	blocked := h.blocked[run.ID]
	h.mu.Unlock()
	return !blocked
}

func (h *Host) step(ctx context.Context, id string) {
	defer func() {
		// Record the revision this step observed as final so dispatch does
		// not start another step from a stale snapshot.
		var rev uint64
		if snap, err := h.r.Snapshot(context.Background()); err == nil {
			rev = snap.Revision()
		}
		h.mu.Lock()
		delete(h.stepping, id)
		h.settled[id] = rev
		h.mu.Unlock()
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}()
	_, _, err := h.svc.Step(ctx, id)
	if errors.Is(err, core.ErrToolOutcomeUnknown) {
		// Leave the run alone until a reconciliation can answer; the next
		// dispatch would otherwise spin on the same unknown outcome. The
		// recovery policy decides, exactly as for a crash.
		h.mu.Lock()
		if run, ok, _ := h.r.Run(context.Background(), id); ok {
			h.blocked[run.ID] = true
		}
		h.mu.Unlock()
		h.events.publish(HostError{ConversationID: id, Err: err.Error()})
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrAwaitingApproval) {
		h.mu.Lock()
		h.metrics.StepsFailed++
		h.mu.Unlock()
		h.events.publish(HostError{ConversationID: id, Err: err.Error()})
	}
}

// HostError is published when a step fails for a reason other than
// cancellation. It names the conversation, never transcript content.
type HostError struct {
	ConversationID string
	Err            string
}

func (HostError) Type() string { return "host_error" }

// recover applies the policy to runs interrupted before this host started.
func (h *Host) recover(ctx context.Context) error {
	plan, err := h.r.RecoveryPreview(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.blocked = map[string]bool{}
	var ops []storage.Operation
	for _, action := range plan.Actions {
		if action.Action == "approve" {
			// Parked on a persisted approval: dispatch skips it until the
			// decision commits, no policy hold is needed.
			continue
		}
		switch h.policy {
		case RecoverNone:
			h.blocked[action.RunID] = true
		case RecoverSafe:
			if !action.Automatic {
				h.blocked[action.RunID] = true
			}
		}
		if h.blocked[action.RunID] {
			ops = append(ops, outboxOp("recovery-blocked-"+action.RunID, "recovery.blocked", action.ConversationID, "recovery needs a decision: "+action.Action, action))
		}
	}
	if len(ops) > 0 {
		snap, err := h.r.Snapshot(ctx)
		if err != nil {
			return err
		}
		// Notifying about a block is idempotent per run ID; a conflict here
		// only means another writer moved first, the next Run notices again.
		if err := h.r.commit(ctx, snap, "recovery.notify", ops...); err != nil && !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	return nil
}

// Unblock releases a run the recovery policy held back, so the next dispatch
// steps it. A human has looked at the plan and decided.
func (h *Host) Unblock(runID string) {
	h.mu.Lock()
	delete(h.blocked, runID)
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Metrics reports operational counters from one committed snapshot.
func (h *Host) Metrics(ctx context.Context) (HostMetrics, error) {
	snap, err := h.r.Snapshot(ctx)
	if err != nil {
		return HostMetrics{}, err
	}
	h.mu.Lock()
	m := h.metrics
	m.Started = h.started
	m.ActiveSteps = len(h.stepping)
	m.BlockedRuns = len(h.blocked)
	h.mu.Unlock()
	m.Generation = h.svc.Generation()
	m.Compaction = h.svc.CompactionMetrics()
	m.Revision = snap.Revision()
	m.WriterEpoch = h.r.Epoch()
	m.Watchers = h.events.subscribers()
	if h.tasks != nil {
		m.TasksInFlight = h.tasks.InFlight()
	}
	count := func(prefix string) int {
		n, after := 0, ""
		for {
			page, err := snap.Page(prefix, after, 500)
			if err != nil || len(page) == 0 {
				return n
			}
			n += len(page)
			after = page[len(page)-1].Key
		}
	}
	m.QueuedInputs = count("queue/")
	after := ""
	for {
		page, err := snap.Page("run/", after, 500)
		if err != nil || len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.Key
			var run Run
			if json.Unmarshal(row.Value, &run) == nil && run.Phase != "done" {
				m.ActiveRuns++
			}
		}
	}
	after = ""
	for {
		page, err := snap.Page("approval/", after, 500)
		if err != nil || len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.Key
			var a Approval
			if json.Unmarshal(row.Value, &a) == nil && a.State == approvalPending {
				m.PendingApprove++
			}
		}
	}
	return m, nil
}

// Reload publishes a new engine generation; see Service.Reload.
func (h *Host) Reload(ctx context.Context, engine Engine) (uint64, error) {
	return h.svc.Reload(ctx, engine)
}

// Blocked lists run IDs held back by the recovery policy.
func (h *Host) Blocked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.blocked))
	for id := range h.blocked {
		out = append(out, id)
	}
	return out
}

// Nudge wakes the loop, for example after a client submission.
func (h *Host) Nudge() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Events returns a bounded subscription to engine events. Slow consumers are
// dropped with a gap marker instead of accumulating unbounded memory.
func (h *Host) Events(buffer int) (<-chan core.AgentEvent, func()) {
	return h.events.subscribe(buffer)
}

// eventFanout delivers engine events to subscribers with per-subscriber
// bounded buffers. A full buffer drops the event and records a gap.
type eventFanout struct {
	mu   sync.Mutex
	subs map[int]*eventSub
	next int
}

type eventSub struct {
	ch      chan core.AgentEvent
	dropped int
}

// EvGap tells a subscriber that events were dropped because it fell behind.
type EvGap struct{ Dropped int }

func (EvGap) Type() string { return "gap" }

func newEventFanout() *eventFanout { return &eventFanout{subs: map[int]*eventSub{}} }

func (f *eventFanout) subscribe(buffer int) (<-chan core.AgentEvent, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	f.mu.Lock()
	id := f.next
	f.next++
	sub := &eventSub{ch: make(chan core.AgentEvent, buffer)}
	f.subs[id] = sub
	f.mu.Unlock()
	return sub.ch, func() {
		f.mu.Lock()
		if s, ok := f.subs[id]; ok {
			delete(f.subs, id)
			close(s.ch)
		}
		f.mu.Unlock()
	}
}

func (f *eventFanout) subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func (f *eventFanout) publish(ev core.AgentEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sub := range f.subs {
		if sub.dropped > 0 {
			select {
			case sub.ch <- EvGap{Dropped: sub.dropped}:
				sub.dropped = 0
			default:
				sub.dropped++
				continue
			}
		}
		select {
		case sub.ch <- ev:
		default:
			sub.dropped++
		}
	}
}

// ConversationSnapshot is what a client needs to render a conversation and
// start watching without a gap: the committed state and the revision it was
// read at.
type ConversationSnapshot struct {
	Revision     uint64       `json:"revision"`
	Conversation Conversation `json:"conversation"`
	Entries      []Entry      `json:"entries"`
	Queue        []Submission `json:"queue"`
	Run          *Run         `json:"run,omitempty"`
	Usage        UsageTotals  `json:"usage"`
	Tasks        []Task       `json:"tasks,omitempty"`
	Approvals    []Approval   `json:"approvals,omitempty"`
	// Partial is the streamed output of the in-flight attempt, when any.
	Partial  *Partial        `json:"partial,omitempty"`
	Recovery *RecoveryAction `json:"recovery,omitempty"`
	// More is set when Entries was truncated to the newest page. Older
	// entries are fetched by sequence.
	More bool `json:"more,omitempty"`
}

// Snapshot reads one committed view of a conversation. limit bounds the
// number of newest entries returned (1..1000).
func (r *Runtime) ConversationSnapshot(ctx context.Context, id string, limit int) (ConversationSnapshot, error) {
	if limit < 1 || limit > 1000 {
		return ConversationSnapshot{}, fmt.Errorf("snapshot limit must be between 1 and 1000")
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return ConversationSnapshot{}, err
	}
	c, err := conversation(snap, id)
	if err != nil {
		return ConversationSnapshot{}, err
	}
	out := ConversationSnapshot{Revision: snap.Revision(), Conversation: c}
	start := uint64(1)
	if c.EntrySequence > uint64(limit) {
		start = c.EntrySequence - uint64(limit) + 1
		out.More = true
	}
	for seq := start; seq <= c.EntrySequence; seq++ {
		e, ok, err := read[Entry](snap, entryKey(id, seq))
		if err != nil {
			return ConversationSnapshot{}, err
		}
		if !ok {
			return ConversationSnapshot{}, fmt.Errorf("%w: missing entry", storage.ErrCorrupt)
		}
		out.Entries = append(out.Entries, e)
	}
	rows, err := snap.Page("queue/"+id+"/", "", 1000)
	if err != nil {
		return ConversationSnapshot{}, err
	}
	for _, row := range rows {
		var sid string
		if json.Unmarshal(row.Value, &sid) != nil {
			return ConversationSnapshot{}, storage.ErrCorrupt
		}
		s, ok, err := read[Submission](snap, "submission/"+sid)
		if err != nil || !ok {
			return ConversationSnapshot{}, storage.ErrCorrupt
		}
		out.Queue = append(out.Queue, s)
	}
	if out.Approvals, err = pendingApprovals(snap, id); err != nil {
		return ConversationSnapshot{}, err
	}
	if p, ok, err := read[Partial](snap, partialKey(id)); err != nil {
		return ConversationSnapshot{}, err
	} else if ok {
		out.Partial = &p
	}
	if run, ok, err := read[Run](snap, runKey(id)); err != nil {
		return ConversationSnapshot{}, err
	} else if ok {
		out.Run = &run
		if run.Phase != "done" {
			a := planRun(run, len(out.Approvals) > 0)
			out.Recovery = &a
		}
	}
	if out.Usage, err = usageTotals(ctx, snap, id); err != nil {
		return ConversationSnapshot{}, err
	}
	taskRows, err := snap.Page("task-conversation/"+id+"/", "", 1000)
	if err != nil {
		return ConversationSnapshot{}, err
	}
	for _, row := range taskRows {
		var tid string
		if json.Unmarshal(row.Value, &tid) != nil {
			return ConversationSnapshot{}, storage.ErrCorrupt
		}
		t, ok, err := read[Task](snap, taskKey(tid))
		if err != nil || !ok {
			return ConversationSnapshot{}, storage.ErrCorrupt
		}
		if t.State != "terminal" {
			out.Tasks = append(out.Tasks, t)
		}
	}
	return out, nil
}

// Watch delivers commits after a revision that touch the conversation, in
// order, until ctx ends. A client applies them on top of its snapshot. The
// returned cursor is the last delivered revision. A cursor below the retained
// history yields storage.ErrCursor so the client resnapshots.
func (r *Runtime) Watch(ctx context.Context, conversationID string, after uint64, visit func(storage.Commit) error) error {
	cursor := after
	prefixes := []string{"conversation/" + conversationID, "entry/" + conversationID + "/", "queue/" + conversationID + "/", runKey(conversationID), "usage/" + conversationID + "/", "task-conversation/" + conversationID + "/", "doc/conversation/" + conversationID + "/", approvalConversationKey(conversationID, ""), partialKey(conversationID)}
	for {
		commits, err := r.Scan(ctx, cursor, 100)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			if err := r.Wait(ctx, cursor); err != nil {
				return err
			}
			continue
		}
		for _, c := range commits {
			cursor = c.Revision
			if !commitTouches(c, prefixes) {
				continue
			}
			if err := visit(c); err != nil {
				return err
			}
		}
	}
}

func commitTouches(c storage.Commit, prefixes []string) bool {
	for _, op := range c.Operations {
		for _, p := range prefixes {
			if strings.HasPrefix(op.Key, p) {
				return true
			}
		}
		if strings.HasPrefix(op.Key, "submission/") || strings.HasPrefix(op.Key, "task/") {
			// Submissions and tasks carry their conversation inside; a cheap
			// substring check avoids decoding every value.
			if strings.Contains(string(op.Value), prefixes[0][len("conversation/"):]) {
				return true
			}
		}
	}
	return false
}
