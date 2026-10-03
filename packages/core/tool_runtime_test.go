package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

type orchestrationTool struct {
	name string
	next string
}

func (t orchestrationTool) Name() string          { return t.name }
func (orchestrationTool) Description() string     { return "Call another tool" }
func (orchestrationTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t orchestrationTool) Execute(ctx context.Context, _ json.RawMessage, _ func(string)) (ToolResult, error) {
	runtime := ToolRuntimeFromContext(ctx)
	if runtime == nil {
		return ToolResult{}, context.Canceled
	}
	return runtime.Call(ctx, t.next, json.RawMessage(`{}`)), nil
}

func TestToolRuntimeRecursionAndDepth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools Registry
	}{
		{"recursion", NewRegistry(orchestrationTool{name: "a", next: "b"}, orchestrationTool{name: "b", next: "a"})},
		{"depth", NewRegistry(orchestrationTool{name: "a", next: "b"}, orchestrationTool{name: "b", next: "c"}, orchestrationTool{name: "c", next: "d"}, orchestrationTool{name: "d", next: "e"}, &recordingTool{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := NewAgent(nil, "test", "", tc.tools)
			res := ag.CallTool(context.Background(), "root", "a", json.RawMessage(`{}`), nil)
			if !res.IsError || res.Status != "blocked" || !strings.Contains(res.Content[0].(provider.TextBlock).Text, "limit exceeded") {
				t.Fatalf("%+v", res)
			}
		})
	}
}

func TestToolRuntimeKeepsIntermediateResultsOutOfTranscript(t *testing.T) {
	client := &preludeClient{}
	ag := NewAgent(client, "test", "", NewRegistry(orchestrationTool{name: "batch", next: "echo"}, &recordingTool{}))
	call := provider.ToolCallBlock{ID: "outer", Name: "batch", Arguments: json.RawMessage(`{}`)}
	if err := ag.PromptWithTool(context.Background(), "run", call, "test", nil); err != nil {
		t.Fatal(err)
	}
	if len(client.messages) != 3 {
		t.Fatalf("messages: %+v", client.messages)
	}
	gotCall := client.messages[1].Content[0].(provider.ToolCallBlock)
	gotResult := client.messages[2].Content[0].(provider.ToolResultBlock)
	if gotCall.Name != "batch" || gotResult.CallID != gotCall.ID || gotResult.IsError || len(client.messages[2].Content) != 1 {
		t.Fatalf("unpaired outer result: %+v", client.messages)
	}
}

func TestToolRuntimeUsesCurrentRegistry(t *testing.T) {
	ag := NewAgent(nil, "test", "", NewRegistry(orchestrationTool{name: "batch", next: "echo"}))
	res := ag.CallTool(context.Background(), "outer", "batch", json.RawMessage(`{}`), nil)
	if !res.IsError {
		t.Fatal("missing tool succeeded")
	}
	ag.SetTools(NewRegistry(orchestrationTool{name: "batch", next: "echo"}, &recordingTool{}))
	res = ag.CallTool(context.Background(), "outer", "batch", json.RawMessage(`{}`), nil)
	if res.IsError {
		t.Fatalf("updated registry not used: %+v", res)
	}
}
