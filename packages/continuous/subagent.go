package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Owner records who created a conversation: a tool call of a run (the usual
// subagent case) or a durable task. Aborting the owner aborts the owned
// conversation's run; the owner is not idle until the owned conversation is.
// Ownership is provenance and abort scope, not access control.
type Owner struct {
	ConversationID string `json:"conversation_id"`
	// ID is a task ID or a tool call owner ID (see ToolCallIdentity.OwnerID).
	ID string `json:"id"`
}

func ownedConversationKey(ownerID, conversationID string) string {
	return "owned-conversation/" + ownerID + "/" + conversationID
}

// CreateOwnedConversation creates a conversation owned by ownerID within the
// owning conversation parentID. The child starts with the parent's
// configuration unless config is set. Creation is idempotent per (owner,
// key): a second call with the same key returns the existing child, so a
// replayed tool call or task phase does not spawn twice.
func (r *Runtime) CreateOwnedConversation(ctx context.Context, parentID, ownerID, key string, config *AgentConfig) (Conversation, error) {
	if key == "" || ownerID == "" {
		return Conversation{}, fmt.Errorf("owned conversation requires an owner and a stable key")
	}
	dedup := hashedKey("owned-key/", ownerID, key)
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		if existing, ok, err := read[string](snap, dedup); err != nil {
			return Conversation{}, err
		} else if ok {
			return conversation(snap, existing)
		}
		parent, err := conversation(snap, parentID)
		if err != nil {
			return Conversation{}, err
		}
		cfg := parent.Config
		if config != nil {
			cfg = *config
		}
		c := Conversation{ID: uuid.NewString(), Created: time.Now().UTC(), Revision: snap.Revision() + 1, Config: cfg, Owner: &Owner{ConversationID: parentID, ID: ownerID}}
		err = r.commit(ctx, snap, "conversation.create.owned", record("conversation/"+c.ID, c), record(ownedConversationKey(ownerID, c.ID), c.ID), record(dedup, c.ID))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return c, err
	}
}

// OwnedConversations lists the conversations an owner created.
func (r *Runtime) OwnedConversations(ctx context.Context, ownerID string) ([]string, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return ownedConversations(snap, ownerID)
}

func ownedConversations(snap storage.Snapshot, ownerID string) ([]string, error) {
	rows, err := snap.Page("owned-conversation/"+ownerID+"/", "", 1000)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var id string
		if err := json.Unmarshal(row.Value, &id); err != nil {
			return nil, storage.ErrCorrupt
		}
		out = append(out, id)
	}
	return out, nil
}

// SubagentTool delegates a self-contained task to an owned conversation and
// returns its answer. It is replay safe: a rerun after a crash finds the same
// child through the owning tool call and stable key, and the same submission
// through its request ID, then continues whatever the child already did. It
// steps the child with the same Service, so the child's model and tool
// configuration come from the host engine. Nesting depth is bounded by
// MaxDepth so a subagent cannot recurse without limit.
type SubagentTool struct {
	Runtime *Runtime
	Service *Service
	// Config overrides the child's agent configuration. Nil copies the parent.
	Config *AgentConfig
	// MaxDepth bounds nesting. Zero means 2.
	MaxDepth int
}

func (t *SubagentTool) Name() string { return "subagent" }
func (t *SubagentTool) Description() string {
	return "Delegate a self-contained task to a subagent with its own conversation and return its final answer."
}
func (t *SubagentTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"task":{"type":"string","description":"What the subagent should do, self-contained"},"key":{"type":"string","description":"Optional stable key so a retry reuses the same subagent"}},"required":["task"]}`)
}
func (t *SubagentTool) ReplayPolicy() core.ToolReplayPolicy { return core.ReplaySafe }

func (t *SubagentTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var args struct {
		Task string `json:"task"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.Task == "" {
		return core.ToolResult{}, fmt.Errorf("subagent requires a task")
	}
	call, ok := ToolCallFromContext(ctx)
	if !ok || t.Runtime == nil || t.Service == nil {
		return core.ToolResult{}, fmt.Errorf("subagent runs only inside a continuous run")
	}
	snap, err := t.Runtime.Snapshot(ctx)
	if err != nil {
		return core.ToolResult{}, err
	}
	depth, err := ownershipDepth(snap, call.ConversationID)
	if err != nil {
		return core.ToolResult{}, err
	}
	maxDepth := t.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	if depth >= maxDepth {
		return core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("subagent nesting limit %d reached", maxDepth)}}}, nil
	}
	key := args.Key
	if key == "" {
		key = hashedKey("", args.Task)
	}
	child, err := t.Runtime.CreateOwnedConversation(ctx, call.ConversationID, call.OwnerID(), key, t.Config)
	if err != nil {
		return core.ToolResult{}, err
	}
	progress("subagent " + child.ID + "\n")
	sub, err := t.Runtime.Submit(ctx, child.ID, "subagent:"+call.OwnerID(), "subagent:"+key, args.Task)
	if err != nil {
		return core.ToolResult{}, err
	}
	if sub.State == "queued" || sub.State == "running" {
		if _, _, err := t.Service.Step(ctx, child.ID); err != nil {
			return core.ToolResult{}, err
		}
	}
	settled, err := t.Runtime.WaitSubmission(ctx, sub.ID)
	if err != nil {
		return core.ToolResult{}, err
	}
	details := map[string]string{"conversation": child.ID}
	if settled.State != "answered" {
		return core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("subagent %s: %s", child.ID, settled.State)}}, Details: details}, nil
	}
	snap, err = t.Runtime.Snapshot(ctx)
	if err != nil {
		return core.ToolResult{}, err
	}
	c, err := conversation(snap, child.ID)
	if err != nil {
		return core.ToolResult{}, err
	}
	answer, err := lastEntryOfType(snap, c.ID, c.EntrySequence, entryAssistant)
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: core.MessageText(answer)}}, Details: details}, nil
}

// ownershipDepth counts owners above a conversation.
func ownershipDepth(snap storage.Snapshot, conversationID string) (int, error) {
	depth := 0
	seen := map[string]bool{}
	for id := conversationID; id != ""; {
		if seen[id] {
			return 0, fmt.Errorf("%w: ownership cycle", storage.ErrCorrupt)
		}
		seen[id] = true
		c, err := conversation(snap, id)
		if err != nil {
			return 0, err
		}
		if c.Owner == nil {
			return depth, nil
		}
		depth++
		id = c.Owner.ConversationID
	}
	return depth, nil
}
