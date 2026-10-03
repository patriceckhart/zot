package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestNestedDisplayMetadataForMultipleParentsAndFailedCalls(t *testing.T) {
	ag := NewAgent(nil, "test", "", NewRegistry(orchestrationTool{name: "batch", next: "inner"}, orchestrationTool{name: "inner", next: "echo"}, &recordingTool{}))
	ag.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return call.Name != "echo", "refused", json.RawMessage(`{"rewritten":true}`)
	}
	message, failed := ag.executeTools(context.Background(), provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
		provider.ToolCallBlock{ID: "first", Name: "batch", Arguments: json.RawMessage(`{}`)},
		provider.ToolCallBlock{ID: "second", Name: "batch", Arguments: json.RawMessage(`{}`)},
	}}, nil)
	if !failed || len(message.Content) != 2 {
		t.Fatalf("unexpected outer results: %+v", message)
	}
	for _, id := range []string{"first", "second"} {
		calls := message.NestedToolCalls()[id]
		if len(calls) != 2 || calls[0].ID != id+"/1" || calls[1].ID != id+"/1/1" {
			t.Fatalf("nested calls missing or duplicated: %+v", calls)
		}
		if calls[1].Status != "blocked" || calls[1].Executed || !calls[1].IsError || calls[1].Result != "refused" || string(calls[1].Args) != `{"rewritten":true}` {
			t.Fatalf("nested refusal not recorded: %+v", calls[1])
		}
	}
}

func TestContextSnapshotOmitsDisplayWithoutMutatingSessions(t *testing.T) {
	message := provider.Message{Role: provider.RoleTool, Meta: map[string]string{
		provider.NestedToolCallsMetaKey: `{"parent":[]}`,
		"tool_state:codemode":           "{}", "synthetic_tool_call": "true",
	}}
	ag := NewAgent(nil, "test", "", nil)
	ag.SetMessages([]provider.Message{message})
	_, _, context := ag.ContextSnapshot()
	if _, exists := context[0].Meta[provider.NestedToolCallsMetaKey]; exists {
		t.Fatal("display metadata entered model context")
	}
	if context[0].Meta["tool_state:codemode"] != "{}" || context[0].Meta["synthetic_tool_call"] != "true" {
		t.Fatal("provider and state metadata were removed")
	}
	if ag.Messages()[0].Meta[provider.NestedToolCallsMetaKey] == "" {
		t.Fatal("model snapshot mutated saved display metadata")
	}
}

func TestNestedDisplayKeepsStartOrderAndOmitsImagePayloads(t *testing.T) {
	runtime := &ToolRuntime{}
	for _, id := range []string{"parent/1", "parent/2"} {
		runtime.recordEvent(EvToolCall{ID: id, Name: "echo", Args: json.RawMessage(`{}`)})
	}
	for _, id := range []string{"parent/2", "parent/1"} {
		runtime.recordEvent(EvToolResult{ID: id, Status: "completed", Executed: true, Args: json.RawMessage(`{}`), Result: ToolResult{Content: []provider.Content{
			provider.TextBlock{Text: id}, provider.ImageBlock{MimeType: "image/png", Data: []byte("private image payload")},
		}}})
	}
	calls := runtime.nestedCalls()
	if len(calls) != 2 || calls[0].ID != "parent/1" || calls[1].ID != "parent/2" {
		t.Fatalf("completion order moved calls: %+v", calls)
	}
	meta := toolResultMetadata(nil, map[string][]provider.NestedToolCall{"parent": calls})
	if strings.Contains(meta[provider.NestedToolCallsMetaKey], "private image payload") || !strings.Contains(calls[0].Result, "[image image/png, 21 bytes]") {
		t.Fatalf("image not reduced to a caption: %+v", calls)
	}
	if toolResultMetadata(nil, map[string][]provider.NestedToolCall{"parent": nil}) != nil {
		t.Fatal("ordinary calls gained empty display metadata")
	}
}
