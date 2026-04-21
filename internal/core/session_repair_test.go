package core

import (
	"testing"

	"github.com/patriceckhart/zot/internal/provider"
)

// TestRepairToolUseResultPairsAppendsStub covers the most common
// corruption shape: assistant tool_use row on disk, no
// corresponding tool_result row after it. The repair must inject
// a stub tool_result so the next request passes the provider's
// validity check.
func TestRepairToolUseResultPairsAppendsStub(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: "t1", Name: "read", Arguments: []byte(`{"path":"/tmp/a"}`)},
		}},
	}
	out := repairToolUseResultPairs(msgs)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages after repair, got %d", len(out))
	}
	if out[2].Role != provider.RoleTool {
		t.Fatalf("expected tool-role message at index 2, got %v", out[2].Role)
	}
	tr, ok := out[2].Content[0].(provider.ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock, got %T", out[2].Content[0])
	}
	if tr.CallID != "t1" {
		t.Errorf("CallID: want t1, got %q", tr.CallID)
	}
	if !tr.IsError {
		t.Errorf("expected IsError=true on stub")
	}
}

// TestRepairToolUseResultPairsMergesIntoExistingToolMessage covers
// the case where a tool-role message exists but is missing one of
// the call ids. The stub must be merged into it (no new message)
// so the ordering stays tool_use, tool_result, tool_use, tool_result.
func TestRepairToolUseResultPairsMergesIntoExistingToolMessage(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: "t1", Name: "read"},
			provider.ToolCallBlock{ID: "t2", Name: "bash"},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: "t1", Content: []provider.Content{provider.TextBlock{Text: "ok"}}},
		}},
	}
	out := repairToolUseResultPairs(msgs)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages (merged), got %d", len(out))
	}
	if out[2].Role != provider.RoleTool {
		t.Fatalf("expected tool-role at index 2")
	}
	if len(out[2].Content) != 2 {
		t.Fatalf("expected 2 tool_result blocks after merge, got %d", len(out[2].Content))
	}
	// second block must be the stub for t2
	tr, ok := out[2].Content[1].(provider.ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock at [1], got %T", out[2].Content[1])
	}
	if tr.CallID != "t2" {
		t.Errorf("merged stub CallID: want t2, got %q", tr.CallID)
	}
}

// TestRepairToolUseResultPairsLeavesValidTranscriptsAlone verifies
// the repair is a no-op when everything is already paired.
func TestRepairToolUseResultPairsLeavesValidTranscriptsAlone(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: "t1", Name: "read"},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: "t1", Content: []provider.Content{provider.TextBlock{Text: "ok"}}},
		}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}},
	}
	out := repairToolUseResultPairs(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("no-op expected, len %d -> %d", len(msgs), len(out))
	}
	for i := range msgs {
		if msgs[i].Role != out[i].Role {
			t.Errorf("msg %d role changed", i)
		}
	}
}

// TestRepairToolUseResultPairsEmpty handles the zero-value case.
func TestRepairToolUseResultPairsEmpty(t *testing.T) {
	if out := repairToolUseResultPairs(nil); len(out) != 0 {
		t.Fatalf("expected empty, got %d", len(out))
	}
}
