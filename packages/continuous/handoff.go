package continuous

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// HandoffTool lets the model close its current context and continue in a
// fresh one: the tool result carries a control marker, and the service, after
// recording the result, commits a reset entry with the handoff note and admits
// exactly one continuation input in the same transaction. The continuation
// uses a request ID derived from the tool call, so a replayed or re-stepped
// round cannot admit it twice. The current run ends completed.
type HandoffTool struct{}

func (HandoffTool) Name() string { return "handoff" }
func (HandoffTool) Description() string {
	return "Close the current context and continue with a fresh one. Provide a handoff note summarizing the state and the instruction to continue with. Use when the context has grown long or the task changes direction."
}
func (HandoffTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"note":{"type":"string","description":"what the next context needs to know"},"continue":{"type":"string","description":"the instruction the next context starts with"}},"required":["note","continue"]}`)
}
func (HandoffTool) ReplayPolicy() core.ToolReplayPolicy { return core.ReplaySafe }

// handoffRequest is the control payload the service reads from the result.
type handoffRequest struct {
	Note     string `json:"note"`
	Continue string `json:"continue"`
}

func (HandoffTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var req handoffRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return core.ToolResult{}, fmt.Errorf("handoff: %w", err)
	}
	req.Note, req.Continue = strings.TrimSpace(req.Note), strings.TrimSpace(req.Continue)
	if req.Continue == "" {
		return core.ToolResult{}, fmt.Errorf("handoff requires a continuation instruction")
	}
	if _, ok := ToolCallFromContext(ctx); !ok {
		return core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "handoff is only available inside a continuous run"}}}, nil
	}
	data, _ := json.Marshal(req)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "handing off to a fresh context"}}, StructuredContent: data}, nil
}

// handoffOps builds the reset and continuation for a handoff result. The
// continuation is admitted with policy queue and a stable request ID, so
// Step picks it up as the next run.
func handoffOps(snap storage.Snapshot, c *Conversation, run Run, callID string, req handoffRequest) ([]storage.Operation, error) {
	actor := "handoff:" + run.ID
	requestID := "handoff:" + callID
	if _, ok, err := read[Submission](snap, hashedKey("dedup/submit/", c.ID, actor, requestID)); err != nil {
		return nil, err
	} else if ok {
		// Already admitted by an earlier attempt of this round.
		return nil, nil
	}
	c.EntrySequence++
	reset := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryReset, Content: req.Note, Time: time.Now().UTC()}
	ops := []storage.Operation{record(entryKey(c.ID, c.EntrySequence), reset)}
	_, admit, err := admissionOps(snap, c, actor, requestID, req.Continue, "")
	if err != nil {
		return nil, err
	}
	return append(ops, admit...), nil
}

// handoffFromResult extracts the control payload when the tool was the
// handoff tool and the call succeeded.
func handoffFromResult(name string, result core.ToolResult) (handoffRequest, bool) {
	if name != (HandoffTool{}).Name() || result.IsError || len(result.StructuredContent) == 0 {
		return handoffRequest{}, false
	}
	var req handoffRequest
	if json.Unmarshal(result.StructuredContent, &req) != nil || req.Continue == "" {
		return handoffRequest{}, false
	}
	return req, true
}
