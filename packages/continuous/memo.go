package continuous

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Memo is a first-write-wins decision record scoped to a run or a task. Hooks
// and phases use it to remember a choice (a generated ID, a routing decision,
// a drawn random value) so a re-executed attempt reuses the same choice
// instead of making a new one. Reading a memo and then acting is not atomic
// with the action: the memo makes the decision durable, not the effect.
type Memo struct {
	Scope    string          `json:"scope"`
	Key      string          `json:"key"`
	Value    json.RawMessage `json:"value"`
	Actor    string          `json:"actor,omitempty"`
	Created  time.Time       `json:"created"`
	Revision uint64          `json:"revision"`
}

func memoKey(scope, key string) string { return "memo/" + scope + "/" + hashedKey("", key) }

// Memoize returns the stored value for (scope, key), writing value when none
// exists. Concurrent writers for the same key observe the first commit; the
// loser receives the winner's value and created false.
func (r *Runtime) Memoize(ctx context.Context, scope, key, actor string, value any) (Memo, bool, error) {
	if scope == "" || key == "" {
		return Memo{}, false, fmt.Errorf("memo requires a scope and a key")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return Memo{}, false, err
	}
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Memo{}, false, err
		}
		existing, ok, err := read[Memo](snap, memoKey(scope, key))
		if err != nil {
			return Memo{}, false, err
		}
		if ok {
			return existing, false, nil
		}
		m := Memo{Scope: scope, Key: key, Value: raw, Actor: actor, Created: time.Now().UTC(), Revision: snap.Revision() + 1}
		err = r.commit(ctx, snap, "memo.write", record(memoKey(scope, key), m))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return m, err == nil, err
	}
}

// Memo reads a decision without writing.
func (r *Runtime) Memo(ctx context.Context, scope, key string) (Memo, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Memo{}, false, err
	}
	return read[Memo](snap, memoKey(scope, key))
}

// MemoScope returns the memo scope for the durable tool call in ctx, or for
// the run when called from a hook with a run ID. Tasks use "task/<id>".
func MemoScope(ctx context.Context) (string, bool) {
	if id, ok := ToolCallFromContext(ctx); ok {
		return "run/" + id.RunID, true
	}
	return "", false
}

// PromptRecord explains which system prompt and tools a response was
// generated against, as content hashes. Records of current stores are
// derived from context entries (ContextChange); stores written before them
// keep their stored prompt/ records, which are listed first.
type PromptRecord struct {
	ConversationID string    `json:"conversation_id"`
	RunID          string    `json:"run_id,omitempty"`
	Turn           int       `json:"turn,omitempty"`
	Attempt        int       `json:"attempt,omitempty"`
	SystemHash     string    `json:"system_hash"`
	ToolsHash      string    `json:"tools_hash"`
	ToolNames      []string  `json:"tool_names"`
	Model          string    `json:"model"`
	Time           time.Time `json:"time"`
	// Entry is the sequence of the context entry, for derived records.
	Entry uint64 `json:"entry,omitempty"`
}

// PromptSection is one distinct system prompt or tool schema text by hash.
type PromptSection struct {
	Hash string `json:"hash"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func promptRecordKey(conversationID, runID string, turn, attempt int) string {
	return fmt.Sprintf("prompt/%s/%s/%06d/%03d", conversationID, runID, turn, attempt)
}
func promptSectionKey(hash string) string { return "prompt-section/" + hash }

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ContextChange is the data of a context entry: the parts of the request
// context that changed since the previous context entry of the current
// context, at the position they took effect. A nil part did not change.
// The first entry after a reset or compaction records both parts.
type ContextChange struct {
	System    *string         `json:"system,omitempty"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	ToolNames []string        `json:"tool_names,omitempty"`
	Model     string          `json:"model,omitempty"`
}

// currentContext folds the context entries of the conversation's current
// context (since its newest reset or compaction) into the effective system
// prompt and tool definitions, through cutoff.
func currentContext(ctx context.Context, snap storage.Snapshot, conversationID string) (system *string, tools json.RawMessage, err error) {
	err = History(ctx, snap, conversationID, 0, func(owner string, seq uint64, e Entry) error {
		switch e.Type {
		case entryReset, entryCompaction:
			system, tools = nil, nil
		case entryContext:
			var cc ContextChange
			if json.Unmarshal(e.Data, &cc) != nil {
				return fmt.Errorf("%w: context entry", storage.ErrCorrupt)
			}
			if cc.System != nil {
				system = cc.System
			}
			if cc.Tools != nil {
				tools = cc.Tools
			}
		}
		return nil
	})
	return system, tools, err
}

// contextChangeOps appends a context entry when the request's system prompt
// or tools differ from the current context, so the transcript carries each
// instruction or tool change once, where it took effect. The entry goes
// directly before the response, which the caller appends next. It advances c.
func contextChangeOps(ctx context.Context, view storage.Snapshot, c *Conversation, system string, tools json.RawMessage, names []string, model string) ([]storage.Operation, error) {
	curSystem, curTools, err := currentContext(ctx, view, c.ID)
	if err != nil {
		return nil, err
	}
	var cc ContextChange
	if curSystem == nil || *curSystem != system {
		cc.System = &system
	}
	if curTools == nil || !sameJSON(curTools, tools) {
		cc.Tools = append(json.RawMessage(nil), tools...)
		cc.ToolNames = names
		if cc.ToolNames == nil {
			cc.ToolNames = []string{}
		}
	}
	if cc.System == nil && cc.Tools == nil {
		return nil, nil
	}
	cc.Model = model
	data, _ := json.Marshal(cc)
	c.EntrySequence++
	entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: view.Revision() + 1, Type: entryContext, Data: data, Time: time.Now().UTC()}
	return []storage.Operation{record(entryKey(c.ID, c.EntrySequence), entry)}, nil
}

// PromptRecords lists the request shapes of a conversation: stored records
// of older stores, then one record per context entry, carrying the full
// effective context at that position.
func (r *Runtime) PromptRecords(ctx context.Context, conversationID string, limit int) ([]PromptRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := snap.Page("prompt/"+conversationID+"/", "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]PromptRecord, 0, len(rows))
	for _, row := range rows {
		var p PromptRecord
		if json.Unmarshal(row.Value, &p) != nil {
			return nil, storage.ErrCorrupt
		}
		out = append(out, p)
	}
	var system string
	var tools json.RawMessage
	var names []string
	err = History(ctx, snap, conversationID, 0, func(owner string, seq uint64, e Entry) error {
		if e.Type != entryContext || len(out) >= limit {
			return nil
		}
		var cc ContextChange
		if json.Unmarshal(e.Data, &cc) != nil {
			return fmt.Errorf("%w: context entry", storage.ErrCorrupt)
		}
		if cc.System != nil {
			system = *cc.System
		}
		if cc.Tools != nil {
			tools, names = cc.Tools, cc.ToolNames
		}
		out = append(out, PromptRecord{ConversationID: conversationID, SystemHash: contentHash([]byte(system)), ToolsHash: contentHash(tools), ToolNames: names, Model: cc.Model, Time: e.Time, Entry: seq})
		return nil
	})
	return out, err
}

// PromptSection reads the text behind a hash: a stored section of an older
// store, else the matching text of any context entry.
func (r *Runtime) PromptSection(ctx context.Context, hash string) (PromptSection, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return PromptSection{}, false, err
	}
	if sec, ok, err := read[PromptSection](snap, promptSectionKey(hash)); ok || err != nil {
		return sec, ok, err
	}
	var found PromptSection
	errFound := errors.New("found")
	err = pageAll(snap, "entry/", func(row storage.Record) error {
		var e Entry
		if json.Unmarshal(row.Value, &e) != nil || e.Type != entryContext {
			return nil
		}
		var cc ContextChange
		if json.Unmarshal(e.Data, &cc) != nil {
			return nil
		}
		if cc.System != nil && contentHash([]byte(*cc.System)) == hash {
			found = PromptSection{Hash: hash, Kind: "system", Text: *cc.System}
			return errFound
		}
		if cc.Tools != nil && contentHash(cc.Tools) == hash {
			found = PromptSection{Hash: hash, Kind: "tools", Text: string(cc.Tools)}
			return errFound
		}
		return nil
	})
	if errors.Is(err, errFound) {
		return found, true, nil
	}
	return PromptSection{}, false, err
}
