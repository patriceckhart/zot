package continuous

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// PromptRecord captures the effective model request shape of one attempt:
// the system prompt and tool schemas as content hashes, with the full text
// stored once per distinct hash. It lets a transcript explain which
// instructions and tools a response was generated against, without storing
// the prompt on every entry.
type PromptRecord struct {
	ConversationID string    `json:"conversation_id"`
	RunID          string    `json:"run_id"`
	Turn           int       `json:"turn"`
	Attempt        int       `json:"attempt"`
	SystemHash     string    `json:"system_hash"`
	ToolsHash      string    `json:"tools_hash"`
	ToolNames      []string  `json:"tool_names"`
	Model          string    `json:"model"`
	Time           time.Time `json:"time"`
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

// promptOps records the request shape of an attempt. Sections are written
// only when their hash is new, so repeated requests cost one small record.
func promptOps(snap storage.Snapshot, c Conversation, run Run, model, system string, tools []byte, toolNames []string) []storage.Operation {
	systemHash, toolsHash := contentHash([]byte(system)), contentHash(tools)
	ops := []storage.Operation{record(promptRecordKey(c.ID, run.ID, run.Turn, run.Attempt), PromptRecord{ConversationID: c.ID, RunID: run.ID, Turn: run.Turn, Attempt: run.Attempt, SystemHash: systemHash, ToolsHash: toolsHash, ToolNames: toolNames, Model: model, Time: time.Now().UTC()})}
	if _, ok := snap.Get(promptSectionKey(systemHash)); !ok {
		ops = append(ops, record(promptSectionKey(systemHash), PromptSection{Hash: systemHash, Kind: "system", Text: system}))
	}
	if _, ok := snap.Get(promptSectionKey(toolsHash)); !ok {
		ops = append(ops, record(promptSectionKey(toolsHash), PromptSection{Hash: toolsHash, Kind: "tools", Text: string(tools)}))
	}
	return ops
}

// PromptRecords lists the request shapes of a conversation's attempts.
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
	return out, nil
}

// PromptSection reads the text behind a hash.
func (r *Runtime) PromptSection(ctx context.Context, hash string) (PromptSection, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return PromptSection{}, false, err
	}
	return read[PromptSection](snap, promptSectionKey(hash))
}
