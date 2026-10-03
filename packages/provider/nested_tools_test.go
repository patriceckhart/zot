package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNestedDisplayMetadataDoesNotEnterProviderRequests(t *testing.T) {
	calls := map[string][]NestedToolCall{"parent": {{ID: "parent/1", Name: "read", Args: json.RawMessage(`{"path":"private.txt"}`), Result: "private intermediate result", Status: "completed", Executed: true}}}
	raw, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Model: "gpt-5", Messages: []Message{
		{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "parent", Name: "batch", Arguments: json.RawMessage(`{}`)}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "parent", Content: []Content{TextBlock{Text: "filtered"}}}}, Meta: map[string]string{NestedToolCallsMetaKey: string(raw)}},
	}}
	chat, err := (&openaiClient{name: "openai"}).buildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	responses, err := (&codexClient{providerName: "openai"}).buildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := (&anthropicClient{}).buildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	gemini, _, err := (&geminiClient{}).buildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []any{chat, responses, anthropic, gemini} {
		encoded, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{NestedToolCallsMetaKey, "private.txt", "private intermediate result", "parent/1"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("display metadata leaked into %T request: %s", wire, encoded)
			}
		}
		if !strings.Contains(string(encoded), "filtered") {
			t.Fatalf("outer result missing in %T request", wire)
		}
	}
}

func TestNestedDisplayMetadataIsOptional(t *testing.T) {
	for _, value := range []string{"", "not JSON", `{"parent":1}`} {
		if calls := (Message{Meta: map[string]string{NestedToolCallsMetaKey: value}}).NestedToolCalls(); len(calls) != 0 {
			t.Fatalf("invalid metadata produced display calls: %+v", calls)
		}
	}
}
