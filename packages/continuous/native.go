package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Execution.
//
// Model generations, tool calls, compactions, and subagents are durable tasks
// driven by the one TaskScheduler:
//
//	admission  submission + user entry + chain record + generation task
//	request    StartEffect (turn, attempt committed), one model attempt,
//	           then assistant entry + usage + owned tool tasks, one commit
//	tool       authorize, approval, StartEffect (effective args, replay
//	           policy), execute, then tool_result entry + outcome, one commit
//	collect    after every tool task is terminal: next turn, or the final
//	           answer + submission settlement, one commit
//	compact    a compaction task summarizes; the generation publishes the
//	           summary at its next request boundary
//
// The chain record is the conversation's single active generation chain and
// the run-shaped compatibility projection: its run ID is the logical run
// identity used for operation keys, approvals, usage rows, and prompt
// records, and it lists the submissions the chain answers.

const (
	// TaskKindGeneration is one generation chain of a conversation.
	TaskKindGeneration = "zot.generation"
	// TaskKindTool is one tool call owned by a generation.
	TaskKindTool = "zot.tool"
	// TaskKindCompaction is one summary request for a conversation.
	TaskKindCompaction = "zot.compaction"

	generationVersion = 1
	toolVersion       = 1
	compactionVersion = 1

	// executorTasks marks recovery actions of generation chains.
	executorTasks = "tasks"
)

// Chain is the active generation chain of a conversation.
type Chain struct {
	ConversationID string `json:"conversation_id"`
	// RunID is the logical run identity. It survives retries, turns,
	// migration, and restarts, unlike task IDs.
	RunID string `json:"run_id"`
	// Task is the generation task driving the chain.
	Task        string    `json:"task"`
	Submissions []string  `json:"submissions"`
	Notices     []string  `json:"notices,omitempty"`
	Started     time.Time `json:"started"`
	Revision    uint64    `json:"revision"`
}

// backgroundKey names the conversation's unpublished background summary
// task. At most one exists per conversation.
func backgroundKey(conversationID string) string { return "bgcompaction/" + conversationID }

// compactionStatsKey holds counters that cannot be derived from live records,
// such as summaries discarded as stale.
const compactionStatsKey = "stats/compaction"

func chainKey(conversationID string) string { return "chain/" + conversationID }

type generationInput struct {
	RunID string `json:"run_id"`
}

// generationCheckpoint is version 1 of the generation checkpoint schema.
type generationCheckpoint struct {
	Turn    int    `json:"turn"`
	Attempt int    `json:"attempt"`
	Cutoff  uint64 `json:"cutoff"`
	// Calls are the owned tool tasks of the current round, in call order.
	Calls []toolRef `json:"calls,omitempty"`
	// Compacted records that this turn already compacted once, so a second
	// overflow fails instead of looping.
	Compacted bool `json:"compacted,omitempty"`
	// Compaction is the blocking compaction task this generation waits on.
	Compaction string `json:"compaction,omitempty"`
	// Overflow is the provider error that triggered an overflow compaction.
	Overflow string `json:"overflow,omitempty"`
}

type toolRef struct {
	Task   string `json:"task"`
	CallID string `json:"call_id"`
}

type toolInput struct {
	RunID  string          `json:"run_id"`
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"`
	// After is the previous sibling. Tools of a round run sequentially.
	After string `json:"after,omitempty"`
	// Turn is the generation turn that requested the call.
	Turn int `json:"turn,omitempty"`
}

// toolCheckpoint is version 1 of the tool checkpoint schema. Args, Replay,
// and Approval are committed by StartEffect before the effect.
type toolCheckpoint struct {
	Args     json.RawMessage       `json:"args,omitempty"`
	Replay   core.ToolReplayPolicy `json:"replay,omitempty"`
	Approval string                `json:"approval,omitempty"`
	// Generation is the engine generation that executed the call.
	Generation uint64 `json:"generation,omitempty"`
	// Subagent is the owned conversation of a subagent call.
	Subagent string `json:"subagent,omitempty"`
	// Waiting is the subagent's submission this call waits on.
	Waiting string `json:"waiting,omitempty"`
	// Unresolved counts reconciliation passes of an effect whose outcome
	// is unknown.
	Unresolved int `json:"unresolved,omitempty"`
}

// maxUnresolved bounds reconciliation passes of an unknown outcome before
// the call is reported to the model as possibly applied.
const maxUnresolved = 5

type toolOutcome struct {
	Entry  uint64 `json:"entry"`
	Status string `json:"status"`
	// Handoff is set when the result ended the chain with a handoff.
	Handoff bool `json:"handoff,omitempty"`
}

type compactionInput struct {
	Reason     string `json:"reason"`
	KeepTokens int    `json:"keep_tokens,omitempty"`
}

// Chain reads the active generation chain of a conversation.
func (r *Runtime) Chain(ctx context.Context, conversationID string) (Chain, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Chain{}, false, err
	}
	return read[Chain](snap, chainKey(conversationID))
}

// MigrateLegacy converts every unfinished run of the earlier run-based
// executor into a generation chain, one conversation per commit, so a pass
// interrupted by a crash resumes on the next call and a converted run is
// never converted twice. The scheduler calls it before its first pass.
//
// A run that cannot be converted without guessing what an interrupted effect
// did keeps its record unchanged and blocks its conversation: queued inputs
// stay queued, RecoveryPreview reports it, and Runtime.Abort settles it. It is
// returned with its reason and announced once in the outbox.
func (r *Runtime) MigrateLegacy(ctx context.Context) (migrated int, blocked map[string]error, err error) {
	blocked = map[string]error{}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return 0, blocked, err
	}
	var ids []string
	if err := pageAll(snap, "run/", func(row storage.Record) error {
		var run Run
		if json.Unmarshal(row.Value, &run) != nil {
			return storage.ErrCorrupt
		}
		if run.Phase != "done" {
			ids = append(ids, run.ConversationID)
		}
		return nil
	}); err != nil {
		return 0, blocked, err
	}
	for _, id := range ids {
		for {
			snap, err := r.store.Snapshot(ctx)
			if err != nil {
				return migrated, blocked, err
			}
			run, ok, err := read[Run](snap, runKey(id))
			if err != nil {
				return migrated, blocked, err
			}
			if !ok || run.Phase == "done" {
				break
			}
			ops, cause := migrateRunOps(snap, run)
			if cause != nil {
				if !errors.Is(cause, ErrMigrationBlocked) {
					return migrated, blocked, cause
				}
				blocked[id] = cause
				notice := outboxOp("migration-blocked-"+run.ID, "migration.blocked", id, "interrupted run cannot continue without a decision: "+cause.Error(), map[string]string{"run_id": run.ID, "error": cause.Error()})
				if _, seen := snap.Get(outboxKey("migration-blocked-" + run.ID)); seen {
					break
				}
				ops = []storage.Operation{notice}
			} else {
				ops = append(ops, formatOps(snap, 2)...)
			}
			err = r.commit(ctx, snap, "run.migrate", ops...)
			if errors.Is(err, storage.ErrConflict) {
				continue
			}
			if err != nil {
				return migrated, blocked, err
			}
			if cause == nil {
				migrated++
			}
			break
		}
	}
	return migrated, blocked, nil
}

// legacyBlocked reports whether a conversation still has an unfinished run of
// the earlier executor. Such a conversation starts no chain until the run is
// migrated or aborted.
func legacyBlocked(snap storage.Snapshot, conversationID string) (bool, error) {
	run, ok, err := read[Run](snap, runKey(conversationID))
	if err != nil {
		return false, err
	}
	return ok && run.Phase != "done", nil
}

// ErrMigrationBlocked reports a legacy run that cannot be converted to tasks
// without inferring what an interrupted effect did.
var ErrMigrationBlocked = errors.New("continuous run cannot be migrated to task execution")

// migrateRunOps converts an unfinished legacy run into a chain with the same
// run ID and submissions. IDs, effective arguments, replay policies,
// approvals, results, and usage stay where they are; only the driver changes.
//
//	request phase       generation task at the same turn and attempt; the
//	                    attempt may have been sent and is resent, like a
//	                    legacy recovery, with its usage recorded as unknown
//	tools phase         generation waiting on one tool task per call:
//	  pending           tool task that has not started
//	  running           tool task with its effect marked started, resolved
//	                    through the committed replay policy, never assumed
//	                    not to have run
//	  done              terminal tool task naming the existing result entry
//	abort requested     every new task carries the abort mark
//
// A pending approval keeps its record; the migrated tool task finds it by
// run, call, and arguments and waits on it.
func migrateRunOps(snap storage.Snapshot, run Run) ([]storage.Operation, error) {
	c, err := conversation(snap, run.ConversationID)
	if err != nil {
		return nil, err
	}
	if run.Turn < 1 || run.Attempt < 1 || len(run.Submissions) == 0 {
		return nil, fmt.Errorf("%w: run %s has no valid turn, attempt, or submissions", ErrMigrationBlocked, run.ID)
	}
	revision := snap.Revision() + 1
	now := time.Now().UTC()
	gen := Task{ID: uuid.NewString(), ConversationID: c.ID, Kind: TaskKindGeneration, Version: generationVersion, State: "pending", Phase: "request", Created: now, Revision: revision, AbortRequested: run.AbortRequested}
	gen.Input, _ = json.Marshal(generationInput{RunID: run.ID})
	cp := generationCheckpoint{Turn: run.Turn, Attempt: run.Attempt, Cutoff: run.Cutoff, Compacted: run.Compacted}
	ops := []storage.Operation{}
	switch run.Phase {
	case "request":
		// The attempt may have been sent before the interruption.
		gen.State, gen.Effect = "running", effectStarted
	case "tools":
		assistant, err := lastEntryOfType(snap, c.ID, run.Cutoff, entryAssistant)
		if err != nil {
			return nil, fmt.Errorf("%w: tool round without its assistant entry: %v", ErrMigrationBlocked, err)
		}
		calls := map[string]provider.ToolCallBlock{}
		for _, block := range assistant.Content {
			if tc, ok := block.(provider.ToolCallBlock); ok {
				calls[tc.ID] = tc
			}
		}
		prev := ""
		var wait []string
		for _, intent := range run.Tools {
			call, ok := calls[intent.CallID]
			if !ok {
				return nil, fmt.Errorf("%w: tool intent %s without its call", ErrMigrationBlocked, intent.CallID)
			}
			tool := Task{ID: uuid.NewString(), ConversationID: c.ID, Kind: TaskKindTool, Version: toolVersion, Owner: gen.ID, State: "pending", Phase: "run", Created: now, Revision: revision, AbortRequested: run.AbortRequested}
			in := toolInput{RunID: run.ID, CallID: call.ID, Name: call.Name, Args: call.Arguments, Turn: run.Turn}
			tcp := toolCheckpoint{}
			switch intent.State {
			case "pending":
				if prev != "" {
					in.After = prev
					tool.Phase = "await"
				}
			case "running":
				if !json.Valid(intent.Args) {
					return nil, fmt.Errorf("%w: running intent %s without committed arguments", ErrMigrationBlocked, intent.CallID)
				}
				tcp = toolCheckpoint{Args: intent.Args, Replay: intent.Replay, Approval: intent.Approval}
				tool.State, tool.Effect = "running", effectStarted
			case "done":
				if intent.Entry == 0 || intent.Entry > c.EntrySequence {
					return nil, fmt.Errorf("%w: done intent %s without its result entry", ErrMigrationBlocked, intent.CallID)
				}
				tool.State, tool.Phase, tool.Outcome = "terminal", "", "completed"
				tool.Result, _ = json.Marshal(toolOutcome{Entry: intent.Entry, Status: "completed"})
				tool.AbortRequested = false
			default:
				return nil, fmt.Errorf("%w: unknown intent state %q", ErrMigrationBlocked, intent.State)
			}
			tool.Input, _ = json.Marshal(in)
			if tool.State != "terminal" {
				tool.Checkpoint, _ = json.Marshal(tcp)
			}
			ops = append(ops, record(taskKey(tool.ID), tool), record(taskConversationKey(c.ID, tool.ID), tool.ID), record(taskOwnerKey(gen.ID, tool.ID), tool.ID))
			cp.Calls = append(cp.Calls, toolRef{Task: tool.ID, CallID: call.ID})
			wait = append(wait, tool.ID)
			prev = tool.ID
		}
		gen.State, gen.Phase, gen.WaitOn, gen.WaitPolicy = "waiting", "collect", wait, "all"
	default:
		return nil, fmt.Errorf("%w: run phase %q", ErrMigrationBlocked, run.Phase)
	}
	gen.Checkpoint, _ = json.Marshal(cp)
	chain := Chain{ConversationID: c.ID, RunID: run.ID, Task: gen.ID, Submissions: run.Submissions, Notices: append(append([]string(nil), run.Notices...), "migrated from a legacy run in phase "+run.Phase), Started: now, Revision: revision}
	// The legacy run settles as done without changing its submissions: the
	// chain owns them now. Its outcome names the migration for audit.
	done := run
	done.Phase, done.Outcome, done.Error, done.Tools, done.Revision = "done", "migrated", "continued by task execution", nil, revision
	c.Revision = revision
	ops = append(ops, record(taskKey(gen.ID), gen), record(taskConversationKey(c.ID, gen.ID), gen.ID), record(chainKey(c.ID), chain), record(runKey(c.ID), done), record("conversation/"+c.ID, c))
	return ops, nil
}

// startChainOps claims every queued submission of c into a new chain with a
// new generation task. It returns nil when the queue is empty.
func startChainOps(snap storage.Snapshot, c Conversation) ([]storage.Operation, error) {
	queue, err := snap.Page("queue/"+c.ID+"/", "", 100)
	if err != nil {
		return nil, err
	}
	if len(queue) == 0 {
		return nil, nil
	}
	revision := snap.Revision() + 1
	chain := Chain{ConversationID: c.ID, RunID: uuid.NewString(), Started: time.Now().UTC(), Revision: revision}
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
		sub.State = "running"
		chain.Submissions = append(chain.Submissions, id)
		ops = append(ops, record("submission/"+id, sub), storage.Operation{Key: row.Key, Delete: true})
	}
	task := Task{ID: uuid.NewString(), ConversationID: c.ID, Kind: TaskKindGeneration, Version: generationVersion, State: "pending", Phase: "request", Created: time.Now().UTC(), Revision: revision}
	task.Input, _ = json.Marshal(generationInput{RunID: chain.RunID})
	task.Checkpoint, _ = json.Marshal(generationCheckpoint{Turn: 1, Attempt: 1, Cutoff: c.EntrySequence})
	chain.Task = task.ID
	ops = append(ops, formatOps(snap, 2)...)
	return append(ops, record(taskKey(task.ID), task), record(taskConversationKey(c.ID, task.ID), task.ID), record(chainKey(c.ID), chain)), nil
}

// AdmitQueued starts a chain for every conversation that has queued inputs
// and no active chain. Admission usually starts the chain in
// the submission commit; this covers inputs queued behind a finished chain
// by older code, migration, and handoff continuations. Returns the number of
// chains started.
func (r *Runtime) AdmitQueued(ctx context.Context) (int, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	after := ""
	for {
		page, err := snap.Page("queue/", after, 500)
		if err != nil {
			return 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			after = row.Key
			if parts := strings.SplitN(strings.TrimPrefix(row.Key, "queue/"), "/", 2); len(parts) == 2 {
				want[parts[0]] = true
			}
		}
	}
	started := 0
	for id := range want {
		for {
			snap, err := r.store.Snapshot(ctx)
			if err != nil {
				return started, err
			}
			held, err := legacyBlocked(snap, id)
			if err != nil {
				return started, err
			}
			if _, active := snap.Get(chainKey(id)); held || active {
				break
			}
			c, err := conversation(snap, id)
			if err != nil {
				return started, err
			}
			ops, err := startChainOps(snap, c)
			if err != nil || len(ops) == 0 {
				if err != nil {
					return started, err
				}
				break
			}
			err = r.commit(ctx, snap, "chain.start", ops...)
			if errors.Is(err, storage.ErrConflict) {
				continue
			}
			if err != nil {
				return started, err
			}
			started++
			break
		}
	}
	return started, nil
}

// NativeExecutor holds the built-in task definitions of execution. It
// reuses the host's Engine, core.Agent.Turn, summarization, and the core tool
// path; it adds no provider client and no second agent loop.
type NativeExecutor struct {
	r          *Runtime
	engine     func() Engine
	generation func() uint64
	opts       ExecutionOptions
	reg        TaskRegistry
	// MaxSubagentDepth bounds subagent nesting. Zero means 2.
	MaxSubagentDepth int
}

// NewNativeExecutor returns an executor whose definitions are added to a
// registry with Register. The engine function is read at each invocation so
// a reload applies to new work; generation, when set, names the engine
// generation recorded with each tool call.
func NewNativeExecutor(r *Runtime, engine func() Engine, opts ExecutionOptions) (*NativeExecutor, error) {
	if r == nil || engine == nil {
		return nil, ErrNoEngine
	}
	return &NativeExecutor{r: r, engine: engine, generation: func() uint64 { return 0 }, opts: opts.normalized()}, nil
}

// Scheduler returns a scheduler for the registry that also admits queued
// inputs at every tick.
func (x *NativeExecutor) Scheduler(reg TaskRegistry) *TaskScheduler {
	s := NewTaskScheduler(x.r, reg)
	var once sync.Once
	s.Prepare = func(ctx context.Context) (int, error) {
		var migrated int
		var err error
		once.Do(func() { migrated, _, err = x.r.MigrateLegacy(ctx) })
		if err != nil {
			once = sync.Once{}
			return 0, err
		}
		n, err := x.r.AdmitQueued(ctx)
		return n + migrated, err
	}
	return s
}

// Register adds the generation, tool, and compaction task definitions.
func (x *NativeExecutor) Register(reg TaskRegistry) error {
	x.reg = reg
	if err := reg.Register(TaskDefinition{
		Kind: TaskKindGeneration, Version: generationVersion, JoinOnAbort: true,
		Initial: func(json.RawMessage) (Next, error) {
			return Next{}, fmt.Errorf("generation tasks are created by admission only")
		},
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"request":   x.request,
			"collect":   x.collect,
			"compacted": x.compacted,
		},
		Abort: x.abortGeneration,
	}); err != nil {
		return err
	}
	if err := reg.Register(TaskDefinition{
		Kind: TaskKindTool, Version: toolVersion, JoinOnAbort: true,
		Initial: func(raw json.RawMessage) (Next, error) {
			var in toolInput
			if err := json.Unmarshal(raw, &in); err != nil || in.CallID == "" || in.Name == "" {
				return Next{}, fmt.Errorf("invalid tool task input")
			}
			if in.After != "" {
				return Next{Phase: "await", Checkpoint: toolCheckpoint{}}, nil
			}
			return Next{Phase: "run", Checkpoint: toolCheckpoint{}}, nil
		},
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"await": func(ctx context.Context, tc TaskContext) (Next, error) {
				var in toolInput
				if err := json.Unmarshal(tc.Task.Input, &in); err != nil {
					return Next{}, fmt.Errorf("%w: tool input", ErrTaskPermanent)
				}
				return Next{Phase: "run", WaitOn: []string{in.After}, Checkpoint: toolCheckpoint{}}, nil
			},
			"run":      x.runTool,
			"approved": x.runTool,
			"subagent": x.subagentResult,
		},
		Abort: x.abortTool,
	}); err != nil {
		return err
	}
	return reg.Register(TaskDefinition{
		Kind: TaskKindCompaction, Version: compactionVersion, JoinOnAbort: true,
		Initial: func(raw json.RawMessage) (Next, error) {
			return Next{Phase: "summarize"}, nil
		},
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"summarize": x.summarize,
		},
		Abort: func(ctx context.Context, tc TaskContext) (Next, error) {
			return Next{Outcome: "aborted", Error: "compaction aborted"}, nil
		},
		Retry: RetryPolicy{MaxAttempts: x.opts.MaxAttempts, Backoff: x.opts.RetryDelay, Retryable: core.RetryableProviderError},
	})
}

func decodeGeneration(t Task) (generationInput, generationCheckpoint, error) {
	var in generationInput
	var cp generationCheckpoint
	if err := json.Unmarshal(t.Input, &in); err != nil || in.RunID == "" {
		return in, cp, fmt.Errorf("%w: generation input", ErrTaskPermanent)
	}
	if err := json.Unmarshal(t.Checkpoint, &cp); err != nil || cp.Turn < 1 || cp.Attempt < 1 {
		return in, cp, fmt.Errorf("%w: generation checkpoint", ErrTaskPermanent)
	}
	return in, cp, nil
}

// request performs exactly one model attempt, or first a compaction.
func (x *NativeExecutor) request(ctx context.Context, tc TaskContext) (Next, error) {
	in, cp, err := decodeGeneration(tc.Task)
	if err != nil {
		return Next{}, err
	}
	c, err := conversation(tc.Snapshot, tc.Task.ConversationID)
	if err != nil {
		return Next{}, err
	}
	if _, err := checkBudgets(ctx, tc.Snapshot, c.ID, time.Now().UTC()); err != nil {
		if !errors.Is(err, ErrBudgetExceeded) {
			return Next{}, err
		}
		reason := err.Error()
		return Next{Commit: func(tx *TaskTx) (Next, error) {
			return x.finish(tx, nil, "failed", reason, "request refused: "+reason)
		}}, nil
	}
	policy := x.opts.Compaction
	// A background summary that finished is published here, at the request
	// boundary, if its source range is still the active context. A running
	// one does not block the request.
	if id, ok, err := read[string](tc.Snapshot, backgroundKey(c.ID)); err != nil {
		return Next{}, err
	} else if ok && !tc.Interrupted {
		bg, found, err := read[Task](tc.Snapshot, taskKey(id))
		if err != nil {
			return Next{}, err
		}
		if !found || bg.State == "terminal" {
			return Next{Commit: func(tx *TaskTx) (Next, error) {
				fresh, err := conversation(tx.Snapshot, c.ID)
				if err != nil {
					return Next{}, err
				}
				next := cp
				tx.Delete(backgroundKey(c.ID))
				notice := "background compaction finished without a summary"
				if found && bg.Outcome == "completed" {
					switch published, err := publishSummaryOps(ctx, tx, &fresh, bg); {
					case errors.Is(err, ErrCompactionStale):
						notice = "background compaction discarded as stale"
						stats, _, _ := read[CompactionMetrics](tx.View(), compactionStatsKey)
						stats.Stale++
						tx.Put(compactionStatsKey, stats)
					case err != nil:
						return Next{}, err
					case published:
						next.Cutoff = fresh.EntrySequence
						notice = "background compaction published before the request"
					}
				}
				x.noticeOps(tx, notice)
				return Next{Phase: "request", Checkpoint: next}, nil
			}}, nil
		}
	}
	agent, err := prepareAgent(ctx, x.engine(), tc.Snapshot, c, cp.Cutoff)
	if err != nil {
		return Next{}, err
	}
	if !tc.Interrupted && !cp.Compacted && needsCompaction(policy, agent.Messages(), agent.System) {
		// Blocking threshold compaction runs as an owned task; this
		// generation waits for it without holding a worker.
		return Next{Commit: func(tx *TaskTx) (Next, error) {
			id, err := tx.CreateChild(TaskSpec{Kind: TaskKindCompaction, Input: compactionInput{Reason: "threshold", KeepTokens: policy.KeepRecentTokens}})
			if err != nil {
				return Next{}, err
			}
			next := cp
			next.Compaction = id
			return Next{Phase: "compacted", Checkpoint: next, WaitOn: []string{id}}, nil
		}}, nil
	}
	_, backgroundRunning := tc.Snapshot.Get(backgroundKey(c.ID))
	startBackground := !tc.Interrupted && !backgroundRunning && policy.BackgroundTokens > 0 && estimateTokens(agent.Messages())+len(agent.System)/4 > policy.BackgroundTokens
	// Durable retries replace the agent's in-memory ones.
	agent.MaxRetries = 0
	interrupted := tc.Interrupted
	err = tc.StartEffect(ctx, nil, func(tx *TaskTx) error {
		if !interrupted {
			return nil
		}
		// An earlier invocation may have sent this attempt. Its cost is
		// unknown, never zero.
		fresh, err := conversation(tx.Snapshot, c.ID)
		if err != nil {
			return err
		}
		tx.Ops(usageOp(&fresh, in.RunID, cp.Turn, cp.Attempt, agent, provider.Usage{}, false))
		fresh.Revision = tx.Snapshot.Revision() + 1
		tx.Put("conversation/"+fresh.ID, fresh)
		return nil
	})
	if err != nil {
		return Next{}, err
	}
	var usage provider.Usage
	usageKnown := false
	var partial *taskPartial
	if x.opts.PartialFlushInterval >= 0 {
		partial = newTaskPartial(tc, x.opts.PartialFlushInterval, Partial{ConversationID: c.ID, RunID: in.RunID, Turn: cp.Turn, Attempt: cp.Attempt})
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
		x.opts.Sink(ev)
	})
	if partial != nil {
		partial.stop()
	}
	if ctx.Err() != nil {
		// Host shutdown or observer cancellation: the task stays running
		// with its effect marked started and resumes on the next pass.
		return Next{}, ctx.Err()
	}
	system, toolDefs, _ := agent.ContextSnapshot()
	toolJSON, _ := json.Marshal(toolDefs)
	names := make([]string, 0, len(toolDefs))
	for _, t := range toolDefs {
		names = append(names, t.Name)
	}
	failed := turnErr != nil || stop == provider.StopError || stop == provider.StopAborted
	var partialText string
	var partialTruncated bool
	if partial != nil {
		partialText, partialTruncated = partial.text()
	}
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		fresh, err := conversation(tx.Snapshot, c.ID)
		if err != nil {
			return Next{}, err
		}
		tx.Ops(usageOp(&fresh, in.RunID, cp.Turn, cp.Attempt, agent, usage, usageKnown))
		tx.Ops(promptOps(tx.Snapshot, fresh, Run{ID: in.RunID, Turn: cp.Turn, Attempt: cp.Attempt}, agent.Model, system, toolJSON, names)...)
		if startBackground {
			// The summary is a conversation-level background task: it
			// outlives this chain and is published at the conversation's
			// next request boundary.
			bg, ops, err := buildTask(x.reg, c.ID, "", TaskSpec{Kind: TaskKindCompaction, Input: compactionInput{Reason: "background", KeepTokens: policy.KeepRecentTokens}, Background: true}, tx.Snapshot.Revision()+1)
			if err != nil {
				return Next{}, err
			}
			tx.Ops(ops...)
			tx.Put(backgroundKey(c.ID), bg.ID)
		}
		aborting := tx.Task().AbortRequested
		if failed {
			reason := string(stop)
			if turnErr != nil {
				reason = turnErr.Error()
			}
			// The failed attempt's streamed text is retained as a final
			// partial for inspection, never as model context.
			if partial != nil && partialText != "" {
				tx.Put(partialKey(c.ID), Partial{ConversationID: c.ID, RunID: in.RunID, Turn: cp.Turn, Attempt: cp.Attempt, Text: partialText, Truncated: partialTruncated, Updated: time.Now().UTC(), Final: true})
			}
			fresh.EntrySequence++
			attempt := Entry{ID: uuid.NewString(), ConversationID: fresh.ID, Revision: tx.Snapshot.Revision() + 1, Type: entryAttempt, Content: reason, Time: time.Now().UTC()}
			if len(msg.Content) > 0 {
				attempt.Message = marshalMessage(msg)
			}
			tx.Put(entryKey(fresh.ID, fresh.EntrySequence), attempt)
			if aborting {
				return x.finish(tx, &fresh, "aborted", "aborted by request", "")
			}
			if turnErr != nil && !cp.Compacted && policy.ContextWindow > 0 && isContextOverflow(turnErr) {
				// Overflow: compact once, then resend without counting it
				// against the retry budget.
				fresh.Revision = tx.Snapshot.Revision() + 1
				tx.Put("conversation/"+fresh.ID, fresh)
				id, err := tx.CreateChild(TaskSpec{Kind: TaskKindCompaction, Input: compactionInput{Reason: "overflow", KeepTokens: policy.KeepRecentTokens}})
				if err != nil {
					return Next{}, err
				}
				next := cp
				next.Compaction, next.Overflow = id, reason
				return Next{Phase: "compacted", Checkpoint: next, WaitOn: []string{id}}, nil
			}
			if turnErr != nil && cp.Attempt < x.opts.MaxAttempts && core.RetryableProviderError(turnErr) {
				next := cp
				next.Attempt++
				fresh.Revision = tx.Snapshot.Revision() + 1
				tx.Put("conversation/"+fresh.ID, fresh)
				delay := x.opts.RetryDelay * time.Duration(1<<(cp.Attempt-1))
				return Next{Phase: "request", Checkpoint: next, WakeAt: time.Now().Add(delay)}, nil
			}
			return x.finish(tx, &fresh, "failed", reason, "")
		}
		// The response replaces the live partial atomically.
		if _, ok := tx.Snapshot.Get(partialKey(c.ID)); ok {
			tx.Delete(partialKey(c.ID))
		}
		fresh.EntrySequence++
		entry := Entry{ID: uuid.NewString(), ConversationID: fresh.ID, Revision: tx.Snapshot.Revision() + 1, Type: entryAssistant, Content: core.MessageText(msg), Time: time.Now().UTC(), Message: marshalMessage(msg)}
		tx.Put(entryKey(fresh.ID, fresh.EntrySequence), entry)
		var calls []provider.ToolCallBlock
		for _, block := range msg.Content {
			if call, ok := block.(provider.ToolCallBlock); ok && !call.Server {
				calls = append(calls, call)
			}
		}
		if stop != provider.StopToolUse || len(calls) == 0 {
			if aborting {
				return x.finish(tx, &fresh, "aborted", "aborted by request", "")
			}
			return x.finish(tx, &fresh, "answered", "", "")
		}
		// The response and its owned tool tasks commit together. Under a
		// pending abort the tool tasks start marked and settle as aborted
		// results without executing, keeping calls and results paired.
		next := generationCheckpoint{Turn: cp.Turn, Attempt: cp.Attempt, Cutoff: fresh.EntrySequence}
		var wait []string
		prev := ""
		for _, call := range calls {
			id, err := tx.CreateChild(TaskSpec{Kind: TaskKindTool, Input: toolInput{RunID: in.RunID, CallID: call.ID, Name: call.Name, Args: call.Arguments, After: prev, Turn: cp.Turn}})
			if err != nil {
				return Next{}, err
			}
			next.Calls = append(next.Calls, toolRef{Task: id, CallID: call.ID})
			wait = append(wait, id)
			prev = id
		}
		fresh.Revision = tx.Snapshot.Revision() + 1
		tx.Put("conversation/"+fresh.ID, fresh)
		return Next{Phase: "collect", Checkpoint: next, WaitOn: wait}, nil
	}}, nil
}

// compacted runs after a blocking compaction task settled: publish its
// summary and resend, or end the chain when an overflow cannot be fixed.
func (x *NativeExecutor) compacted(ctx context.Context, tc TaskContext) (Next, error) {
	_, cp, err := decodeGeneration(tc.Task)
	if err != nil {
		return Next{}, err
	}
	var task Task
	if len(tc.Waited) == 1 {
		task = tc.Waited[0]
	}
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		fresh, err := conversation(tx.Snapshot, tx.Task().ConversationID)
		if err != nil {
			return Next{}, err
		}
		if tx.Task().AbortRequested {
			return x.finish(tx, &fresh, "aborted", "aborted by request", "")
		}
		next := cp
		next.Compaction, next.Overflow, next.Compacted = "", "", true
		var cause error
		if task.Outcome == "completed" {
			published, err := publishSummaryOps(ctx, tx, &fresh, task)
			switch {
			case err == nil && published:
				next.Cutoff = fresh.EntrySequence
				switch compactionReason(task) {
				case "overflow":
					x.noticeOps(tx, "compacted after the provider rejected the context size")
				default:
					x.noticeOps(tx, "threshold compaction before the request")
				}
				return Next{Phase: "request", Checkpoint: next}, nil
			case err == nil:
				cause = errNothingToCompact
			case errors.Is(err, ErrCompactionStale):
				cause = err
			default:
				return Next{}, err
			}
		} else {
			cause = errors.New(firstNonEmpty(task.Error, "compaction "+task.Outcome))
		}
		if cp.Overflow != "" {
			return x.finish(tx, &fresh, "failed", fmt.Sprintf("%s (compaction after overflow failed: %v)", cp.Overflow, cause), "")
		}
		// A failed threshold compaction does not block the request.
		x.noticeOps(tx, "threshold compaction failed: "+cause.Error())
		return Next{Phase: "request", Checkpoint: next}, nil
	}}, nil
}

func compactionReason(t Task) string {
	var in compactionInput
	_ = json.Unmarshal(t.Input, &in)
	return in.Reason
}

// publishSummaryOps adds the publication of a compaction task's summary to
// tx. It reports false when the task produced no summary.
func publishSummaryOps(ctx context.Context, tx *TaskTx, c *Conversation, task Task) (bool, error) {
	var summary PendingSummary
	if len(task.Result) == 0 || json.Unmarshal(task.Result, &summary) != nil || summary.Summary == "" {
		return false, nil
	}
	ops, err := compactionOps(ctx, tx.View(), c, summary.pending())
	if err != nil {
		return false, err
	}
	tx.Ops(ops...)
	return true, nil
}

// summarize is the one model request of a compaction task. The summary is
// the task's result; the owning generation publishes it at a request
// boundary, so a restart does not lose a computed summary.
func (x *NativeExecutor) summarize(ctx context.Context, tc TaskContext) (Next, error) {
	var in compactionInput
	_ = json.Unmarshal(tc.Task.Input, &in)
	if err := tc.StartEffect(ctx, nil, nil); err != nil {
		return Next{}, err
	}
	pending, err := x.r.summarize(ctx, x.engine(), tc.Task.ConversationID, "", in.KeepTokens, firstNonEmpty(in.Reason, "threshold"), x.opts.Sink)
	if ctx.Err() != nil {
		return Next{}, ctx.Err()
	}
	if errors.Is(err, errNothingToCompact) {
		return Next{Outcome: "completed", Result: PendingSummary{}}, nil
	}
	if err != nil {
		return Next{}, err
	}
	return Next{Outcome: "completed", Result: pending.durable()}, nil
}

// noticeOps appends a recovery notice to the chain in tx.
func (x *NativeExecutor) noticeOps(tx *TaskTx, notice string) {
	chain, ok, _ := read[Chain](tx.View(), chainKey(tx.Task().ConversationID))
	if !ok {
		return
	}
	chain.Notices = append(chain.Notices, notice)
	chain.Revision = tx.Snapshot.Revision() + 1
	tx.Put(chainKey(chain.ConversationID), chain)
}

// collect runs after every tool task of the round is terminal. A handoff
// result ends the chain; tool tasks that ended without a result entry get
// an error result so the transcript stays paired.
func (x *NativeExecutor) collect(ctx context.Context, tc TaskContext) (Next, error) {
	_, cp, err := decodeGeneration(tc.Task)
	if err != nil {
		return Next{}, err
	}
	waited := tc.Waited
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		fresh, err := conversation(tx.Snapshot, tx.Task().ConversationID)
		if err != nil {
			return Next{}, err
		}
		repairToolResults(tx, &fresh, cp.Calls, waited)
		for _, t := range waited {
			var out toolOutcome
			if json.Unmarshal(t.Result, &out) == nil && out.Handoff {
				fresh.Revision = tx.Snapshot.Revision() + 1
				tx.Put("conversation/"+fresh.ID, fresh)
				return x.finish(tx, &fresh, "answered", "", "handoff: context reset, continuation admitted")
			}
		}
		if tx.Task().AbortRequested {
			return x.finish(tx, &fresh, "aborted", "aborted by request", "")
		}
		if cp.Turn >= x.opts.MaxTurns {
			return x.finish(tx, &fresh, "failed", fmt.Sprintf("max turns (%d) exceeded", x.opts.MaxTurns), "")
		}
		chain, ok, err := read[Chain](tx.View(), chainKey(fresh.ID))
		if err != nil || !ok {
			return Next{}, fmt.Errorf("%w: generation without chain", storage.ErrCorrupt)
		}
		run := Run{Submissions: chain.Submissions, Turn: cp.Turn}
		steer, err := claimSteeringOps(tx.Snapshot, &fresh, &run)
		if err != nil {
			return Next{}, err
		}
		tx.Ops(steer...)
		chain.Submissions, chain.Notices = run.Submissions, append(chain.Notices, run.Notices...)
		chain.Revision = tx.Snapshot.Revision() + 1
		tx.Put(chainKey(fresh.ID), chain)
		fresh.Revision = tx.Snapshot.Revision() + 1
		tx.Put("conversation/"+fresh.ID, fresh)
		return Next{Phase: "request", Checkpoint: generationCheckpoint{Turn: cp.Turn + 1, Attempt: 1, Cutoff: fresh.EntrySequence}}, nil
	}}, nil
}

// abortGeneration settles the chain aborted. It runs after every owned tool
// task settled, so every call of the round already has a result.
func (x *NativeExecutor) abortGeneration(ctx context.Context, tc TaskContext) (Next, error) {
	in, cp, _ := decodeGeneration(tc.Task)
	interrupted := tc.Task.Effect == effectStarted && tc.Task.Phase == "request"
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		fresh, err := conversation(tx.Snapshot, tx.Task().ConversationID)
		if err != nil {
			return Next{}, err
		}
		if interrupted && in.RunID != "" {
			// A request may have been sent before the abort; its usage is
			// unknown.
			fresh.UsageSequence++
			tx.Put(usageKey(fresh.ID, fresh.UsageSequence), UsageRecord{ConversationID: fresh.ID, RunID: in.RunID, Turn: max(cp.Turn, 1), Attempt: max(cp.Attempt, 1), Provider: fresh.Config.Provider, Model: fresh.Config.Model, Status: "unknown", Source: "model", Time: time.Now().UTC()})
		}
		repairToolResults(tx, &fresh, cp.Calls, nil)
		return x.finish(tx, &fresh, "aborted", "aborted by request", "")
	}}, nil
}

// repairToolResults writes an error result for every call of the round whose
// tool task ended without one. waited, when set, are the terminal tasks.
func repairToolResults(tx *TaskTx, c *Conversation, calls []toolRef, waited []Task) {
	byID := map[string]Task{}
	for _, t := range waited {
		byID[t.ID] = t
	}
	for _, ref := range calls {
		t, ok := byID[ref.Task]
		if !ok {
			t, _, _ = read[Task](tx.Snapshot, taskKey(ref.Task))
		}
		var out toolOutcome
		if len(t.Result) > 0 && json.Unmarshal(t.Result, &out) == nil && out.Entry > 0 {
			continue
		}
		text := "failed: the tool call ended without a result"
		if t.Error != "" {
			text = "failed: " + t.Error
		}
		c.EntrySequence++
		block := provider.ToolResultBlock{CallID: ref.CallID, Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: tx.Snapshot.Revision() + 1, Type: entryToolResult, Content: text, Time: time.Now().UTC()}
		entry.Message = marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}, Time: time.Now().UTC()})
		tx.Put(entryKey(c.ID, c.EntrySequence), entry)
	}
}

// finish settles the chain's submissions with state and ends the generation
// task. Inputs queued meanwhile start the next chain in the same commit, so
// queue ordering survives a crash between the two. Pending approvals of the
// chain are expired.
func (x *NativeExecutor) finish(tx *TaskTx, c *Conversation, state, reason, notice string) (Next, error) {
	view := tx.View()
	if c == nil {
		fresh, err := conversation(view, tx.Task().ConversationID)
		if err != nil {
			return Next{}, err
		}
		c = &fresh
	}
	chain, ok, err := read[Chain](view, chainKey(c.ID))
	if err != nil || !ok || chain.Task != tx.Task().ID {
		return Next{}, fmt.Errorf("%w: generation without chain", storage.ErrCorrupt)
	}
	tx.Ops(settleOps(view, chain.Submissions, state)...)
	pending, err := pendingApprovals(view, c.ID)
	if err != nil {
		return Next{}, err
	}
	for _, a := range pending {
		if a.RunID == chain.RunID {
			a.State, a.Reason, a.Revision = approvalExpired, "run "+state, tx.Snapshot.Revision()+1
			tx.Put(approvalKey(a.ID), a)
		}
	}
	// A live partial of this chain is retained as final output, so no live
	// record outlives its chain.
	if p, ok, _ := read[Partial](view, partialKey(c.ID)); ok && !p.Final {
		p.Final, p.Updated = true, time.Now().UTC()
		tx.Put(partialKey(c.ID), p)
	}
	c.Revision = tx.Snapshot.Revision() + 1
	tx.Put("conversation/"+c.ID, *c)
	// Settlement is applied to the view, so queue rows of the settled
	// chain are already gone and only new inputs are claimed.
	next, err := startChainOps(tx.View(), *c)
	if err != nil {
		return Next{}, err
	}
	if next == nil {
		tx.Delete(chainKey(c.ID))
	}
	tx.Ops(next...)
	for _, op := range next {
		if strings.HasPrefix(op.Key, "task/") {
			tx.declare(strings.TrimPrefix(op.Key, "task/"))
		}
	}
	outcome := "completed"
	switch state {
	case "failed":
		outcome = "failed"
	case "aborted":
		outcome = "aborted"
	}
	result := map[string]any{"run_id": chain.RunID, "state": state, "turn": max(1, chainTurn(tx.Task())), "attempt": max(1, chainAttempt(tx.Task())), "submissions": chain.Submissions}
	if len(chain.Notices) > 0 {
		result["notices"] = chain.Notices
	}
	if notice != "" {
		result["notice"] = notice
	}
	return Next{Outcome: outcome, Error: reason, Result: result}, nil
}

func decodeTool(t Task) (toolInput, toolCheckpoint, error) {
	var in toolInput
	var cp toolCheckpoint
	if err := json.Unmarshal(t.Input, &in); err != nil || in.CallID == "" || in.RunID == "" {
		return in, cp, fmt.Errorf("%w: tool input", ErrTaskPermanent)
	}
	if len(t.Checkpoint) > 0 && string(t.Checkpoint) != "null" {
		if err := json.Unmarshal(t.Checkpoint, &cp); err != nil {
			return in, cp, fmt.Errorf("%w: tool checkpoint", ErrTaskPermanent)
		}
	}
	return in, cp, nil
}

// runTool authorizes, resolves approval, commits the effective intent, and
// executes one call. A call found with its effect started is resolved
// through its replay policy, never by assuming it did not run.
func (x *NativeExecutor) runTool(ctx context.Context, tc TaskContext) (Next, error) {
	in, cp, err := decodeTool(tc.Task)
	if err != nil {
		return Next{}, err
	}
	c, err := conversation(tc.Snapshot, tc.Task.ConversationID)
	if err != nil {
		return Next{}, err
	}
	agent, err := prepareAgent(ctx, x.engine(), tc.Snapshot, c, 0)
	if err != nil {
		return Next{}, err
	}
	identity := ToolCallIdentity{RunID: in.RunID, ConversationID: c.ID, CallID: in.CallID}
	call := provider.ToolCallBlock{ID: in.CallID, Name: in.Name, Arguments: in.Args}
	var result core.ToolResult
	status, notice := "", ""
	executed := false
	switch {
	case tc.Interrupted && in.Name == subagentToolName:
		// A subagent call is replay safe by construction: the owned
		// conversation and its submission are found again by key.
		return x.startSubagent(ctx, tc, in, cp, c)
	case tc.Interrupted:
		decision, replayResult, replayed := recoverCall(ctx, x.r, agent, identity, ToolIntent{CallID: in.CallID, Name: in.Name, Args: cp.Args, Replay: cp.Replay}, call, x.opts.Sink)
		if ctx.Err() != nil {
			return Next{}, ctx.Err()
		}
		// A reconcilable call whose reconciliation could not answer yet (the
		// worker is unreachable) is asked again later, never assumed.
		unanswered := !replayed && replayResult.Content == nil && cp.Replay == core.ReplayReconcile && strings.Contains(decision, "reconciliation")
		if (unanswered || (replayed && replayResult.Status == "unknown")) && cp.Unresolved < maxUnresolved {
			return x.parkUnresolved(cp, decision), nil
		}
		notice = decision
		switch {
		case replayed:
			result, executed = replayResult, true
		case replayResult.Content != nil:
			result = replayResult
		default:
			result = core.ToolResult{IsError: true, Status: "interrupted", Content: []provider.Content{provider.TextBlock{Text: "interrupted: the process stopped after this tool call started and before its result was recorded. Its effect is unknown and it was not repeated automatically."}}}
			status = "interrupted"
		}
		call.Arguments = cp.Args
	default:
		args, allowed, reason, policy := authorizeCall(ctx, x.r, agent, c.ID, call)
		if !allowed {
			result = core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: reason}}}
			status = "blocked"
			break
		}
		approvalID := ""
		if x.opts.Approver != nil {
			run := Run{ID: in.RunID, ConversationID: c.ID}
			pending, approved, decided, err := approvalFor(tc.Snapshot, c, run, call, args)
			if err != nil {
				return Next{}, err
			}
			switch {
			case pending != nil:
				return Next{Phase: "approved", WaitApproval: pending.ID, Checkpoint: cp}, nil
			case decided != nil && !approved:
				result = core.ToolResult{IsError: true, Status: "blocked", Content: []provider.Content{provider.TextBlock{Text: "denied: " + firstNonEmpty(decided.Reason, "a human denied this tool call")}}}
				status = "blocked"
			case approved:
				approvalID = decided.ID
			default:
				if req, need := x.opts.Approver(ctx, c, call, args); need {
					return x.requestApproval(c, in, cp, call, args, req), nil
				}
			}
			if status == "blocked" {
				break
			}
		}
		if in.Name == subagentToolName {
			cp.Args, cp.Replay, cp.Approval = args, core.ReplaySafe, approvalID
			return x.startSubagent(ctx, tc, in, cp, c)
		}
		cp = toolCheckpoint{Args: args, Replay: policy, Approval: approvalID, Generation: x.generation()}
		if err := tc.StartEffect(ctx, cp, nil); err != nil {
			return Next{}, err
		}
		call.Arguments = args
		progress := newToolProgress(tc, in.CallID)
		result = executeCall(ctx, agent, identity, call, func(ev core.AgentEvent) {
			if p, ok := ev.(core.EvToolProgress); ok {
				progress.add(p.Text)
			}
			x.opts.Sink(ev)
		})
		progress.flush()
		executed = true
		if ctx.Err() != nil {
			return Next{}, ctx.Err()
		}
	}
	if result.Status == "unknown" && cp.Replay == core.ReplayReconcile && cp.Unresolved < maxUnresolved {
		// The effect may still complete on the other side. Keep the
		// committed intent and reconcile later through the operation key.
		return x.parkUnresolved(cp, fmt.Sprintf("tool %s outcome unknown: %s", call.ID, core.ToolResultText(result))), nil
	}
	if result.Status == "unknown" {
		// A lost outcome is reported, never retried as if it failed.
		notice = fmt.Sprintf("tool %s outcome unknown: %s", call.ID, core.ToolResultText(result))
		result = core.ToolResult{IsError: true, Status: "unknown", Content: []provider.Content{provider.TextBlock{Text: "outcome unknown: the tool lost track of this operation. It was not repeated automatically. Its effect may have happened."}}}
		status = "unknown"
	}
	if status == "" {
		status = "completed"
		if result.IsError {
			status = "failed"
		}
	}
	ev := core.EvToolResult{ID: call.ID, Name: call.Name, Args: call.Arguments, Status: status, Executed: executed, Result: result}
	req, handoff := handoffFromResult(call.Name, result)
	return Next{
		Commit: func(tx *TaskTx) (Next, error) {
			if handoff && !tx.Task().AbortRequested {
				return commitHandoff(tx, in, call.ID, result, req)
			}
			return commitToolResult(tx, call.ID, result, status, notice, "completed")
		},
		AfterCommit: func() { x.opts.Sink(ev) },
	}, nil
}

// parkUnresolved keeps the effect marked started and wakes the task later to
// reconcile. Backoff doubles per pass.
func (x *NativeExecutor) parkUnresolved(cp toolCheckpoint, notice string) Next {
	cp.Unresolved++
	delay := x.opts.RetryDelay * time.Duration(1<<min(cp.Unresolved-1, 10))
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		x.noticeOps(tx, notice)
		return Next{Phase: "run", Checkpoint: cp, WakeAt: time.Now().Add(delay), KeepEffect: true}, nil
	}}
}

// requestApproval commits a pending approval and parks the tool task on it,
// together with the outbox notification. Nothing executes.
func (x *NativeExecutor) requestApproval(c Conversation, in toolInput, cp toolCheckpoint, call provider.ToolCallBlock, args json.RawMessage, req ApprovalRequest) Next {
	var a Approval
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		fresh, err := conversation(tx.Snapshot, c.ID)
		if err != nil {
			return Next{}, err
		}
		a = Approval{ID: uuid.NewString(), ConversationID: c.ID, RunID: in.RunID, CallID: call.ID, Tool: call.Name, Args: args, ArgsHash: argsHash(args), Summary: req.Summary, State: approvalPending, PolicyRevision: fresh.ConfigRevision, Created: time.Now().UTC(), Revision: tx.Snapshot.Revision() + 1}
		if req.TTL > 0 {
			exp := a.Created.Add(req.TTL)
			a.ExpiresAt = &exp
		}
		tx.Put(approvalKey(a.ID), a)
		tx.Put(approvalConversationKey(c.ID, a.ID), a.ID)
		tx.Ops(outboxOp(approvalNotificationID(a.ID), "approval.pending", c.ID, fmt.Sprintf("approval needed: %s", firstNonEmpty(req.Summary, call.Name)), map[string]any{"approval_id": a.ID, "tool": call.Name, "run_id": in.RunID}))
		x.noticeOps(tx, fmt.Sprintf("tool %s is waiting for approval %s", call.ID, a.ID))
		return Next{Phase: "approved", WaitApproval: a.ID, Checkpoint: cp}, nil
	}, AfterCommit: func() { x.opts.Sink(EvApproval{Approval: a}) }}
}

// commitHandoff ends the round with a handoff: the result, aborted results
// for the remaining calls, the reset entry, and the continuation admission
// commit together. The remaining sibling tool tasks are aborted in the same
// commit, so none of them executes.
func commitHandoff(tx *TaskTx, in toolInput, callID string, result core.ToolResult, req handoffRequest) (Next, error) {
	next, err := commitToolResult(tx, callID, result, "completed", "", "completed")
	if err != nil {
		return Next{}, err
	}
	view := tx.View()
	c, err := conversation(view, tx.Task().ConversationID)
	if err != nil {
		return Next{}, err
	}
	gen, ok, err := read[Task](view, taskKey(tx.Task().Owner))
	if err != nil || !ok {
		return Next{}, fmt.Errorf("%w: tool task without generation", storage.ErrCorrupt)
	}
	_, cp, err := decodeGeneration(gen)
	if err != nil {
		return Next{}, err
	}
	after := false
	for _, ref := range cp.Calls {
		if ref.Task == tx.Task().ID {
			after = true
			continue
		}
		if !after {
			continue
		}
		sibling, ok, err := read[Task](view, taskKey(ref.Task))
		if err != nil || !ok || sibling.State == "terminal" {
			continue
		}
		text := "skipped: the model handed off to a fresh context before this call ran."
		c.EntrySequence++
		block := provider.ToolResultBlock{CallID: ref.CallID, Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: tx.Snapshot.Revision() + 1, Type: entryToolResult, Content: text, Time: time.Now().UTC()}
		entry.Message = marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}, Time: time.Now().UTC()})
		tx.Put(entryKey(c.ID, c.EntrySequence), entry)
		sibling.State, sibling.Outcome, sibling.Error = "terminal", "aborted", "skipped by handoff"
		sibling.Phase, sibling.Checkpoint, sibling.WaitOn, sibling.WaitPolicy, sibling.WakeAt, sibling.Effect = "", nil, nil, "", nil, ""
		sibling.Result, _ = json.Marshal(toolOutcome{Entry: c.EntrySequence, Status: "skipped"})
		sibling.Revision = tx.Snapshot.Revision() + 1
		tx.Put(taskKey(sibling.ID), sibling)
	}
	ops, err := handoffOps(view, &c, Run{ID: in.RunID}, callID, req)
	if err != nil {
		return Next{}, err
	}
	tx.Ops(ops...)
	c.Revision = tx.Snapshot.Revision() + 1
	tx.Put("conversation/"+c.ID, c)
	var out toolOutcome
	raw, _ := json.Marshal(next.Result)
	_ = json.Unmarshal(raw, &out)
	out.Handoff = true
	next.Result = out
	return next, nil
}

// abortTool settles an aborted call with an error result. Nothing executes.
// A subagent call's owned conversation was aborted with it (Linked).
func (x *NativeExecutor) abortTool(ctx context.Context, tc TaskContext) (Next, error) {
	in, _, err := decodeTool(tc.Task)
	if err != nil {
		return Next{Outcome: "aborted", Error: err.Error()}, nil
	}
	text := "aborted: the run was aborted before this tool call started."
	notice := ""
	if tc.Task.Effect == effectStarted || tc.Task.Phase == "subagent" {
		text = "aborted: the run was aborted after this tool call started. Its effect is unknown."
		notice = fmt.Sprintf("tool %s aborted while running, effect unknown", in.CallID)
	}
	result := core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: text}}}
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		return commitToolResult(tx, in.CallID, result, "aborted", notice, "aborted")
	}}, nil
}

func commitToolResult(tx *TaskTx, callID string, result core.ToolResult, status, notice, outcome string) (Next, error) {
	c, err := conversation(tx.View(), tx.Task().ConversationID)
	if err != nil {
		return Next{}, err
	}
	c.EntrySequence++
	block := provider.ToolResultBlock{CallID: callID, Content: result.Content, IsError: result.IsError}
	entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: tx.Snapshot.Revision() + 1, Type: entryToolResult, Content: core.ToolResultText(result), Time: time.Now().UTC()}
	entry.Message = marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}, Time: time.Now().UTC()})
	tx.Put(entryKey(c.ID, c.EntrySequence), entry)
	c.Revision = tx.Snapshot.Revision() + 1
	tx.Put("conversation/"+c.ID, c)
	// The progress record of this call is replaced by its result.
	if _, ok := tx.Snapshot.Get(progressKey(tx.Task().ID)); ok {
		tx.Delete(progressKey(tx.Task().ID))
	}
	next := Next{Outcome: outcome, Result: toolOutcome{Entry: c.EntrySequence, Status: status}}
	if outcome == "aborted" {
		next.Error = "aborted by request"
	}
	if notice != "" {
		// Recovery decisions are for humans: recorded on the chain, never
		// model-visible.
		next.Error = notice
		if chain, ok, _ := read[Chain](tx.View(), chainKey(c.ID)); ok {
			chain.Notices = append(chain.Notices, notice)
			chain.Revision = tx.Snapshot.Revision() + 1
			tx.Put(chainKey(c.ID), chain)
		}
	}
	return next, nil
}

const subagentToolName = "subagent"

// startSubagent creates (or finds again) the owned conversation and its
// submission in one commit and parks the tool task on the child's
// generation, without occupying a worker. The child's generation is linked:
// aborting this call aborts the child, and the call settles only after the
// child did.
func (x *NativeExecutor) startSubagent(ctx context.Context, tc TaskContext, in toolInput, cp toolCheckpoint, c Conversation) (Next, error) {
	var args struct {
		Task string `json:"task"`
		Key  string `json:"key"`
	}
	raw := cp.Args
	if len(raw) == 0 {
		raw = in.Args
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.Task == "" {
		result := core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "subagent requires a task"}}}
		return Next{Commit: func(tx *TaskTx) (Next, error) {
			return commitToolResult(tx, in.CallID, result, "failed", "", "completed")
		}}, nil
	}
	maxDepth := x.MaxSubagentDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	depth, err := ownershipDepth(tc.Snapshot, c.ID)
	if err != nil {
		return Next{}, err
	}
	if depth >= maxDepth {
		result := core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("subagent nesting limit %d reached", maxDepth)}}}
		return Next{Commit: func(tx *TaskTx) (Next, error) {
			return commitToolResult(tx, in.CallID, result, "blocked", "", "completed")
		}}, nil
	}
	key := args.Key
	if key == "" {
		key = hashedKey("", args.Task)
	}
	owner := ToolCallIdentity{RunID: in.RunID, ConversationID: c.ID, CallID: in.CallID}.OwnerID()
	return Next{Commit: func(tx *TaskTx) (Next, error) {
		view := tx.View()
		if tx.Task().AbortRequested {
			return Next{}, ErrTaskAborting
		}
		childID, existed, err := read[string](view, hashedKey("owned-key/", owner, key))
		if err != nil {
			return Next{}, err
		}
		if !existed {
			child, ops, err := ownedConversationOps(view, c.ID, owner, key, nil)
			if err != nil {
				return Next{}, err
			}

			tx.Ops(ops...)
			childID = child.ID
			view = tx.View()
		}
		child, err := conversation(view, childID)
		if err != nil {
			return Next{}, err
		}
		dedup := hashedKey("dedup/submit/", childID, "subagent:"+owner, "subagent:"+key)
		sub, admitted, err := read[Submission](view, dedup)
		if err != nil {
			return Next{}, err
		}
		if !admitted {
			var ops []storage.Operation
			sub, ops, err = admissionOps(view, &child, "subagent:"+owner, "subagent:"+key, args.Task, "")
			if err != nil {
				return Next{}, err
			}
			tx.Ops(ops...)
			tx.Put("conversation/"+child.ID, child)
			if _, active := view.Get(chainKey(child.ID)); !active {
				start, err := startChainOps(tx.View(), child)
				if err != nil {
					return Next{}, err
				}
				tx.Ops(start...)
			}
		}
		next := cp
		next.Subagent, next.Waiting = childID, sub.ID
		chain, ok, err := read[Chain](tx.View(), chainKey(childID))
		if err != nil {
			return Next{}, err
		}
		if !ok {
			// Already settled: collect the answer now.
			return Next{Phase: "subagent", Checkpoint: next, WakeAt: time.Now()}, nil
		}
		tx.declare(chain.Task)
		return Next{Phase: "subagent", Checkpoint: next, WaitOn: []string{chain.Task}, Link: []string{chain.Task}}, nil
	}, AfterCommit: func() {
		x.opts.Sink(core.EvToolProgress{ID: in.CallID, Text: "subagent started\n"})
	}}, nil
}

// subagentResult runs when the child's chain settled. A queued follow-up of
// the child may still be answering the submission; then the call waits for
// the next chain.
func (x *NativeExecutor) subagentResult(ctx context.Context, tc TaskContext) (Next, error) {
	in, cp, err := decodeTool(tc.Task)
	if err != nil {
		return Next{}, err
	}
	sub, ok, err := read[Submission](tc.Snapshot, "submission/"+cp.Waiting)
	if err != nil || !ok {
		return Next{}, fmt.Errorf("%w: subagent submission missing", ErrTaskPermanent)
	}
	if sub.State == "queued" || sub.State == "running" {
		chain, active, err := read[Chain](tc.Snapshot, chainKey(cp.Subagent))
		if err != nil {
			return Next{}, err
		}
		if active {
			return Next{Phase: "subagent", Checkpoint: cp, WaitOn: []string{chain.Task}, Link: []string{chain.Task}}, nil
		}
		return Next{Phase: "subagent", Checkpoint: cp, WakeAt: time.Now().Add(x.opts.RetryDelay)}, nil
	}
	details := map[string]string{"conversation": cp.Subagent}
	result := core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("subagent %s: %s", cp.Subagent, sub.State)}}, Details: details}
	if sub.State == "answered" {
		child, err := conversation(tc.Snapshot, cp.Subagent)
		if err != nil {
			return Next{}, err
		}
		answer, err := lastEntryOfType(tc.Snapshot, child.ID, child.EntrySequence, entryAssistant)
		if err != nil {
			return Next{}, err
		}
		result = core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: core.MessageText(answer)}}, Details: details}
	}
	status := "completed"
	if result.IsError {
		status = "failed"
	}
	ev := core.EvToolResult{ID: in.CallID, Name: in.Name, Args: cp.Args, Status: status, Executed: true, Result: result}
	return Next{
		Commit: func(tx *TaskTx) (Next, error) {
			return commitToolResult(tx, in.CallID, result, status, "", "completed")
		},
		AfterCommit: func() { x.opts.Sink(ev) },
	}, nil
}

// taskPartial persists streamed text of a generation attempt in bounded
// batches through fenced task-scoped commits. A flush of a stale invocation
// is refused by the fence, so it cannot overwrite a newer attempt or a
// terminal result.
type taskPartial struct {
	tc       TaskContext
	base     Partial
	interval time.Duration
	mu       sync.Mutex
	buf      strings.Builder
	dirty    bool
	trunc    bool
	quit     chan struct{}
	done     chan struct{}
}

func newTaskPartial(tc TaskContext, interval time.Duration, base Partial) *taskPartial {
	if interval <= 0 {
		interval = PartialFlushInterval
	}
	p := &taskPartial{tc: tc, base: base, interval: interval, quit: make(chan struct{}), done: make(chan struct{})}
	go p.loop()
	return p
}

func (p *taskPartial) add(delta string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if room := MaxPartialBytes - p.buf.Len(); room <= 0 {
		p.trunc = true
		return
	} else if len(delta) > room {
		delta, p.trunc = delta[:room], true
	}
	p.buf.WriteString(delta)
	p.dirty = true
}

func (p *taskPartial) text() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String(), p.trunc
}

func (p *taskPartial) loop() {
	defer close(p.done)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-p.quit:
			return
		case <-t.C:
			p.flush()
		}
	}
}

func (p *taskPartial) flush() {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return
	}
	rec := p.base
	rec.Text, rec.Truncated, rec.Updated = p.buf.String(), p.trunc, time.Now().UTC()
	p.dirty = false
	p.mu.Unlock()
	_ = p.tc.Commit(context.Background(), func(tx *TaskTx) error {
		tx.Put(partialKey(rec.ConversationID), rec)
		return nil
	})
}

func (p *taskPartial) stop() {
	close(p.quit)
	<-p.done
}

// ToolProgress is the bounded, committed progress of a running tool task.
type ToolProgress struct {
	TaskID    string    `json:"task_id"`
	CallID    string    `json:"call_id"`
	Text      string    `json:"text"`
	Truncated bool      `json:"truncated,omitempty"`
	Updated   time.Time `json:"updated"`
}

func progressKey(taskID string) string { return "progress/" + taskID }

// MaxToolProgressBytes bounds committed tool progress; the tail is kept.
const MaxToolProgressBytes = 16 << 10

// toolProgress persists progress at most every PartialFlushInterval through
// fenced task-scoped commits, so progress of a stale invocation is refused
// and the result commit removes it atomically.
type toolProgress struct {
	tc    TaskContext
	call  string
	mu    sync.Mutex
	text  string
	trunc bool
	last  time.Time
}

func newToolProgress(tc TaskContext, callID string) *toolProgress {
	return &toolProgress{tc: tc, call: callID}
}

func (p *toolProgress) add(text string) {
	p.mu.Lock()
	p.text += text
	if len(p.text) > MaxToolProgressBytes {
		p.text, p.trunc = p.text[len(p.text)-MaxToolProgressBytes:], true
	}
	due := time.Since(p.last) >= PartialFlushInterval
	p.mu.Unlock()
	if due {
		p.flush()
	}
}

func (p *toolProgress) flush() {
	p.mu.Lock()
	rec := ToolProgress{TaskID: p.tc.Task.ID, CallID: p.call, Text: p.text, Truncated: p.trunc, Updated: time.Now().UTC()}
	p.last = rec.Updated
	p.mu.Unlock()
	if rec.Text == "" {
		return
	}
	_ = p.tc.Commit(context.Background(), func(tx *TaskTx) error {
		tx.Put(progressKey(rec.TaskID), rec)
		return nil
	})
}

// TaskView is the committed view of execution of one
// conversation: the chain, live tasks with ownership and dependencies, and
// the run-shaped projection for older clients.
type TaskView struct {
	Chain *Chain `json:"chain,omitempty"`
	Run   *Run   `json:"run,omitempty"`
	Tasks []Task `json:"tasks"`
	// Edges lists owner -> child and waiter -> dependency relations of the
	// live tasks, for inspection.
	Edges []TaskEdge `json:"edges,omitempty"`
}

// TaskEdge is one relation between tasks.
type TaskEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind is owns, waits, or links.
	Kind string `json:"kind"`
}

// TaskView reads the committed task view of a conversation.
func (r *Runtime) TaskView(ctx context.Context, conversationID string) (TaskView, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return TaskView{}, err
	}
	view := TaskView{Tasks: []Task{}}
	if chain, ok, err := read[Chain](snap, chainKey(conversationID)); err != nil {
		return TaskView{}, err
	} else if ok {
		view.Chain = &chain
		run := chainRun(snap, chain)
		view.Run = &run
	}
	rows, err := snap.Page("task-conversation/"+conversationID+"/", "", 1000)
	if err != nil {
		return TaskView{}, err
	}
	for _, row := range rows {
		var id string
		if json.Unmarshal(row.Value, &id) != nil {
			return TaskView{}, storage.ErrCorrupt
		}
		t, ok, err := read[Task](snap, taskKey(id))
		if err != nil || !ok {
			return TaskView{}, fmt.Errorf("%w: listed task missing", storage.ErrCorrupt)
		}
		if t.State == "terminal" {
			continue
		}
		view.Tasks = append(view.Tasks, t)
		if t.Owner != "" {
			view.Edges = append(view.Edges, TaskEdge{From: t.Owner, To: t.ID, Kind: "owns"})
		}
		for _, dep := range t.WaitOn {
			view.Edges = append(view.Edges, TaskEdge{From: t.ID, To: dep, Kind: "waits"})
		}
		for _, l := range t.Linked {
			view.Edges = append(view.Edges, TaskEdge{From: t.ID, To: l, Kind: "links"})
		}
	}
	sortTasks(view.Tasks)
	return view, nil
}

// overlaySnapshot is a snapshot with uncommitted operations applied, so a
// builder can compose with operations earlier in the same commit.
type overlaySnapshot struct {
	storage.Snapshot
	ops map[string]storage.Operation
}

func pendingSnapshot(snap storage.Snapshot, ops []storage.Operation) storage.Snapshot {
	o := &overlaySnapshot{Snapshot: snap, ops: map[string]storage.Operation{}}
	for _, op := range ops {
		o.ops[op.Key] = op
	}
	return o
}

func (o *overlaySnapshot) Get(key string) (json.RawMessage, bool) {
	if op, ok := o.ops[key]; ok {
		if op.Delete {
			return nil, false
		}
		return op.Value, true
	}
	return o.Snapshot.Get(key)
}

func (o *overlaySnapshot) Page(prefix, after string, limit int) ([]storage.Record, error) {
	// Overlaid deletes can hide base rows, so fetch extra, within the
	// store's page bound. Callers page on, so a short page is only slower.
	fetch := min(limit+len(o.ops), 1000)
	base, err := o.Snapshot.Page(prefix, after, fetch)
	if err != nil {
		return nil, err
	}
	// A full base page covers keys only up to its last one; overlay keys
	// past it belong to a later page, or base rows would be skipped.
	bound := ""
	if len(base) == fetch {
		bound = base[len(base)-1].Key
	}
	merged := map[string]json.RawMessage{}
	for _, r := range base {
		merged[r.Key] = r.Value
	}
	for k, op := range o.ops {
		if !strings.HasPrefix(k, prefix) || k <= after || (bound != "" && k > bound) {
			continue
		}
		if op.Delete {
			delete(merged, k)
		} else {
			merged[k] = op.Value
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]storage.Record, len(keys))
	for i, k := range keys {
		out[i] = storage.Record{Key: k, Value: merged[k]}
	}
	return out, nil
}

// mergeOps keeps the last operation per key, in first-seen order. Stores
// reject a commit that names a key twice.
func mergeOps(ops []storage.Operation) []storage.Operation {
	index := map[string]int{}
	out := make([]storage.Operation, 0, len(ops))
	for _, op := range ops {
		if i, ok := index[op.Key]; ok {
			out[i] = op
			continue
		}
		index[op.Key] = len(out)
		out = append(out, op)
	}
	return out
}

// chainRun projects the active chain onto the run shape for run-oriented
// clients. The projection is read-only; nothing drives it.
func chainRun(snap storage.Snapshot, chain Chain) Run {
	run := Run{ID: chain.RunID, ConversationID: chain.ConversationID, Submissions: chain.Submissions, Phase: "request", Turn: 1, Attempt: 1, Notices: chain.Notices, Revision: chain.Revision}
	t, ok, _ := read[Task](snap, taskKey(chain.Task))
	if !ok {
		return run
	}
	run.AbortRequested = t.AbortRequested
	if _, cp, err := decodeGeneration(t); err == nil {
		run.Turn, run.Attempt, run.Cutoff, run.Compacted = cp.Turn, cp.Attempt, cp.Cutoff, cp.Compacted
		if t.Phase == "collect" {
			run.Phase = "tools"
			for _, ref := range cp.Calls {
				intent := ToolIntent{CallID: ref.CallID, State: "pending"}
				if tool, ok, _ := read[Task](snap, taskKey(ref.Task)); ok {
					in, tcp, _ := decodeTool(tool)
					intent.Name, intent.Args, intent.Replay, intent.Approval = in.Name, tcp.Args, tcp.Replay, tcp.Approval
					switch {
					case tool.State == "terminal":
						intent.State = "done"
						var out toolOutcome
						if json.Unmarshal(tool.Result, &out) == nil {
							intent.Entry = out.Entry
						}
					case tool.Effect == effectStarted || tool.Phase == "subagent":
						intent.State = "running"
					}
				}
				run.Tools = append(run.Tools, intent)
			}
		}
	}
	return run
}

// planChain is the recovery preview of an interrupted chain. Like planRun it
// reads committed state only.
func planChain(snap storage.Snapshot, chain Chain) RecoveryAction {
	run := chainRun(snap, chain)
	pending, _ := pendingApprovals(snap, chain.ConversationID)
	awaiting := false
	for _, a := range pending {
		if a.RunID == chain.RunID {
			awaiting = true
		}
	}
	a := planRun(run, awaiting)
	a.Executor = executorTasks
	gen, ok, _ := read[Task](snap, taskKey(chain.Task))
	if ok && !run.AbortRequested && !awaiting && run.Phase == "request" {
		if gen.Effect == effectStarted {
			a.Action, a.Detail = "resend", fmt.Sprintf("the model request of turn %d attempt %d may have been sent; it is sent again and the earlier attempt's usage is recorded as unknown", run.Turn, run.Attempt)
		} else {
			a.Action, a.Detail = "continue", "the next model request has not started"
		}
	}
	return a
}

func chainAttempt(t Task) int {
	_, cp, err := decodeGeneration(t)
	if err != nil {
		return 1
	}
	return cp.Attempt
}

func chainTurn(t Task) int {
	_, cp, err := decodeGeneration(t)
	if err != nil {
		return 1
	}
	return cp.Turn
}

// settledRun projects a terminal generation task onto the run shape.
func settledRun(t Task) (Run, bool) {
	var result struct {
		RunID       string   `json:"run_id"`
		Turn        int      `json:"turn"`
		Attempt     int      `json:"attempt"`
		Notices     []string `json:"notices"`
		Submissions []string `json:"submissions"`
	}
	if t.Kind != TaskKindGeneration || t.State != "terminal" || json.Unmarshal(t.Result, &result) != nil || result.RunID == "" {
		return Run{}, false
	}
	return Run{ID: result.RunID, ConversationID: t.ConversationID, Submissions: result.Submissions, Phase: "done", Outcome: t.Outcome, Error: t.Error, Turn: max(result.Turn, 1), Attempt: max(result.Attempt, 1), Notices: result.Notices, Revision: t.Revision}, true
}

// stepTasks implements Service.Step. It drives the conversation's tasks, and
// the tasks of conversations it owns, on a scheduler restricted to them
// until no chain is active and nothing is queued. It returns the run-shaped
// projection of the last chain it saw.
func (s *Service) stepTasks(ctx context.Context, conversationID string) (Run, bool, error) {
	x, err := NewNativeExecutor(s.r, s.currentEngine, s.opts)
	if err != nil {
		return Run{}, false, err
	}
	x.generation = s.Generation
	reg := TaskRegistry{}
	if err := x.Register(reg); err != nil {
		return Run{}, false, err
	}
	sched := x.Scheduler(reg)
	sched.Filter = func(snap storage.Snapshot, t Task) bool {
		return ownedBy(snap, t.ConversationID, conversationID)
	}
	// Step returns only after every invocation it started has returned, so
	// no handler writes after Step, whatever the reason it ends.
	defer sched.Join(context.Background())
	var last Run
	did := false
	if _, blocked, err := s.r.MigrateLegacy(ctx); err != nil {
		return last, did, err
	} else if cause := blocked[conversationID]; cause != nil {
		snap, err := s.r.store.Snapshot(ctx)
		if err != nil {
			return last, did, err
		}
		run, _, _ := read[Run](snap, runKey(conversationID))
		return run, false, cause
	}
	for {
		snap, err := s.r.store.Snapshot(ctx)
		if err != nil {
			return last, did, err
		}
		chain, active, err := read[Chain](snap, chainKey(conversationID))
		if err != nil {
			return last, did, err
		}
		queued, err := snap.Page("queue/"+conversationID+"/", "", 1)
		if err != nil {
			return last, did, err
		}
		if active {
			last, did = chainRun(snap, chain), true
		}
		if !active && len(queued) == 0 && !liveTasks(snap, conversationID) {
			if did {
				// Report the settled chain from its generation task.
				if t, ok, _ := read[Task](snap, taskKey(lastGeneration(snap, conversationID))); ok {
					if run, ok := settledRun(t); ok && run.ID == last.ID {
						last = run
					}
				}
			}
			return last, did, nil
		}
		n, wake, err := sched.Tick(ctx)
		if err != nil {
			return last, true, err
		}
		if n > 0 {
			continue
		}
		if sched.InFlight() == 0 {
			// Decide on state committed after this tick's invocations.
			if fresh, err := s.r.store.Snapshot(ctx); err == nil && fresh.Revision() != snap.Revision() {
				continue
			}
			// A call whose outcome is unknown waits for reconciliation with
			// backoff. Step returns so the caller can restore the receiver;
			// the next Step or the host reconciles it.
			if unresolvedCall(snap, conversationID) {
				return last, true, fmt.Errorf("%w: reconciliation pending", core.ErrToolOutcomeUnknown)
			}
			// A pending human decision parks the call. Step returns instead
			// of waiting for a human; the decision's commit resumes it.
			if pending, _ := pendingApprovals(snap, conversationID); len(pending) > 0 {
				return last, true, ErrAwaitingApproval
			}
			if wake.IsZero() && active {
				// Blocked cleanup or a dependency outside this tree.
				return last, true, fmt.Errorf("%w: chain %s cannot progress", ErrBusy, chain.RunID)
			}
		}
		if err := sched.waitProgress(ctx, wake); err != nil {
			return last, true, err
		}
	}
}

// lastGeneration returns the newest generation task of a conversation.
func lastGeneration(snap storage.Snapshot, conversationID string) string {
	var newest Task
	_ = pageAll(snap, "task-conversation/"+conversationID+"/", func(row storage.Record) error {
		var id string
		if json.Unmarshal(row.Value, &id) != nil {
			return nil
		}
		t, ok, _ := read[Task](snap, taskKey(id))
		if ok && t.Kind == TaskKindGeneration && (newest.ID == "" || t.Created.After(newest.Created) || t.Created.Equal(newest.Created) && t.Revision > newest.Revision) {
			newest = t
		}
		return nil
	})
	return newest.ID
}

// ownedBy reports whether conversation id is root or owned, transitively,
// by a conversation in root's tree.
func ownedBy(snap storage.Snapshot, id, root string) bool {
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		if id == root {
			return true
		}
		seen[id] = true
		c, err := conversation(snap, id)
		if err != nil || c.Owner == nil {
			return false
		}
		id = c.Owner.ConversationID
	}
	return false
}

// pageAll visits every record under prefix in key order, page by page.
func pageAll(snap storage.Snapshot, prefix string, visit func(storage.Record) error) error {
	after := ""
	for {
		page, err := snap.Page(prefix, after, 500)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			after = row.Key
			if err := visit(row); err != nil {
				return err
			}
		}
	}
}

// liveTasks reports whether a conversation has an unpublished background
// summary still running, which Step waits for so its result is durable.
func liveTasks(snap storage.Snapshot, conversationID string) bool {
	id, ok, _ := read[string](snap, backgroundKey(conversationID))
	if !ok {
		return false
	}
	t, ok, _ := read[Task](snap, taskKey(id))
	return ok && t.State != "terminal"
}

// unresolvedCall reports whether a tool call of the conversation's active
// chain is parked on an unknown outcome.
func unresolvedCall(snap storage.Snapshot, conversationID string) bool {
	chain, ok, _ := read[Chain](snap, chainKey(conversationID))
	if !ok {
		return false
	}
	gen, ok, _ := read[Task](snap, taskKey(chain.Task))
	if !ok {
		return false
	}
	_, cp, err := decodeGeneration(gen)
	if err != nil {
		return false
	}
	for _, ref := range cp.Calls {
		t, ok, _ := read[Task](snap, taskKey(ref.Task))
		if !ok || t.State != "waiting" || t.Effect != effectStarted {
			continue
		}
		if _, tcp, err := decodeTool(t); err == nil && tcp.Unresolved > 0 {
			return true
		}
	}
	return false
}
