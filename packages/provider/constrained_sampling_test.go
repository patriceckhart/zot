package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func scriptTool() Tool {
	return Tool{Name: "codemode", Description: "Run JavaScript", Schema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}`), ConstrainedSampling: &ConstrainedSampling{Type: "grammar", Variants: map[string]string{"openai_lark": `start: /[\s\S]+/`}}}
}

func TestGrammarCapability(t *testing.T) {
	for _, tc := range []struct {
		provider, id    string
		responses, want bool
	}{
		{"openai", "gpt-5", true, true}, {"openai-codex", "gpt-6.1", true, true},
		{"github-copilot", "gpt-5-mini", true, true}, {"openai", "gpt-4.1", true, false},
		{"openai", "o3", true, false}, {"custom", "gpt-5", true, false},
		{"openrouter", "gpt-5", true, false}, {"openai", "gpt-5", false, false},
	} {
		if got := supportsGrammarTools(Model{ID: tc.id}, tc.provider, tc.responses); got != tc.want {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}

func TestResponsesGrammarRequestAndReplay(t *testing.T) {
	code := "return \"Grüße\"\n"
	args, _ := json.Marshal(map[string]string{"code": code})
	req := Request{Model: "gpt-5", Tools: []Tool{scriptTool(), {Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}}, Messages: []Message{
		{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "call_1", Name: "codemode", Arguments: args}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call_1", Content: []Content{TextBlock{Text: "done"}}}}},
	}}
	for _, provider := range []string{"openai", "openai-codex", "custom"} {
		client := &codexClient{providerName: provider}
		wire, err := client.buildRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		native := provider != "custom"
		if native {
			if wire.Tools[0].Type != "custom" || wire.Tools[0].Format.Syntax != "lark" || len(wire.Tools[0].Parameters) != 0 {
				t.Fatalf("tool: %+v", wire.Tools[0])
			}
			call, ok := wire.Input[0].(codexCustomCall)
			if !ok || call.Input != code {
				t.Fatalf("call: %+v", wire.Input[0])
			}
			if wire.Input[1].(codexFunctionCallOutput).Type != "custom_tool_call_output" {
				t.Fatalf("output: %+v", wire.Input[1])
			}
		} else if wire.Tools[0].Type != "function" || wire.Input[0].(codexFunctionCall).Arguments != string(args) {
			t.Fatalf("fallback: %+v", wire)
		}
		if wire.Tools[1].Type != "function" {
			t.Fatal("ordinary tool changed")
		}
	}
}

func TestResponsesCustomInputStreaming(t *testing.T) {
	for _, doneOnly := range []bool{false, true} {
		code := "return \"Grüße\"\n"
		input, _ := json.Marshal(code)
		var sse strings.Builder
		sse.WriteString("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call_1\",\"name\":\"codemode\"}}\n\n")
		if !doneOnly {
			for _, delta := range []string{"return ", "\"Grüße\"", "\n"} {
				raw, _ := json.Marshal(delta)
				sse.WriteString("data: {\"type\":\"response.custom_tool_call_input.delta\",\"output_index\":0,\"delta\":" + string(raw) + "}\n\n")
			}
			sse.WriteString("data: {\"type\":\"response.custom_tool_call_input.done\",\"output_index\":0,\"input\":" + string(input) + "}\n\n")
		}
		sse.WriteString("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"input\":" + string(input) + "}}\n\n")
		sse.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		events := make(chan Event, 32)
		client := &codexClient{providerName: "openai"}
		go client.runStream(context.Background(), &http.Response{Body: io.NopCloser(strings.NewReader(sse.String()))}, Request{Model: "gpt-5", Tools: []Tool{scriptTool()}}, events)
		var deltas strings.Builder
		var done EventDone
		ends := 0
		for event := range events {
			switch e := event.(type) {
			case EventToolArgs:
				deltas.WriteString(e.Delta)
			case EventDone:
				done = e
			case EventToolEnd:
				ends++
			}
		}
		var args map[string]string
		if json.Unmarshal([]byte(deltas.String()), &args) != nil || args["code"] != code {
			t.Fatalf("deltas: %q", deltas.String())
		}
		call := done.Message.Content[0].(ToolCallBlock)
		if json.Unmarshal(call.Arguments, &args) != nil || args["code"] != code || done.Stop != StopToolUse || ends != 1 {
			t.Fatalf("done: %+v", done)
		}
	}
}

func TestCompletionsCustomStreaming(t *testing.T) {
	// Build chunks as JSON to exercise quoted strings without fixture escaping.
	var wire strings.Builder
	for i, delta := range []string{"return ", "\"Grüße\"\n"} {
		call := map[string]any{"index": 0, "custom": map[string]string{"input": delta}}
		if i == 0 {
			call["id"] = "call_1"
			call["type"] = "custom"
			call["custom"].(map[string]string)["name"] = "codemode"
		}
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{call}}}}})
		wire.WriteString("data: " + string(chunk) + "\n\n")
	}
	wire.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	events := make(chan Event, 32)
	client := &openaiClient{name: "openai"}
	go client.runStream(context.Background(), &http.Response{Body: io.NopCloser(strings.NewReader(wire.String()))}, Request{Model: "gpt-5", Tools: []Tool{scriptTool()}}, events)
	var deltas strings.Builder
	for event := range events {
		switch e := event.(type) {
		case EventToolArgs:
			deltas.WriteString(e.Delta)
		case EventDone:
			if e.Stop != StopToolUse {
				t.Fatal(e)
			}
		}
	}
	var args map[string]string
	// Unexpected native calls still get a valid input property, even without an opt-in.
	if json.Unmarshal([]byte(deltas.String()), &args) != nil || args["input"] != "return \"Grüße\"\n" {
		t.Fatalf("deltas: %q", deltas.String())
	}
}
