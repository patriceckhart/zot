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

// Run is the run shape clients read. For a conversation's active generation
// chain it is a read-only projection (see chainRun). Stores written by the
// earlier run-based executor also hold Run records under run/<conversation>;
// an unfinished one is migrated into a chain at open (MigrateLegacy) and
// never driven directly.
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
	// AbortRequested is the committed abort intent.
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

// Service answers admitted work of a runtime on the task scheduler. Step
// drives one conversation in the caller's goroutine; a Host drives every
// conversation. Both use the same task definitions, so there is one
// executor.
type Service struct {
	r    *Runtime
	opts ExecutionOptions
	// engine is the current registry generation. Reload replaces it
	// atomically; a request or tool call that already built its agent keeps
	// the generation it started with.
	engine     atomic.Pointer[engineGeneration]
	generation atomic.Uint64
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
	s := &Service{r: r, opts: opts.normalized()}
	s.engine.Store(&engineGeneration{Engine: engine, Generation: 1})
	s.generation.Store(1)
	return s, nil
}

// Run returns the run shape of a conversation: the projection of its active
// chain, else the last settled chain, else a stored run record of the
// earlier executor.
func (r *Runtime) Run(ctx context.Context, conversationID string) (Run, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Run{}, false, err
	}
	return runView(snap, conversationID)
}

func runView(snap storage.Snapshot, conversationID string) (Run, bool, error) {
	if chain, ok, err := read[Chain](snap, chainKey(conversationID)); err != nil {
		return Run{}, false, err
	} else if ok {
		return chainRun(snap, chain), true, nil
	}
	if legacy, ok, err := read[Run](snap, runKey(conversationID)); err != nil {
		return Run{}, false, err
	} else if ok && legacy.Phase != "done" {
		return legacy, true, nil
	}
	if t, ok, _ := read[Task](snap, taskKey(lastGeneration(snap, conversationID))); ok {
		if run, ok := settledRun(t); ok {
			return run, true, nil
		}
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

func (s *Service) currentEngine() Engine { return s.engine.Load().Engine }

// CompactionMetrics reports compaction activity from committed state.
func (s *Service) CompactionMetrics(ctx context.Context) (CompactionMetrics, error) {
	snap, err := s.r.store.Snapshot(ctx)
	if err != nil {
		return CompactionMetrics{}, err
	}
	return compactionMetrics(snap), nil
}

// Step answers the queued work of one conversation, and of the
// conversations it owns, on a scheduler restricted to them, until no chain
// is active and nothing is queued. It first migrates interrupted runs of the
// earlier executor. It returns the run shape of the last chain and whether
// work was done. A conversation blocked by an unmigratable run returns
// ErrMigrationBlocked; Runtime.Abort settles it.
func (s *Service) Step(ctx context.Context, conversationID string) (Run, bool, error) {
	return s.stepTasks(ctx, conversationID)
}

// prepareAgent builds a fresh agent through the host's Engine, replaces its
// transcript with the committed model context through cutoff, and applies the
// conversation's configuration.
func prepareAgent(ctx context.Context, engine Engine, snap storage.Snapshot, c Conversation, cutoff uint64) (*core.Agent, error) {
	agent, err := engine.Build(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("build engine: %w", err)
	}
	if agent == nil {
		return nil, fmt.Errorf("engine returned no agent")
	}
	messages, err := ModelContext(ctx, snap, c.ID, cutoff)
	if err != nil {
		return nil, err
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
	return agent, nil
}

// claimSteering removes queued steer submissions from the queue and adds them
// to the run. Each steered input gets a fresh user entry at the current tail
// so ModelContext places it after the tool results; the original admission
// entry is retained for history and marked superseded so it is not sent twice.
func claimSteeringOps(snap storage.Snapshot, c *Conversation, run *Run) ([]storage.Operation, error) {
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

// recoverCall applies the replay contract to an interrupted call. It
// returns a human-readable decision, a result when one was obtained, and
// whether the tool executed again. An empty result with replayed false means
// the call is reported as interrupted.
func recoverCall(ctx context.Context, r *Runtime, agent *core.Agent, identity ToolCallIdentity, intent ToolIntent, call provider.ToolCallBlock, sink func(core.AgentEvent)) (string, core.ToolResult, bool) {
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
	// Give hooks their own copy so an in-place rewrite cannot alter the
	// committed arguments used for comparison or reconciliation.
	call.Arguments = append(json.RawMessage(nil), intent.Args...)
	args, allowed, reason, _ := authorizeCall(ctx, r, agent, identity.ConversationID, call)
	if !allowed {
		return fmt.Sprintf("tool %s interrupted, replay denied by current authorization: %s", call.ID, reason), none, false
	}
	if !sameJSON(args, intent.Args) {
		return fmt.Sprintf("tool %s interrupted, not replayed: committed arguments changed by current authorization", call.ID), none, false
	}
	switch intent.Replay {
	case core.ReplaySafe:
		return fmt.Sprintf("tool %s replayed after interruption (policy safe)", call.ID), executeCall(ctx, agent, identity, call, sink), true
	case core.ReplayIdempotent:
		return fmt.Sprintf("tool %s replayed after interruption with the same operation key (policy idempotent); the receiver must enforce the key", call.ID), executeCall(ctx, agent, identity, call, sink), true
	case core.ReplayReconcile:
		rec, ok := tool.(core.ToolReconciler)
		if !ok {
			return fmt.Sprintf("tool %s interrupted, not replayed: reconcile policy without reconciler", call.ID), none, false
		}
		key := identity.OperationKey()
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
			return fmt.Sprintf("tool %s reconciled as not started, executed (policy reconcile)", call.ID), executeCall(ctx, agent, identity, call, sink), true
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

// usageRow builds the ledger row of one attempt and advances the counter on
// the conversation copy that the same commit writes. Missing provider usage is
// recorded as unknown so totals never understate spend silently.
func usageOp(c *Conversation, runID string, turn, attempt int, agent *core.Agent, usage provider.Usage, known bool) storage.Operation {
	c.UsageSequence++
	status := "unknown"
	if known {
		status = "known"
	}
	providerName := c.Config.Provider
	if agent.Client != nil {
		providerName = agent.Client.Name()
	}
	return record(usageKey(c.ID, c.UsageSequence), UsageRecord{ConversationID: c.ID, RunID: runID, Turn: turn, Attempt: attempt, Provider: providerName, Model: agent.Model, Usage: usage, Status: status, Source: "model", Time: time.Now().UTC()})
}

// authorizeCall applies the conversation's tool configuration and the host's
// guard to one call and returns the effective arguments and replay policy.
// It fails closed: an unreadable conversation refuses the call.
func authorizeCall(ctx context.Context, r *Runtime, agent *core.Agent, conversationID string, call provider.ToolCallBlock) (json.RawMessage, bool, string, core.ToolReplayPolicy) {
	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if c, cerr := r.Conversation(ctx, conversationID); cerr != nil {
		return args, false, "tool call refused: conversation configuration unavailable", core.ReplayNever
	} else if !c.Config.allows(call.Name) {
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
func executeCall(ctx context.Context, agent *core.Agent, identity ToolCallIdentity, call provider.ToolCallBlock, sink func(core.AgentEvent)) core.ToolResult {
	before, beforeCtx := agent.BeforeToolExecute, agent.BeforeToolExecuteContext
	agent.BeforeToolExecute, agent.BeforeToolExecuteContext = nil, nil
	defer func() { agent.BeforeToolExecute, agent.BeforeToolExecuteContext = before, beforeCtx }()
	ctx = context.WithValue(ctx, toolCallKey{}, identity)
	ctx = core.WithToolOperationKey(ctx, identity.OperationKey())
	return agent.CallTool(ctx, call.ID, call.Name, call.Arguments, func(ev core.AgentEvent) {
		switch ev.(type) {
		case core.EvToolResult, core.EvToolCall:
			// The turn already announced the call, and the service reports the
			// result after its commit so observers never see an uncommitted one.
			return
		}
		sink(ev)
	})
}

func settleOps(snap storage.Snapshot, submissions []string, state string) []storage.Operation {
	var ops []storage.Operation
	for _, id := range submissions {
		sub, ok, err := read[Submission](snap, "submission/"+id)
		if err != nil || !ok {
			continue
		}
		sub.State = state
		ops = append(ops, record("submission/"+id, sub))
	}
	return ops
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
