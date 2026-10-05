package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Engine builds the core agent used for one conversation. It is supplied by
// the host, which owns provider clients, credentials, tool registries, and
// permission checks. The runtime never constructs clients or reads credentials.
// Build must return a fresh agent whose transcript the runtime replaces.
type Engine interface {
	Build(ctx context.Context, conversation Conversation) (*core.Agent, error)
}

// EngineFunc adapts a function to Engine.
type EngineFunc func(ctx context.Context, conversation Conversation) (*core.Agent, error)

func (f EngineFunc) Build(ctx context.Context, c Conversation) (*core.Agent, error) {
	return f(ctx, c)
}

// ExecutionOptions bounds one run. Zero values use conservative defaults.
type ExecutionOptions struct {
	// MaxTurns caps model requests per run. Zero means 64.
	MaxTurns int
	// MaxAttempts caps model request attempts for one turn, including the
	// first. Zero means 3. Retries follow core.RetryableProviderError.
	MaxAttempts int
	// RetryDelay is the base of the exponential backoff. Zero means 2s.
	RetryDelay time.Duration
	// Sink observes engine events for the host. May be nil.
	Sink func(core.AgentEvent)
	// Compaction enables automatic context compaction. Zero disables it.
	Compaction CompactionPolicy
	// Approver, when set, can require a persisted human decision before a
	// tool call's intent commits. Nil never asks.
	Approver Approver
	// PartialFlushInterval batches streamed text into partial records for
	// attached clients. Zero means PartialFlushInterval. Negative disables
	// partial persistence.
	PartialFlushInterval time.Duration
}

func (o ExecutionOptions) normalized() ExecutionOptions {
	if o.MaxTurns <= 0 {
		o.MaxTurns = 64
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.RetryDelay <= 0 {
		o.RetryDelay = 2 * time.Second
	}
	if o.Sink == nil {
		o.Sink = func(core.AgentEvent) {}
	}
	return o
}

// Run is the persisted execution state machine of one conversation. At most
// one run exists per conversation. Its phase is committed before the external
// work of that phase starts, so recovery knows what may have happened.
type Run struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	// Submissions answered by this run, in queue order.
	Submissions []string `json:"submissions"`
	// Phase is request, tools, or done.
	Phase string `json:"phase"`
	// Turn counts model requests started in this run.
	Turn int `json:"turn"`
	// Attempt counts requests started for the current turn.
	Attempt int `json:"attempt"`
	// Cutoff is the entry sequence included in the current request.
	Cutoff uint64 `json:"cutoff"`
	// Tools are the pending or running tool intents of the current round.
	Tools []ToolIntent `json:"tools,omitempty"`
	// Outcome is completed, failed, or aborted once Phase is done.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
	// Notices are recovery decisions for humans, never model-visible.
	Notices []string `json:"notices,omitempty"`
	// AbortRequested is the committed abort intent. The stepper honours it at
	// its next boundary.
	AbortRequested bool `json:"abort_requested,omitempty"`
	// Compacted records that this turn already compacted once, so a second
	// overflow fails instead of looping.
	Compacted bool   `json:"compacted,omitempty"`
	Revision  uint64 `json:"revision"`
}

// ToolIntent is one tool call of the current round. State is pending while
// the call has not started, running once intent was committed, and done once
// its result entry exists.
type ToolIntent struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Args are the effective arguments committed with the intent. The model's
	// original arguments stay in the assistant entry.
	Args   json.RawMessage           `json:"args,omitempty"`
	Replay core.ToolReplayPolicy     `json:"replay,omitempty"`
	State  string                    `json:"state"`
	Entry  uint64                    `json:"entry,omitempty"`
	Result *provider.ToolResultBlock `json:"-"`
	// Approval is the ID of the decision that allowed this call, when one
	// was required.
	Approval string `json:"approval,omitempty"`
}

// Entry types written by execution. Legacy and queued user entries are
// defined in runtime.go and session.go.
const (
	entryAssistant  = "assistant"
	entryToolResult = "tool_result"
	entryAttempt    = "attempt"
	// entrySteer is a steered input placed at the run boundary it joined.
	// The submission's original user entry stays where it was admitted and
	// is excluded from model context in favour of this one.
	entrySteer = "steer"
)

func runKey(conversationID string) string { return "run/" + conversationID }

var ErrBusy = errors.New("continuous conversation has an active run")
var ErrNoEngine = errors.New("continuous runtime has no engine")

// Service runs admitted work. It is single-process and trusted-local. One
// Service per runtime; a second concurrent Service on the same store is
// prevented by the store's writer fence, not by this type.
type Service struct {
	r    *Runtime
	opts ExecutionOptions
	// engine is the current registry generation. Reload replaces it
	// atomically; a request or tool call that already built its agent keeps
	// the generation it started with.
	engine     atomic.Pointer[engineGeneration]
	generation atomic.Uint64
	// compactor holds background summaries until a request boundary
	// publishes them.
	compactor *backgroundCompactor
}

type engineGeneration struct {
	Engine     Engine
	Generation uint64
}

// NewService attaches an engine to a runtime. No goroutine is started.
func NewService(r *Runtime, engine Engine, opts ExecutionOptions) (*Service, error) {
	if r == nil {
		return nil, fmt.Errorf("continuous service requires a runtime")
	}
	if engine == nil {
		return nil, ErrNoEngine
	}
	s := &Service{r: r, opts: opts.normalized(), compactor: newBackgroundCompactor()}
	s.engine.Store(&engineGeneration{Engine: engine, Generation: 1})
	s.generation.Store(1)
	return s, nil
}

// Run reads the committed run of a conversation.
func (r *Runtime) Run(ctx context.Context, conversationID string) (Run, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Run{}, false, err
	}
	return read[Run](snap, runKey(conversationID))
}

// Reload validates a replacement engine and publishes it atomically. The
// engine must build an agent for a probe conversation first; a failing build
// leaves the current generation active and returns the error. Calls that
// already started keep their generation. Returns the new generation number.
func (s *Service) Reload(ctx context.Context, engine Engine) (uint64, error) {
	if engine == nil {
		return s.generation.Load(), ErrNoEngine
	}
	probe, err := engine.Build(ctx, Conversation{ID: "reload-probe", Config: AgentConfig{}})
	if err != nil {
		return s.generation.Load(), fmt.Errorf("reload rejected: %w", err)
	}
	if probe == nil {
		return s.generation.Load(), fmt.Errorf("reload rejected: engine returned no agent")
	}
	gen := s.generation.Add(1)
	s.engine.Store(&engineGeneration{Engine: engine, Generation: gen})
	return gen, nil
}

// Generation is the current engine generation.
func (s *Service) Generation() uint64 { return s.generation.Load() }

// CompactionMetrics reports background compaction activity.
func (s *Service) CompactionMetrics() CompactionMetrics { return s.compactor.metrics() }

// WaitCompactions blocks until in-flight background summaries finish or ctx
// ends. Summaries that finish after the host stops are not published.
func (s *Service) WaitCompactions(ctx context.Context) { s.compactor.wait(ctx) }

func (s *Service) currentEngine() Engine { return s.engine.Load().Engine }

// Step advances one conversation until its queue is empty or ctx ends. It
// first recovers an interrupted run, then answers queued submissions in
// order. It returns the last committed run of the conversation and whether
// this call performed work. Only one Step per conversation may execute at a
// time; a concurrent stepper loses the revision check with ErrBusy.
func (s *Service) Step(ctx context.Context, conversationID string) (Run, bool, error) {
	var last Run
	did := false
	for {
		if err := ctx.Err(); err != nil {
			return last, did, err
		}
		run, ok, err := s.r.Run(ctx, conversationID)
		if err != nil {
			return last, did, err
		}
		if ok {
			last = run
		}
		if ok && run.Phase != "done" {
			run, err = s.drive(ctx, run)
			if err != nil {
				return run, true, err
			}
			last, did = run, true
			continue
		}
		started, ok, err := s.start(ctx, conversationID)
		if err != nil {
			return last, did, err
		}
		if !ok {
			return last, did, nil
		}
		last, did = started, true
	}
}

// start creates a run for the queued submissions of a conversation. The run
// record, the submission state changes, and the queue removal commit together.
func (s *Service) start(ctx context.Context, conversationID string) (Run, bool, error) {
	for {
		snap, err := s.r.store.Snapshot(ctx)
		if err != nil {
			return Run{}, false, err
		}
		c, err := conversation(snap, conversationID)
		if err != nil {
			return Run{}, false, err
		}
		if existing, ok, err := read[Run](snap, runKey(conversationID)); err != nil {
			return Run{}, false, err
		} else if ok && existing.Phase != "done" {
			return existing, true, nil
		}
		queue, err := snap.Page("queue/"+conversationID+"/", "", 100)
		if err != nil {
			return Run{}, false, err
		}
		if len(queue) == 0 {
			return Run{}, false, nil
		}
		run := Run{ID: uuid.NewString(), ConversationID: conversationID, Phase: "request", Turn: 1, Attempt: 1, Cutoff: c.EntrySequence}
		ops := []storage.Operation{}
		for _, row := range queue {
			var id string
			if err := json.Unmarshal(row.Value, &id); err != nil {
				return Run{}, false, storage.ErrCorrupt
			}
			sub, ok, err := read[Submission](snap, "submission/"+id)
			if err != nil || !ok {
				return Run{}, false, fmt.Errorf("%w: queued submission missing", storage.ErrCorrupt)
			}
			sub.State = "running"
			run.Submissions = append(run.Submissions, id)
			ops = append(ops, record("submission/"+id, sub), storage.Operation{Key: row.Key, Delete: true})
		}
		c.Revision = snap.Revision() + 1
		run.Revision = c.Revision
		ops = append(ops, record("conversation/"+conversationID, c), record(runKey(conversationID), run))
		err = s.r.commit(ctx, snap, "run.start", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if err != nil {
			return Run{}, false, err
		}
		return run, true, nil
	}
}

// drive executes the committed phase of a run until the run is done or ctx
// ends. Every external effect sits between two commits.
func (s *Service) drive(ctx context.Context, run Run) (Run, error) {
	for run.Phase != "done" {
		if err := ctx.Err(); err != nil {
			return run, err
		}
		// Reread the committed run so an abort requested by another caller
		// is honoured at this boundary.
		current, ok, err := s.r.Run(ctx, run.ConversationID)
		if err != nil {
			return run, err
		}
		if !ok || current.ID != run.ID {
			return run, fmt.Errorf("%w: run replaced", storage.ErrConflict)
		}
		run = current
		if run.AbortRequested {
			return s.abort(ctx, run)
		}
		switch run.Phase {
		case "request":
			run, err = s.request(ctx, run)
		case "tools":
			run, err = s.tools(ctx, run)
		default:
			return run, fmt.Errorf("%w: unknown run phase", storage.ErrCorrupt)
		}
		if err != nil {
			return run, err
		}
	}
	return run, nil
}

func (s *Service) load(ctx context.Context, run Run) (*core.Agent, Conversation, storage.Snapshot, error) {
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return nil, Conversation{}, nil, err
	}
	c, err := conversation(snap, run.ConversationID)
	if err != nil {
		return nil, Conversation{}, nil, err
	}
	current, ok, err := read[Run](snap, runKey(run.ConversationID))
	if err != nil {
		return nil, Conversation{}, nil, err
	}
	if !ok || current.ID != run.ID || current.Revision != run.Revision {
		return nil, Conversation{}, nil, fmt.Errorf("%w: run changed concurrently", storage.ErrConflict)
	}
	agent, err := s.currentEngine().Build(ctx, c)
	if err != nil {
		return nil, Conversation{}, nil, fmt.Errorf("build engine: %w", err)
	}
	if agent == nil {
		return nil, Conversation{}, nil, fmt.Errorf("engine returned no agent")
	}
	messages, err := ModelContext(ctx, snap, run.ConversationID, run.Cutoff)
	if err != nil {
		return nil, Conversation{}, nil, err
	}
	agent.SetMessages(messages)
	// Per-conversation tool selection: narrow the host registry to what the
	// configuration permits. It can only remove tools, never add.
	if len(c.Config.Tools) > 0 || len(c.Config.DenyTools) > 0 {
		narrowed := core.Registry{}
		for name, tool := range agent.Tools {
			if c.Config.allows(name) {
				narrowed[name] = tool
			}
		}
		agent.SetTools(narrowed)
	}
	if c.Config.Model != "" {
		agent.Model = c.Config.Model
	}
	if c.Config.Reasoning != "" {
		agent.Reasoning = c.Config.Reasoning
	}
	if c.Config.Instructions != "" {
		agent.System = strings.TrimSpace(agent.System + "\n\n" + c.Config.Instructions)
	}
	agent.SessionID = c.ID
	return agent, c, snap, nil
}

// request performs one model attempt. Intent (turn, attempt, cutoff) is already
// committed in the run record. The assembled response and the tool intents
// commit atomically; a failed attempt commits an attempt entry instead.
func (s *Service) request(ctx context.Context, run Run) (Run, error) {
	agent, c, snap, err := s.load(ctx, run)
	if err != nil {
		return run, err
	}
	// Budgets: the request must not start when the scope's known spend plus
	// a conservative reservation exceeds the limit. The run fails with the
	// reason; nothing is sent, so no unknown charge is incurred here.
	if _, err := checkBudgets(ctx, snap, c.ID, time.Now().UTC()); err != nil {
		if !errors.Is(err, ErrBudgetExceeded) {
			return run, err
		}
		run.Phase, run.Outcome, run.Error = "done", "failed", err.Error()
		run.Notices = append(run.Notices, "request refused: "+err.Error())
		return s.commitRun(ctx, snap, c, run, "run.budget", s.settle(snap, run, "failed")...)
	}
	// A background summary that finished while the run continued is
	// published here, at the request boundary, if its source range is still
	// the active context. A stale summary is discarded and counted; the run
	// is not blocked by it.
	if pending := s.compactor.take(c.ID); pending != nil {
		if _, err := s.r.publishCompaction(ctx, pending, s.opts.Sink); err == nil {
			return s.afterCompaction(ctx, run, "background compaction published before the request")
		} else if errors.Is(err, ErrCompactionStale) {
			s.compactor.markStale()
		} else {
			return run, err
		}
	}
	// Threshold compaction: when the context is near the window, summarize
	// first. The compaction commits on its own; the run then reloads so the
	// request includes the summary and the cutoff advances past it.
	if !run.Compacted && needsCompaction(s.opts.Compaction, agent.Messages(), agent.System) {
		if _, err := s.r.compact(ctx, s.currentEngine(), c.ID, "", s.opts.Compaction.KeepRecentTokens, "threshold", s.opts.Sink); err == nil {
			return s.afterCompaction(ctx, run, "threshold compaction before the request")
		} else if !errors.Is(err, errNothingToCompact) {
			run.Notices = append(run.Notices, "threshold compaction failed: "+err.Error())
		}
	}
	// Background start: above the background threshold but below the
	// blocking one, summarize concurrently with this request. The result
	// waits for the next request boundary of this conversation.
	if s.opts.Compaction.BackgroundTokens > 0 && estimateTokens(agent.Messages())+len(agent.System)/4 > s.opts.Compaction.BackgroundTokens {
		s.compactor.start(ctx, s.r, s.currentEngine(), c.ID, s.opts.Compaction.KeepRecentTokens, s.opts.Sink)
	}
	var usage provider.Usage
	usageKnown := false
	var partial *partialBatcher
	if s.opts.PartialFlushInterval >= 0 {
		partial = s.startPartial(ctx, run)
	}
	stop, msg, turnErr := agent.Turn(ctx, func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvUsage:
			if !e.Auxiliary {
				usage, usageKnown = usage.Add(e.Usage), true
			}
		case core.EvTextDelta:
			if partial != nil {
				partial.add(e.Delta)
			}
		}
		s.opts.Sink(ev)
	})
	completed := turnErr == nil && stop != provider.StopError && stop != provider.StopAborted
	if partial != nil {
		partial.finish(ctx, completed)
	}
	// The attempt's final commit removes the live partial record, or, for a
	// failed attempt, the retained final record stays until the next attempt
	// replaces it. Reread the snapshot so the clear sees the last flush.
	snap, err = s.r.store.Snapshot(ctx)
	if err != nil {
		return run, err
	}
	c, err = conversation(snap, c.ID)
	if err != nil {
		return run, err
	}
	var clear []storage.Operation
	if completed {
		clear = partialClearOp(snap, c.ID)
	}
	// Record the effective request shape (system prompt and tool schemas by
	// hash) so the attempt can be explained later.
	system, toolDefs, _ := agent.ContextSnapshot()
	toolJSON, _ := json.Marshal(toolDefs)
	names := make([]string, 0, len(toolDefs))
	for _, t := range toolDefs {
		names = append(names, t.Name)
	}
	clear = append(clear, promptOps(snap, c, run, agent.Model, system, toolJSON, names)...)
	ledger := s.usageRow(&c, run, agent, usage, usageKnown)
	if turnErr != nil && !run.Compacted && s.opts.Compaction.ContextWindow > 0 && isContextOverflow(turnErr) {
		// Overflow: record the attempt, compact once, and retry the request
		// without counting it against the retry budget.
		c.EntrySequence++
		attempt := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryAttempt, Content: turnErr.Error(), Time: time.Now().UTC()}
		committed, err := s.commitRun(ctx, snap, c, run, "run.overflow", append([]storage.Operation{record(entryKey(c.ID, c.EntrySequence), attempt), ledger}, clear...)...)
		if err != nil {
			return committed, err
		}
		if _, err := s.r.compact(ctx, s.currentEngine(), c.ID, "", s.opts.Compaction.KeepRecentTokens, "overflow", s.opts.Sink); err != nil {
			// The attempt and its ledger row are already committed. End the
			// run on fresh state so no entry or counter is written twice.
			snap, snapErr := s.r.store.Snapshot(ctx)
			if snapErr != nil {
				return committed, snapErr
			}
			fresh, cErr := conversation(snap, c.ID)
			if cErr != nil {
				return committed, cErr
			}
			committed.Phase, committed.Outcome, committed.Error = "done", "failed", fmt.Sprintf("%v (compaction after overflow failed: %v)", turnErr, err)
			return s.commitRun(ctx, snap, fresh, committed, "run.failed", s.settle(snap, committed, "failed")...)
		}
		return s.afterCompaction(ctx, committed, "compacted after the provider rejected the context size")
	}
	if turnErr != nil || stop == provider.StopError || stop == provider.StopAborted {
		return s.failedAttempt(ctx, run, c, snap, msg, stop, turnErr, ledger, clear...)
	}
	// A successful turn appends the assistant message. Tool intents are
	// recorded in the same commit so recovery never sees a call without an
	// owning round or a round without its call.
	next := run
	next.Tools = nil
	var calls []provider.ToolCallBlock
	for _, block := range msg.Content {
		if tc, ok := block.(provider.ToolCallBlock); ok && !tc.Server {
			calls = append(calls, tc)
		}
	}
	for _, tc := range calls {
		next.Tools = append(next.Tools, ToolIntent{CallID: tc.ID, Name: tc.Name, State: "pending"})
	}
	c.EntrySequence++
	entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryAssistant, Content: core.MessageText(msg), Time: time.Now().UTC()}
	entry.Message = marshalMessage(msg)
	ops := append([]storage.Operation{record(entryKey(c.ID, c.EntrySequence), entry), ledger}, clear...)
	if stop == provider.StopToolUse && len(calls) > 0 {
		next.Phase = "tools"
	} else {
		next.Phase = "done"
		next.Outcome = "completed"
		ops = append(ops, s.settle(snap, next, "answered")...)
	}
	next.Cutoff = c.EntrySequence
	return s.commitRun(ctx, snap, c, next, "run.response", ops...)
}

// afterCompaction moves the run's cutoff past the new compaction entry and
// marks the turn compacted. The request is then sent on the next drive pass.
func (s *Service) afterCompaction(ctx context.Context, run Run, notice string) (Run, error) {
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return run, err
	}
	c, err := conversation(snap, run.ConversationID)
	if err != nil {
		return run, err
	}
	latest, ok, err := read[Run](snap, runKey(c.ID))
	if err != nil {
		return run, err
	}
	if !ok || latest.ID != run.ID {
		return run, fmt.Errorf("%w: run replaced", storage.ErrConflict)
	}
	latest.Cutoff, latest.Compacted = c.EntrySequence, true
	latest.Notices = append(latest.Notices, notice)
	return s.commitRun(ctx, snap, c, latest, "run.compacted")
}

// failedAttempt records the failed or aborted attempt as a non-context entry
// and either schedules another attempt or ends the run. Usage already charged
// is unknown here and is never reported as zero.
func (s *Service) failedAttempt(ctx context.Context, run Run, c Conversation, snap storage.Snapshot, msg provider.Message, stop provider.StopReason, turnErr error, ledger storage.Operation, extra ...storage.Operation) (Run, error) {
	reason := string(stop)
	if turnErr != nil {
		reason = turnErr.Error()
	}
	c.EntrySequence++
	entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryAttempt, Content: reason, Time: time.Now().UTC()}
	if len(msg.Content) > 0 {
		entry.Message = marshalMessage(msg)
	}
	ops := append([]storage.Operation{record(entryKey(c.ID, c.EntrySequence), entry), ledger}, extra...)
	next := run
	// Context cancellation is the caller's decision, not a provider failure.
	// The run stays in its request phase for the next Step.
	if errors.Is(turnErr, context.Canceled) || errors.Is(turnErr, context.DeadlineExceeded) || (stop == provider.StopAborted && turnErr == nil && ctx.Err() != nil) {
		next.Notices = append(next.Notices, fmt.Sprintf("turn %d attempt %d interrupted by cancellation", run.Turn, run.Attempt))
		committed, err := s.commitRun(ctx, snap, c, next, "run.interrupt", ops...)
		if err != nil {
			return committed, err
		}
		return committed, ctx.Err()
	}
	if turnErr != nil && run.Attempt < s.opts.MaxAttempts && core.RetryableProviderError(turnErr) {
		next.Attempt++
		committed, err := s.commitRun(ctx, snap, c, next, "run.retry", ops...)
		if err != nil {
			return committed, err
		}
		delay := s.opts.RetryDelay * time.Duration(1<<(run.Attempt-1))
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return committed, ctx.Err()
		case <-timer.C:
		}
		return committed, nil
	}
	next.Phase = "done"
	next.Outcome = "failed"
	next.Error = reason
	ops = append(ops, s.settle(snap, next, "failed")...)
	return s.commitRun(ctx, snap, c, next, "run.failed", ops...)
}

// tools executes the committed round. For each intent: commit running state
// with effective arguments and replay policy, execute, commit the result
// entry. A tool found running at load time was interrupted by a crash and is
// resolved through its replay policy, never by assuming it did not run.
func (s *Service) tools(ctx context.Context, run Run) (Run, error) {
	agent, c, snap, err := s.load(ctx, run)
	if err != nil {
		return run, err
	}
	assistant, err := lastEntryOfType(snap, c.ID, run.Cutoff, entryAssistant)
	if err != nil {
		return run, err
	}
	calls := map[string]provider.ToolCallBlock{}
	for _, block := range assistant.Content {
		if tc, ok := block.(provider.ToolCallBlock); ok {
			calls[tc.ID] = tc
		}
	}
	for i := range run.Tools {
		if err := ctx.Err(); err != nil {
			return run, err
		}
		intent := run.Tools[i]
		if intent.State == "done" {
			continue
		}
		// An abort requested during the round stops before the next effect.
		if current, ok, err := s.r.Run(ctx, run.ConversationID); err != nil {
			return run, err
		} else if ok && current.ID == run.ID && current.AbortRequested {
			run.AbortRequested = true
			return run, nil
		}
		call, ok := calls[intent.CallID]
		if !ok {
			return run, fmt.Errorf("%w: tool intent without call", storage.ErrCorrupt)
		}
		var result core.ToolResult
		var status string
		executed := false
		switch intent.State {
		case "pending":
			// Fresh authorization and argument rewrite run before the intent
			// commit so recovery can rerun with the effective arguments.
			args, allowed, reason, policy := s.authorize(ctx, agent, run, call)
			if !allowed {
				result = core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: reason}}}
				status = "blocked"
				break
			}
			// Persisted approval: a pending record parks the run until a
			// human decides. The decision binds to these exact arguments.
			if s.opts.Approver != nil {
				pending, approved, decided, err := approvalFor(snap, c, run, call, args)
				if err != nil {
					return run, err
				}
				switch {
				case pending != nil:
					return run, ErrAwaitingApproval
				case decided != nil && !approved:
					result = core.ToolResult{IsError: true, Status: "blocked", Content: []provider.Content{provider.TextBlock{Text: "denied: " + firstNonEmpty(decided.Reason, "a human denied this tool call")}}}
					status = "blocked"
					run.Tools[i].Approval = decided.ID
				case approved:
					run.Tools[i].Approval = decided.ID
				default:
					if req, need := s.opts.Approver(ctx, c, call, args); need {
						if _, err := s.requestApproval(ctx, snap, c, run, call, args, req); err != nil {
							return run, err
						}
						return run, ErrAwaitingApproval
					}
				}
				if status == "blocked" {
					break
				}
			}
			run.Tools[i].State, run.Tools[i].Args, run.Tools[i].Replay = "running", args, policy
			run, err = s.commitRun(ctx, snap, c, run, "tool.intent")
			if err != nil {
				return run, err
			}
			snap, err = s.r.store.Snapshot(ctx)
			if err != nil {
				return run, err
			}
			call.Arguments = args
			result = s.execute(ctx, agent, run, call)
			executed = true
			if ctx.Err() != nil {
				// The process is still alive and knows the call was interrupted
				// by cancellation. Leave the intent running so the next Step
				// resolves it through the replay policy like a crash.
				return run, ctx.Err()
			}
			if result.Status == "unknown" {
				// The tool lost track of an external operation. Nothing is
				// recorded as a result; the intent stays running and the
				// next Step reconciles or reports it.
				run.Notices = append(run.Notices, fmt.Sprintf("tool %s outcome unknown: %s", call.ID, core.ToolResultText(result)))
				return run, fmt.Errorf("%w: tool %s", core.ErrToolOutcomeUnknown, call.ID)
			}
		case "running":
			// Interrupted after the intent commit. The stored policy and the
			// live tool must agree, and authorization is checked again.
			// Nothing here assumes the earlier execution did not happen.
			decision, replayResult, replayed := s.recoverInterrupted(ctx, agent, c, run, intent, call)
			if ctx.Err() != nil {
				return run, ctx.Err()
			}
			run.Notices = append(run.Notices, decision)
			if replayed {
				if replayResult.Status == "unknown" {
					run.Notices = append(run.Notices, fmt.Sprintf("tool %s outcome unknown after replay: %s", call.ID, core.ToolResultText(replayResult)))
					return run, fmt.Errorf("%w: tool %s", core.ErrToolOutcomeUnknown, call.ID)
				}
				result, executed = replayResult, true
				break
			}
			if replayResult.Content != nil {
				// Reconciliation found the effect completed and supplied the result.
				result = replayResult
				break
			}
			result = core.ToolResult{IsError: true, Status: "interrupted", Content: []provider.Content{provider.TextBlock{Text: "interrupted: the process stopped after this tool call started and before its result was recorded. Its effect is unknown and it was not repeated automatically."}}}
			status = "interrupted"
		default:
			return run, fmt.Errorf("%w: unknown tool intent state", storage.ErrCorrupt)
		}
		if status == "" {
			status = "completed"
			if result.IsError {
				status = "failed"
			}
		}
		// A tool may have committed on its own behalf (owned conversations,
		// documents). Re-read the conversation at the current snapshot so the
		// result commit builds on committed state, not a pre-execution copy.
		// The run itself must not have changed except for an abort request.
		snap, err = s.r.store.Snapshot(ctx)
		if err != nil {
			return run, err
		}
		latest, ok, err := read[Run](snap, runKey(c.ID))
		if err != nil {
			return run, err
		}
		if !ok || latest.ID != run.ID {
			return run, fmt.Errorf("%w: run replaced during tool execution", storage.ErrConflict)
		}
		// The committed intent must be exactly what this stepper left: a
		// different state means a competing stepper resolved the call, and
		// this result must not be recorded twice. Locally appended notices
		// and an abort request from another caller are allowed to differ.
		if i >= len(latest.Tools) || latest.Tools[i].CallID != call.ID || latest.Tools[i].State != intentStateBeforeExecute(intent, executed) || latest.Phase != "tools" {
			return run, fmt.Errorf("%w: run changed during tool execution", ErrBusy)
		}
		run.AbortRequested = latest.AbortRequested
		run.Revision = latest.Revision
		c, err = conversation(snap, c.ID)
		if err != nil {
			return run, err
		}
		block := provider.ToolResultBlock{CallID: call.ID, Content: result.Content, IsError: result.IsError}
		run.Tools[i].State = "done"
		run.Tools[i].Result = &block
		c.EntrySequence++
		run.Tools[i].Entry = c.EntrySequence
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryToolResult, Content: core.ToolResultText(result), Time: time.Now().UTC()}
		entry.Message = marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}, Time: time.Now().UTC()})
		run, c, snap, err = s.commitResult(ctx, snap, c, run, i, entry)
		if err != nil {
			return run, err
		}
		s.opts.Sink(core.EvToolResult{ID: call.ID, Name: call.Name, Args: call.Arguments, Status: status, Executed: executed, Result: result})
		snap, err = s.r.store.Snapshot(ctx)
		if err != nil {
			return run, err
		}
		if req, ok := handoffFromResult(call.Name, result); ok {
			return s.handoff(ctx, snap, c, run, i, call.ID, req)
		}
	}
	// Round complete: next request includes every result entry.
	if run.Turn >= s.opts.MaxTurns {
		run.Phase, run.Outcome, run.Error = "done", "failed", fmt.Sprintf("max turns (%d) exceeded", s.opts.MaxTurns)
		return s.commitRun(ctx, snap, c, run, "run.failed", s.settle(snap, run, "failed")...)
	}
	// Steering: inputs admitted with the steer policy while this round ran
	// join the run here, at the safe boundary after the tool results. Their
	// user entries move into the model context by rewriting them after the
	// results, so the provider sees results before the new instruction.
	steer, err := s.claimSteering(snap, &c, &run)
	if err != nil {
		return run, err
	}
	run.Phase, run.Turn, run.Attempt, run.Tools, run.Cutoff, run.Compacted = "request", run.Turn+1, 1, nil, c.EntrySequence, false
	return s.commitRun(ctx, snap, c, run, "run.next", steer...)
}

// claimSteering removes queued steer submissions from the queue and adds them
// to the run. Each steered input gets a fresh user entry at the current tail
// so ModelContext places it after the tool results; the original admission
// entry is retained for history and marked superseded so it is not sent twice.
func (s *Service) claimSteering(snap storage.Snapshot, c *Conversation, run *Run) ([]storage.Operation, error) {
	queue, err := snap.Page("queue/"+c.ID+"/", "", 100)
	if err != nil {
		return nil, err
	}
	var ops []storage.Operation
	for _, row := range queue {
		var id string
		if err := json.Unmarshal(row.Value, &id); err != nil {
			return nil, storage.ErrCorrupt
		}
		sub, ok, err := read[Submission](snap, "submission/"+id)
		if err != nil || !ok {
			return nil, fmt.Errorf("%w: queued submission missing", storage.ErrCorrupt)
		}
		if sub.Policy != PolicySteer {
			// Queue policy waits for the next run; order is preserved because
			// start claims the queue prefix in sequence order.
			break
		}
		sub.State = "running"
		run.Submissions = append(run.Submissions, id)
		run.Notices = append(run.Notices, fmt.Sprintf("steered input %s joined at turn %d", id, run.Turn+1))
		c.EntrySequence++
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, SubmissionID: id, Revision: snap.Revision() + 1, Type: entrySteer, Content: sub.Content, Time: time.Now().UTC()}
		ops = append(ops, record("submission/"+id, sub), storage.Operation{Key: row.Key, Delete: true}, record(entryKey(c.ID, c.EntrySequence), entry))
	}
	return ops, nil
}

// handoff ends the run after a handoff tool result: remaining intents of the
// round receive aborted results so the transcript stays paired, a reset entry
// with the note closes the context, and the continuation is admitted. All in
// one commit, deduplicated by the call so a re-step cannot admit it twice.
func (s *Service) handoff(ctx context.Context, snap storage.Snapshot, c Conversation, run Run, index int, callID string, req handoffRequest) (Run, error) {
	var ops []storage.Operation
	for i := index + 1; i < len(run.Tools); i++ {
		if run.Tools[i].State == "done" {
			continue
		}
		text := "skipped: the model handed off to a fresh context before this call ran."
		block := provider.ToolResultBlock{CallID: run.Tools[i].CallID, Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}
		c.EntrySequence++
		run.Tools[i].State, run.Tools[i].Entry = "done", c.EntrySequence
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryToolResult, Content: text, Time: time.Now().UTC()}
		entry.Message = marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}, Time: time.Now().UTC()})
		ops = append(ops, record(entryKey(c.ID, c.EntrySequence), entry))
	}
	handoff, err := handoffOps(snap, &c, run, callID, req)
	if err != nil {
		return run, err
	}
	ops = append(ops, handoff...)
	run.Phase, run.Outcome = "done", "completed"
	run.Notices = append(run.Notices, fmt.Sprintf("handoff by tool call %s: context reset, continuation admitted", callID))
	ops = append(ops, s.settle(snap, run, "answered")...)
	return s.commitRun(ctx, snap, c, run, "run.handoff", ops...)
}

// abort settles an aborted run. Every unfinished intent receives an aborted
// error result so the transcript stays paired; nothing executes. Inputs settle
// aborted. The commit is one transaction.
func (s *Service) abort(ctx context.Context, run Run) (Run, error) {
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return run, err
	}
	c, err := conversation(snap, run.ConversationID)
	if err != nil {
		return run, err
	}
	var ops []storage.Operation
	for i, intent := range run.Tools {
		if intent.State == "done" {
			continue
		}
		text := "aborted: the run was aborted before this tool call started."
		if intent.State == "running" {
			text = "aborted: the run was aborted after this tool call started. Its effect is unknown."
			run.Notices = append(run.Notices, fmt.Sprintf("tool %s aborted while running, effect unknown", intent.CallID))
		}
		block := provider.ToolResultBlock{CallID: intent.CallID, Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}
		c.EntrySequence++
		run.Tools[i].State, run.Tools[i].Entry = "done", c.EntrySequence
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryToolResult, Content: text, Time: time.Now().UTC()}
		entry.Message = marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}, Time: time.Now().UTC()})
		ops = append(ops, record(entryKey(c.ID, c.EntrySequence), entry))
	}
	// Pending approvals of this run can no longer be acted on; expire them
	// so a later decision is refused instead of approving nothing.
	pending, err := pendingApprovals(snap, c.ID)
	if err != nil {
		return run, err
	}
	for _, a := range pending {
		if a.RunID == run.ID {
			a.State = approvalExpired
			a.Reason = "run aborted"
			a.Revision = snap.Revision() + 1
			ops = append(ops, record(approvalKey(a.ID), a))
		}
	}
	run.Phase, run.Outcome, run.Error = "done", "aborted", "aborted by request"
	ops = append(ops, s.settle(snap, run, "aborted")...)
	return s.commitRun(ctx, snap, c, run, "run.abort", ops...)
}

// recoverInterrupted applies the replay contract to a running intent. It
// returns a human-readable decision, a result when one was obtained, and
// whether the tool executed again. An empty result with replayed false means
// the call is reported as interrupted.
func (s *Service) recoverInterrupted(ctx context.Context, agent *core.Agent, c Conversation, run Run, intent ToolIntent, call provider.ToolCallBlock) (string, core.ToolResult, bool) {
	none := core.ToolResult{}
	if intent.Replay == core.ReplayNever || intent.Replay == "" {
		return fmt.Sprintf("tool %s interrupted, effect unknown, not replayed (policy never)", call.ID), none, false
	}
	tool, err := agent.Tools.Get(call.Name)
	if err != nil {
		return fmt.Sprintf("tool %s interrupted, not replayed: tool no longer registered", call.ID), none, false
	}
	live := core.ReplayPolicyOf(tool)
	if live != intent.Replay {
		return fmt.Sprintf("tool %s interrupted, not replayed: stored policy %s, live policy %s", call.ID, intent.Replay, live), none, false
	}
	_, allowed, reason, _ := s.authorize(ctx, agent, run, call)
	if !allowed {
		return fmt.Sprintf("tool %s interrupted, replay denied by current authorization: %s", call.ID, reason), none, false
	}
	call.Arguments = intent.Args
	switch intent.Replay {
	case core.ReplaySafe:
		return fmt.Sprintf("tool %s replayed after interruption (policy safe)", call.ID), s.execute(ctx, agent, run, call), true
	case core.ReplayIdempotent:
		return fmt.Sprintf("tool %s replayed after interruption with the same operation key (policy idempotent); the receiver must enforce the key", call.ID), s.execute(ctx, agent, run, call), true
	case core.ReplayReconcile:
		rec, ok := tool.(core.ToolReconciler)
		if !ok {
			return fmt.Sprintf("tool %s interrupted, not replayed: reconcile policy without reconciler", call.ID), none, false
		}
		key := ToolCallIdentity{RunID: run.ID, ConversationID: c.ID, CallID: call.ID}.OperationKey()
		outcome, found, err := rec.Reconcile(ctx, key, intent.Args)
		if err != nil {
			return fmt.Sprintf("tool %s interrupted, reconciliation failed, not replayed: %v", call.ID, err), none, false
		}
		switch outcome {
		case core.ReconcileCompleted:
			if found.Content == nil {
				found.Content = []provider.Content{provider.TextBlock{Text: "completed before the interruption (reconciled)"}}
			}
			return fmt.Sprintf("tool %s reconciled as completed, result recovered without re-execution", call.ID), found, false
		case core.ReconcileNotStarted:
			return fmt.Sprintf("tool %s reconciled as not started, executed (policy reconcile)", call.ID), s.execute(ctx, agent, run, call), true
		default:
			return fmt.Sprintf("tool %s interrupted, reconciliation outcome unknown, not replayed", call.ID), none, false
		}
	}
	return fmt.Sprintf("tool %s interrupted, unknown replay policy %s, not replayed", call.ID, intent.Replay), none, false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// intentStateBeforeExecute is the committed intent state this stepper expects
// to find after executing (or declining to execute) a call: running once an
// intent was committed, otherwise still the state it was loaded in.
func intentStateBeforeExecute(intent ToolIntent, executed bool) string {
	if executed || intent.State == "running" {
		return "running"
	}
	return intent.State
}

// usageRow builds the ledger row of one attempt and advances the counter on
// the conversation copy that the same commit writes. Missing provider usage is
// recorded as unknown so totals never understate spend silently.
func (s *Service) usageRow(c *Conversation, run Run, agent *core.Agent, usage provider.Usage, known bool) storage.Operation {
	c.UsageSequence++
	status := "unknown"
	if known {
		status = "known"
	}
	providerName := c.Config.Provider
	if agent.Client != nil {
		providerName = agent.Client.Name()
	}
	return record(usageKey(c.ID, c.UsageSequence), UsageRecord{ConversationID: c.ID, RunID: run.ID, Turn: run.Turn, Attempt: run.Attempt, Provider: providerName, Model: agent.Model, Usage: usage, Status: status, Source: "model", Time: time.Now().UTC()})
}

func (s *Service) authorize(ctx context.Context, agent *core.Agent, run Run, call provider.ToolCallBlock) (json.RawMessage, bool, string, core.ToolReplayPolicy) {
	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if c, cerr := s.r.Conversation(ctx, run.ConversationID); cerr == nil && !c.Config.allows(call.Name) {
		// Checked at authorization time so a configuration change between
		// request and tool round applies to the current call.
		return args, false, fmt.Sprintf("tool %q is not permitted by this conversation's configuration", call.Name), core.ReplayNever
	}
	tool, err := agent.Tools.Get(call.Name)
	if err != nil {
		return args, false, err.Error(), core.ReplayNever
	}
	if agent.BeforeToolExecuteContext != nil {
		allowed, reason, modified := agent.BeforeToolExecuteContext(ctx, call)
		if len(modified) > 0 && json.Valid(modified) {
			args = modified
		}
		if !allowed {
			if reason == "" {
				reason = "tool call refused by host policy"
			}
			return args, false, reason, core.ReplayNever
		}
	} else if agent.BeforeToolExecute != nil {
		allowed, reason, modified := agent.BeforeToolExecute(call)
		if len(modified) > 0 && json.Valid(modified) {
			args = modified
		}
		if !allowed {
			if reason == "" {
				reason = "tool call refused by host policy"
			}
			return args, false, reason, core.ReplayNever
		}
	}
	return args, true, "", core.ReplayPolicyOf(tool)
}

// execute runs one authorized call through the core tool path with hooks
// disabled, because authorization already happened before the intent commit
// and must not run twice for one effect.
func (s *Service) execute(ctx context.Context, agent *core.Agent, run Run, call provider.ToolCallBlock) core.ToolResult {
	before, beforeCtx := agent.BeforeToolExecute, agent.BeforeToolExecuteContext
	agent.BeforeToolExecute, agent.BeforeToolExecuteContext = nil, nil
	defer func() { agent.BeforeToolExecute, agent.BeforeToolExecuteContext = before, beforeCtx }()
	identity := ToolCallIdentity{RunID: run.ID, ConversationID: run.ConversationID, CallID: call.ID}
	ctx = context.WithValue(ctx, toolCallKey{}, identity)
	ctx = core.WithToolOperationKey(ctx, identity.OperationKey())
	return agent.CallTool(ctx, call.ID, call.Name, call.Arguments, func(ev core.AgentEvent) {
		switch ev.(type) {
		case core.EvToolResult, core.EvToolCall:
			// The turn already announced the call, and the service reports the
			// result after its commit so observers never see an uncommitted one.
			return
		}
		s.opts.Sink(ev)
	})
}

func (s *Service) settle(snap storage.Snapshot, run Run, state string) []storage.Operation {
	var ops []storage.Operation
	for _, id := range run.Submissions {
		sub, ok, err := read[Submission](snap, "submission/"+id)
		if err != nil || !ok {
			continue
		}
		sub.State = state
		ops = append(ops, record("submission/"+id, sub))
	}
	return ops
}

// commitResult commits one tool result. The only concurrent write this
// stepper accepts is an abort request: the result is still recorded (the
// effect happened) and the abort flag is carried forward. Any other change to
// the run is a competing stepper and fails with ErrBusy.
func (s *Service) commitResult(ctx context.Context, snap storage.Snapshot, c Conversation, run Run, i int, entry Entry) (Run, Conversation, storage.Snapshot, error) {
	for {
		committed, err := s.commitRun(ctx, snap, c, run, "tool.result", record(entryKey(c.ID, c.EntrySequence), entry))
		if err == nil {
			next, err := s.r.store.Snapshot(ctx)
			return committed, c, next, err
		}
		if !errors.Is(err, storage.ErrConflict) {
			return run, c, snap, err
		}
		latestSnap, snapErr := s.r.store.Snapshot(ctx)
		if snapErr != nil {
			return run, c, snap, snapErr
		}
		latest, ok, readErr := read[Run](latestSnap, runKey(run.ConversationID))
		if readErr != nil {
			return run, c, snap, readErr
		}
		expected := run
		expected.Tools = append([]ToolIntent(nil), run.Tools...)
		expected.AbortRequested = latest.AbortRequested
		expected.Revision = latest.Revision
		expected.Tools[i].State, expected.Tools[i].Entry = "running", 0
		before, _ := json.Marshal(expected)
		after, _ := json.Marshal(latest)
		if !ok || !latest.AbortRequested || string(before) != string(after) {
			return run, c, snap, err
		}
		latestC, cErr := conversation(latestSnap, c.ID)
		if cErr != nil {
			return run, c, snap, cErr
		}
		if latestC.EntrySequence != c.EntrySequence-1 {
			return run, c, snap, err
		}
		latestC.EntrySequence = c.EntrySequence
		run.AbortRequested = true
		entry.Revision = latestSnap.Revision() + 1
		c, snap = latestC, latestSnap
	}
}

// commitRun commits run and conversation state. The store's optimistic check
// is global, so commits of unrelated conversations can collide. A collision
// is retried against a fresh snapshot as long as this conversation and its
// run are unchanged; a change to either means a competing stepper and fails
// with ErrBusy. Entry keys written by ops are rebased onto the fresh
// snapshot's conversation counters, which are unchanged by construction.
func (s *Service) commitRun(ctx context.Context, snap storage.Snapshot, c Conversation, run Run, actor string, ops ...storage.Operation) (Run, error) {
	baseConversation, _, _ := read[Conversation](snap, "conversation/"+c.ID)
	baseRun, _, _ := read[Run](snap, runKey(c.ID))
	for attempt := 0; ; attempt++ {
		c.Revision = snap.Revision() + 1
		run.Revision = c.Revision
		all := append(append([]storage.Operation(nil), ops...), record("conversation/"+c.ID, c), record(runKey(c.ID), run))
		err := s.r.commit(ctx, snap, actor, all...)
		if err == nil {
			return run, nil
		}
		if !errors.Is(err, storage.ErrConflict) || attempt >= 64 {
			if errors.Is(err, storage.ErrConflict) {
				return run, fmt.Errorf("%w: %w", ErrBusy, err)
			}
			return run, err
		}
		fresh, snapErr := s.r.store.Snapshot(ctx)
		if snapErr != nil {
			return run, snapErr
		}
		freshConversation, _, _ := read[Conversation](fresh, "conversation/"+c.ID)
		freshRun, _, _ := read[Run](fresh, runKey(c.ID))
		if !sameJSON(baseConversation, freshConversation) || !sameJSON(baseRun, freshRun) {
			return run, fmt.Errorf("%w: %w", ErrBusy, err)
		}
		snap = fresh
	}
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func marshalMessage(msg provider.Message) json.RawMessage {
	b, _ := json.Marshal(msg)
	return b
}

func lastEntryOfType(snap storage.Snapshot, conversationID string, cutoff uint64, kind string) (provider.Message, error) {
	for seq := cutoff; seq > 0; seq-- {
		e, ok, err := read[Entry](snap, entryKey(conversationID, seq))
		if err != nil {
			return provider.Message{}, err
		}
		if !ok {
			return provider.Message{}, fmt.Errorf("%w: missing entry", storage.ErrCorrupt)
		}
		if e.Type == kind {
			return core.DecodeMessage(e.Message)
		}
	}
	return provider.Message{}, fmt.Errorf("%w: no %s entry", storage.ErrCorrupt, kind)
}

// ModelContext projects the visible history of a conversation through cutoff
// into provider messages. It follows fork ancestry, starts the context at the
// newest reset boundary, excludes failed attempts, merges a round's tool
// results into one tool message, and repairs dangling calls with the
// provider-neutral pairing rules. Legacy entries use their paired projection.
func ModelContext(ctx context.Context, snap storage.Snapshot, conversationID string, cutoff uint64) ([]provider.Message, error) {
	var messages []provider.Message
	// keptAfter maps an entry sequence of this conversation to the index in
	// messages where that entry's message starts, so a compaction can keep
	// the tail from its head.
	keptAfter := map[uint64]int{}
	appendMessage := func(msg provider.Message) {
		if len(msg.Content) == 0 {
			return
		}
		// One result entry per call, one tool message per round for the
		// provider. Merge consecutive results into the round's message.
		if n := len(messages); msg.Role == provider.RoleTool && n > 0 && messages[n-1].Role == provider.RoleTool {
			merged := messages[n-1]
			merged.Content = append(append([]provider.Content(nil), merged.Content...), msg.Content...)
			messages[n-1] = merged
			return
		}
		messages = append(messages, msg)
	}
	// Steered submissions appear twice in history: at admission (type user)
	// and where they joined the run (type steer). Only the latter is model
	// context. The admission entry always precedes the steer entry, so the
	// user message is appended provisionally and removed when its steer
	// entry arrives; userAt remembers where each admission landed.
	userAt := map[string]int{}
	err := History(ctx, snap, conversationID, cutoff, func(owner string, seq uint64, e Entry) error {
		if owner == conversationID {
			keptAfter[seq] = len(messages)
		}
		switch {
		case e.Type == entryAttempt:
		case e.Type == entrySteer:
			if at, ok := userAt[e.SubmissionID]; ok && at < len(messages) {
				messages = append(messages[:at], messages[at+1:]...)
				for id, pos := range userAt {
					if pos > at {
						userAt[id] = pos - 1
					}
				}
				for k, v := range keptAfter {
					if v > at {
						keptAfter[k] = v - 1
					}
				}
				delete(userAt, e.SubmissionID)
			}
			messages = append(messages, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: e.Content}}})
		case e.Type == entryReset:
			messages = messages[:0]
			clear(userAt)
			if e.Content != "" {
				messages = append(messages, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: e.Content}}})
			}
		case e.Type == entryCompaction:
			// The summary replaces everything before its head. Entries from
			// the head through this compaction were appended after the
			// summary's cut and are kept verbatim; they already sit in
			// messages, so drop only the part before the head.
			var info CompactionInfo
			if err := json.Unmarshal(e.Data, &info); err != nil || info.Head == 0 {
				return fmt.Errorf("%w: compaction entry without head", storage.ErrCorrupt)
			}
			kept := keptAfter[info.Head]
			summary, err := core.DecodeMessage(e.Message)
			if err != nil {
				return fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
			}
			tail := append([]provider.Message(nil), messages[kept:]...)
			messages = append(append(messages[:0], summary), tail...)
			messages = provider.RepairOrphanedToolResults(messages)
			// Admission positions before the head are summarized away; the
			// ones after shift behind the summary.
			for id, pos := range userAt {
				if pos < kept {
					delete(userAt, id)
				} else {
					userAt[id] = pos - kept + 1
				}
			}
		case len(e.Message) > 0:
			msg, err := core.DecodeMessage(e.Message)
			if err != nil {
				return fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
			}
			appendMessage(msg)
		case e.Type == "user":
			if e.SubmissionID != "" {
				// A withdrawn submission keeps its history entry but is not
				// model context.
				if sub, ok, err := read[Submission](snap, "submission/"+e.SubmissionID); err != nil {
					return err
				} else if ok && sub.State == "withdrawn" {
					return nil
				}
				userAt[e.SubmissionID] = len(messages)
			}
			messages = append(messages, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: e.Content}}})
		case strings.HasPrefix(e.Type, "legacy_"):
			rows := e.SessionProjection
			if len(rows) == 0 {
				rows = []json.RawMessage{e.LegacyRow}
			}
			for _, row := range rows {
				var line struct {
					Type     string            `json:"type"`
					Message  json.RawMessage   `json:"message"`
					Messages []json.RawMessage `json:"messages"`
				}
				if err := json.Unmarshal(row, &line); err != nil {
					return storage.ErrCorrupt
				}
				switch line.Type {
				case "message":
					msg, err := core.DecodeMessage(line.Message)
					if err != nil {
						return fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
					}
					appendMessage(msg)
				case "compaction":
					// A compaction replaces provider context; history remains stored.
					messages = messages[:0]
					for _, raw := range line.Messages {
						msg, err := core.DecodeMessage(raw)
						if err != nil {
							return fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
						}
						appendMessage(msg)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return provider.RepairOrphanedToolResults(messages), nil
}

// ToolCallIdentity names the durable tool call a tool is executing for. Tools
// that create owned work (subagents, child tasks) use it as a stable owner so
// a replay attaches to existing work instead of creating more.
type ToolCallIdentity struct {
	RunID          string
	ConversationID string
	CallID         string
}

type toolCallKey struct{}

// ToolCallFromContext returns the identity of the current durable tool call,
// or false when the tool runs outside a continuous run.
func ToolCallFromContext(ctx context.Context) (ToolCallIdentity, bool) {
	id, ok := ctx.Value(toolCallKey{}).(ToolCallIdentity)
	return id, ok
}

// OwnerID is the stable owner key for work created by this call.
func (id ToolCallIdentity) OwnerID() string { return "call/" + id.RunID + "/" + id.CallID }

// OperationKey is the stable idempotency key of the call. It is the same for
// every attempt of one committed intent and differs from the writer epoch, so
// an external receiver can deduplicate across crashes.
func (id ToolCallIdentity) OperationKey() string {
	return "zot-op-" + hashedKey("", id.ConversationID, id.RunID, id.CallID)[:32]
}
