package swarm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestToolActivityAndTranscriptReplay(t *testing.T) {
	a := &Agent{}
	sink := agentSink{a: a}
	events := []Event{
		NewEvent("tool_call", map[string]any{"id": "a", "name": "bash", "args": map[string]any{"command": "go test ./..."}}),
		NewEvent("tool_call", map[string]any{"id": "b", "name": "read", "args": map[string]any{"path": "go.mod"}}),
		NewEvent("turn_end", map[string]any{"stop": "tool_use"}),
	}
	for _, ev := range events {
		applyEventToSink(ev, sink)
	}
	if !strings.HasPrefix(a.Activity(), "tool: ") {
		t.Fatalf("activity during execution = %q", a.Activity())
	}
	result := NewEvent("tool_result", map[string]any{"id": "b", "is_error": true, "content": []any{map[string]any{"type": "text", "text": "read failed"}}})
	events = append(events, result)
	applyEventToSink(result, sink)
	if a.Activity() != "tool: bash" {
		t.Fatalf("remaining tool activity = %q", a.Activity())
	}
	result = NewEvent("tool_result", map[string]any{"id": "a", "content": []any{map[string]any{"type": "text", "text": "tests passed\nsecond line"}}})
	events = append(events, result)
	applyEventToSink(result, sink)
	if a.Activity() != "thinking" {
		t.Fatalf("post-tool activity = %q", a.Activity())
	}
	applyEventToSink(NewEvent("turn_end", map[string]any{"step": float64(1), "error": "cancelled"}), sink)
	if a.Activity() != "idle" {
		t.Fatalf("completed activity = %q", a.Activity())
	}
	text := strings.Join(a.Transcript(), "\n")
	for _, want := range []string{"bash", "go test ./...", "go.mod", "read failed", "tests passed", "second line", "error"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if a.Snapshot().LastAssistant != "" {
		t.Fatal("tool output polluted final assistant response")
	}
	replay := &Agent{}
	replayEventsIntoAgent(replay, events)
	if strings.Join(replay.Transcript(), "\n") != text {
		t.Fatalf("replay differs: %v", replay.Transcript())
	}
}

func TestToolActivityClearsOnPromptCancellation(t *testing.T) {
	a := &Agent{}
	sink := agentSink{a: a}
	applyEventToSink(NewEvent("tool_call", map[string]any{"id": "cancelled", "name": "bash"}), sink)
	applyEventToSink(NewEvent("turn_end", map[string]any{"stop": "tool_use"}), sink)
	if a.Activity() != "tool: bash" {
		t.Fatalf("running command = %q", a.Activity())
	}
	applyEventToSink(NewEvent("turn_end", map[string]any{"step": float64(1), "error": "context canceled"}), sink)
	if a.Activity() != "idle" {
		t.Fatalf("cancelled command = %q", a.Activity())
	}
	applyEventToSink(NewEvent("tool_call", map[string]any{"id": "next", "name": "read"}), sink)
	if a.Activity() != "tool: read" {
		t.Fatalf("stale tool carried into next prompt: %q", a.Activity())
	}
}

func TestHostToolEntriesPreservePayloads(t *testing.T) {
	call := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "running tests"}, provider.ToolCallBlock{ID: "a", Name: "bash", Arguments: json.RawMessage(`{"command":"go test ./..."}`)}}}
	result := provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "a", Content: []provider.Content{provider.TextBlock{Text: "test output"}}}}}
	var entries []continuous.Entry
	for n, msg := range []provider.Message{call, result} {
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		typ := "assistant"
		if n == 1 {
			typ = "tool_result"
		}
		entries = append(entries, continuous.Entry{Type: typ, Message: raw})
	}
	a := &Agent{}
	r := &HostRunner{}
	r.mirrorEntries(entries[:1], agentSink{a: a}, nil)
	if a.Activity() != "tool: bash" {
		t.Fatalf("host activity = %q", a.Activity())
	}
	r.mirrorEntries(entries[1:], agentSink{a: a}, nil)
	text := strings.Join(a.Transcript(), "\n")
	if !strings.Contains(text, "go test ./...") || !strings.Contains(text, "test output") {
		t.Fatalf("host transcript = %s", text)
	}
	var replayEvents []Event
	for _, entry := range entries {
		emitEntry(func(typ string, data map[string]any) {
			raw, _ := json.Marshal(NewEvent(typ, data))
			var ev Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatal(err)
			}
			replayEvents = append(replayEvents, ev)
		}, entry)
	}
	replay := &Agent{}
	replayEventsIntoAgent(replay, replayEvents)
	if strings.Join(replay.Transcript(), "\n") != text {
		t.Fatalf("host replay differs: %v", replay.Transcript())
	}
}
