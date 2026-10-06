package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// A conversation view is the live state of one conversation as typed
// documents, each changed only by commits:
//
//	execution  the active chain and its live tasks (chain/, task/)
//	queue      unclaimed submissions in order (queue/, submission/)
//	usage      the usage ledger (usage/)
//	agent      model, reasoning, extensions, placement modes, tool lists
//	           (the conversation's AgentConfig)
//	provider   the provider identity: provider name and provider session
//	entries    transcript entries (entry/)
//	partial    streamed output of the in-flight attempt (partial/)
//	progress   committed progress of running tool calls (progress/)
//	approvals  pending approvals (approval/)
//
// The documents are projections of the records the runtime already writes
// in the same commits, so they cannot disagree with execution. WatchView
// turns each commit into the operations it applied to these documents.

// View documents.
const (
	ViewExecution = "execution"
	ViewQueue     = "queue"
	ViewUsage     = "usage"
	ViewAgent     = "agent"
	ViewProvider  = "provider"
	ViewEntries   = "entries"
	ViewPartial   = "partial"
	ViewProgress  = "progress"
	ViewApprovals = "approvals"
)

// ProviderIdentity is the provider-facing identity of a conversation.
type ProviderIdentity struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Session is sent to providers for prompt caching and sticky routing.
	Session string `json:"session"`
}

// ViewOp is one change to a view document. Op is put or delete. Key
// identifies the element within a collection document (an entry sequence,
// submission ID, task ID, or approval ID); it is empty for single-value
// documents. Value is the new element or document.
type ViewOp struct {
	Doc   string          `json:"doc"`
	Op    string          `json:"op"`
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ViewChange is the view-level effect of one commit.
type ViewChange struct {
	Revision uint64   `json:"revision"`
	Actor    string   `json:"actor,omitempty"`
	Ops      []ViewOp `json:"ops"`
}

// viewOps maps a commit's operations onto the view documents of one
// conversation. Records of other conversations yield nothing.
func viewOps(cm storage.Commit, conversationID string) []ViewOp {
	var ops []ViewOp
	put := func(doc, key string, value json.RawMessage, del bool) {
		op := ViewOp{Doc: doc, Key: key, Value: value, Op: "put"}
		if del {
			op.Op, op.Value = "delete", nil
		}
		ops = append(ops, op)
	}
	for _, op := range cm.Operations {
		k := op.Key
		switch {
		case k == "conversation/"+conversationID:
			var c Conversation
			if op.Delete || json.Unmarshal(op.Value, &c) != nil {
				continue
			}
			agent, _ := json.Marshal(c.Config)
			put(ViewAgent, "", agent, false)
			ident, _ := json.Marshal(ProviderIdentity{Provider: c.Config.Provider, Model: c.Config.Model, Session: c.ProviderSessionID()})
			put(ViewProvider, "", ident, false)
		case k == chainKey(conversationID):
			put(ViewExecution, "chain", op.Value, op.Delete)
		case strings.HasPrefix(k, "entry/"+conversationID+"/"):
			put(ViewEntries, strings.TrimLeft(strings.TrimPrefix(k, "entry/"+conversationID+"/"), "0"), op.Value, op.Delete)
		case strings.HasPrefix(k, "queue/"+conversationID+"/"):
			var id string
			if !op.Delete {
				_ = json.Unmarshal(op.Value, &id)
			}
			put(ViewQueue, strings.TrimPrefix(k, "queue/"+conversationID+"/"), json.RawMessage(quoteJSON(id)), op.Delete)
		case strings.HasPrefix(k, "usage/"+conversationID+"/"):
			put(ViewUsage, strings.TrimLeft(strings.TrimPrefix(k, "usage/"+conversationID+"/"), "0"), op.Value, op.Delete)
		case k == partialKey(conversationID):
			put(ViewPartial, "", op.Value, op.Delete)
		case strings.HasPrefix(k, "task/"):
			var t Task
			if op.Delete || json.Unmarshal(op.Value, &t) != nil || t.ConversationID != conversationID {
				continue
			}
			put(ViewExecution, "task/"+t.ID, op.Value, false)
		case strings.HasPrefix(k, "progress/"):
			var p ToolProgress
			if !op.Delete && json.Unmarshal(op.Value, &p) == nil {
				// Progress carries its task, which names the conversation
				// only through the task record; deliveries filter by the
				// commit's task records below.
				put(ViewProgress, p.TaskID, op.Value, false)
			} else if op.Delete {
				put(ViewProgress, strings.TrimPrefix(k, "progress/"), nil, true)
			}
		case strings.HasPrefix(k, "approval/"):
			var a Approval
			if op.Delete || json.Unmarshal(op.Value, &a) != nil || a.ConversationID != conversationID {
				continue
			}
			put(ViewApprovals, a.ID, op.Value, false)
		case strings.HasPrefix(k, "submission/"):
			var s Submission
			if op.Delete || json.Unmarshal(op.Value, &s) != nil || s.ConversationID != conversationID {
				continue
			}
			put(ViewQueue, "submission/"+s.ID, op.Value, false)
		}
	}
	return ops
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// WatchView delivers the view-level changes of a conversation after a
// revision, one per commit that changed it, in order, until ctx ends. A
// cursor outside retained history yields storage.ErrCursor; the client reads
// ConversationSnapshot and watches from its revision.
func (r *Runtime) WatchView(ctx context.Context, conversationID string, after uint64, visit func(ViewChange) error) error {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	if _, err := conversation(snap, conversationID); err != nil {
		return err
	}
	// Progress records name their task, not their conversation; tasks of
	// this conversation are learned from the snapshot and from commits.
	tasks := map[string]bool{}
	_ = pageAll(snap, "task-conversation/"+conversationID+"/", func(row storage.Record) error {
		var id string
		if json.Unmarshal(row.Value, &id) == nil {
			tasks[id] = true
		}
		return nil
	})
	return r.Watch(ctx, conversationID, after, func(cm storage.Commit) error {
		ops := viewOps(cm, conversationID)
		kept := ops[:0]
		for _, op := range ops {
			if op.Doc == ViewExecution && strings.HasPrefix(op.Key, "task/") {
				tasks[strings.TrimPrefix(op.Key, "task/")] = true
			}
		}
		for _, op := range ops {
			if op.Doc == ViewProgress && !tasks[op.Key] {
				continue
			}
			kept = append(kept, op)
		}
		if len(kept) == 0 {
			return nil
		}
		return visit(ViewChange{Revision: cm.Revision, Actor: cm.Actor, Ops: kept})
	})
}

// AgentEvent is a committed agent event derived from transcript and task
// commits. Unlike engine events, every agent event describes durable state:
// a client that reconnects reads the same events again from the same
// revision.
type AgentEvent struct {
	Revision uint64 `json:"revision"`
	// Type is message_start, message_update, message_end,
	// tool_execution_start, tool_execution_update, tool_execution_end,
	// compaction_start, or compaction_end.
	Type string `json:"type"`
	// Entry is the transcript entry of message_end and compaction_end.
	Entry *Entry `json:"entry,omitempty"`
	// Partial is the streamed text of message_start and message_update.
	Partial *Partial `json:"partial,omitempty"`
	// Tool events name the call and carry its progress or result status.
	CallID   string          `json:"call_id,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	Progress string          `json:"progress,omitempty"`
	Status   string          `json:"status,omitempty"`
	// Task is the task of tool and compaction events.
	Task string `json:"task,omitempty"`
}

// AgentEventStream is a bounded subscription to the committed agent events
// of one conversation. Snapshot is the state the events apply to. When the
// subscriber falls more than its buffer behind, the stream ends with
// ErrEventLag; the client resubscribes and gets a fresh snapshot.
type AgentEventStream struct {
	Snapshot ConversationSnapshot
	Events   <-chan AgentEvent
	// Err reports why Events closed: nil after cancellation, ErrEventLag,
	// storage.ErrCursor, or a store error.
	Err func() error
}

// ErrEventLag reports a subscriber that fell behind its bound.
var ErrEventLag = errors.New("continuous agent event subscriber fell behind")

// SubscribeAgentEvents returns a fresh snapshot and the committed agent
// events after it. buffer bounds the lag (default 256).
func (r *Runtime) SubscribeAgentEvents(ctx context.Context, conversationID string, buffer int) (*AgentEventStream, error) {
	if buffer <= 0 {
		buffer = 256
	}
	snap, err := r.ConversationSnapshot(ctx, conversationID, 1)
	if err != nil {
		return nil, err
	}
	ch := make(chan AgentEvent, buffer)
	var streamErr error
	done := make(chan struct{})
	d := newEventDeriver(conversationID, snap)
	go func() {
		defer close(done)
		defer close(ch)
		err := r.Watch(ctx, conversationID, snap.Revision, func(cm storage.Commit) error {
			for _, ev := range d.derive(cm) {
				select {
				case ch <- ev:
				default:
					return ErrEventLag
				}
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			streamErr = err
		}
	}()
	return &AgentEventStream{Snapshot: snap, Events: ch, Err: func() error { <-done; return streamErr }}, nil
}

// eventDeriver turns commits into agent events. It keeps the minimal state
// needed to pair starts with ends: the live partial and the running calls.
type eventDeriver struct {
	id        string
	streaming bool
	running   map[string]bool
	compact   map[string]bool
}

func newEventDeriver(conversationID string, snap ConversationSnapshot) *eventDeriver {
	d := &eventDeriver{id: conversationID, running: map[string]bool{}, compact: map[string]bool{}}
	d.streaming = snap.Partial != nil && !snap.Partial.Final
	for _, t := range snap.Tasks {
		switch {
		case t.Kind == TaskKindTool && (t.Effect == effectStarted || t.Phase == "subagent"):
			d.running[t.ID] = true
		case t.Kind == TaskKindCompaction && t.State != "terminal":
			d.compact[t.ID] = true
		}
	}
	return d
}

func (d *eventDeriver) derive(cm storage.Commit) []AgentEvent {
	var out []AgentEvent
	ev := func(e AgentEvent) {
		e.Revision = cm.Revision
		out = append(out, e)
	}
	for _, op := range cm.Operations {
		switch {
		case op.Key == partialKey(d.id):
			var p Partial
			if op.Delete || json.Unmarshal(op.Value, &p) != nil || p.Final {
				continue
			}
			if !d.streaming {
				d.streaming = true
				ev(AgentEvent{Type: "message_start", Partial: &p})
			} else {
				ev(AgentEvent{Type: "message_update", Partial: &p})
			}
		case strings.HasPrefix(op.Key, "task/"):
			var t Task
			if op.Delete || json.Unmarshal(op.Value, &t) != nil || t.ConversationID != d.id {
				continue
			}
			switch t.Kind {
			case TaskKindTool:
				var in toolInput
				_ = json.Unmarshal(t.Input, &in)
				started := t.Effect == effectStarted || t.Phase == "subagent"
				if started && !d.running[t.ID] {
					d.running[t.ID] = true
					var cp toolCheckpoint
					_ = json.Unmarshal(t.Checkpoint, &cp)
					ev(AgentEvent{Type: "tool_execution_start", Task: t.ID, CallID: in.CallID, Tool: in.Name, Args: cp.Args})
				}
				if t.State == "terminal" {
					var res toolOutcome
					_ = json.Unmarshal(t.Result, &res)
					delete(d.running, t.ID)
					ev(AgentEvent{Type: "tool_execution_end", Task: t.ID, CallID: in.CallID, Tool: in.Name, Status: firstNonEmpty(res.Status, t.Outcome)})
				}
			case TaskKindCompaction:
				if t.State != "terminal" && !d.compact[t.ID] {
					d.compact[t.ID] = true
					ev(AgentEvent{Type: "compaction_start", Task: t.ID})
				}
			}
		case strings.HasPrefix(op.Key, "progress/"):
			var p ToolProgress
			if op.Delete || json.Unmarshal(op.Value, &p) != nil || !d.running[p.TaskID] {
				continue
			}
			ev(AgentEvent{Type: "tool_execution_update", Task: p.TaskID, CallID: p.CallID, Progress: p.Text})
		case strings.HasPrefix(op.Key, "entry/"+d.id+"/"):
			var e Entry
			if op.Delete || json.Unmarshal(op.Value, &e) != nil {
				continue
			}
			switch e.Type {
			case entryAssistant, "user", entrySteer, entryContinue, entryToolResult:
				if e.Type == entryAssistant && !d.streaming {
					ev(AgentEvent{Type: "message_start"})
				}
				if e.Type == entryAssistant {
					d.streaming = false
				}
				entry := e
				ev(AgentEvent{Type: "message_end", Entry: &entry})
			case entryAttempt:
				d.streaming = false
			case entryCompaction:
				entry := e
				for id := range d.compact {
					delete(d.compact, id)
				}
				ev(AgentEvent{Type: "compaction_end", Entry: &entry})
			}
		}
	}
	return out
}
