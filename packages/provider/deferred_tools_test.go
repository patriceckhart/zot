package provider

import (
	"encoding/json"
	"testing"
)

func TestKimiK3DeferredToolsLoadAtToolResult(t *testing.T) {
	client := NewMoonshot("token", "").(*openaiClient)
	tools := []Tool{
		{Name: "search_tools", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "lookup_weather", Schema: json.RawMessage(`{"type":"object"}`), Deferred: true},
	}
	wire, err := client.buildRequest(Request{
		Model: "kimi-k3",
		Tools: tools,
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "weather"}}},
			{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "call-1", Name: "search_tools", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call-1", Content: []Content{TextBlock{Text: "found"}}}}, AddedToolNames: []string{"lookup_weather"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Function == nil || wire.Tools[0].Function.Name != "search_tools" {
		t.Fatalf("top-level tools = %+v, want only search_tools", wire.Tools)
	}
	var loaded []oaiTool
	for _, message := range wire.Messages {
		if message.Role == "system" && len(message.Tools) > 0 {
			loaded = message.Tools
		}
	}
	if len(loaded) != 1 || loaded[0].Function == nil || loaded[0].Function.Name != "lookup_weather" {
		t.Fatalf("loaded tools = %+v, want lookup_weather", loaded)
	}
}

// The deferred-tools handshake is a Moonshot API extension. Another provider
// serving the same model id must get the standard wire instead: no tools-bearing
// system message (it carries no content, which strict schemas reject), and the
// activated tool in the top-level definitions where every provider reads it.
func TestKimiK3DeferredToolsSkippedForNonMoonshotProvider(t *testing.T) {
	client := NewGondola("token", "").(*openaiClient)
	tools := []Tool{
		{Name: "search_tools", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "lookup_weather", Schema: json.RawMessage(`{"type":"object"}`), Deferred: true},
	}
	wire, err := client.buildRequest(Request{
		Model: "kimi-k3",
		Tools: tools,
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "weather"}}},
			{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "call-1", Name: "search_tools", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call-1", Content: []Content{TextBlock{Text: "found"}}}}, AddedToolNames: []string{"lookup_weather"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range wire.Messages {
		if len(message.Tools) > 0 {
			t.Fatalf("message %q carries tools; deferred injection must not reach a non-Moonshot provider", message.Role)
		}
		if message.Content == nil && len(message.ToolCalls) == 0 {
			t.Fatalf("message %q has neither content nor tool calls", message.Role)
		}
	}
	var names []string
	for _, tool := range wire.Tools {
		if tool.Function != nil {
			names = append(names, tool.Function.Name)
		}
	}
	if len(names) != 2 || names[0] != "search_tools" || names[1] != "lookup_weather" {
		t.Fatalf("top-level tools = %v, want [search_tools lookup_weather]", names)
	}
}

func TestDeferredToolsFallbackToActiveTopLevelDefinitions(t *testing.T) {
	tools := []Tool{
		{Name: "base", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "late", Schema: json.RawMessage(`{"type":"object"}`), Deferred: true},
	}
	builders := map[string]func(Request) ([]string, error){
		"openai": func(req Request) ([]string, error) {
			wire, err := NewOpenAI("token", "").(*openaiClient).buildRequest(req)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(wire.Tools))
			for _, tool := range wire.Tools {
				if tool.Function != nil {
					names = append(names, tool.Function.Name)
				}
			}
			return names, nil
		},
		"anthropic": func(req Request) ([]string, error) {
			req.Model = "claude-sonnet-4-6"
			wire, err := NewAnthropic("token", "").(*anthropicClient).buildRequest(req)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(wire.Tools))
			for _, tool := range wire.Tools {
				names = append(names, tool.Name)
			}
			return names, nil
		},
		"bedrock": func(req Request) ([]string, error) {
			req.Model = "anthropic.claude-sonnet-4-5-20250929-v1:0"
			wire, err := (&bedrockClient{region: "us-east-1"}).buildRequest(req)
			if err != nil {
				return nil, err
			}
			if wire.ToolConfig == nil {
				return nil, nil
			}
			names := make([]string, 0, len(wire.ToolConfig.Tools))
			for _, tool := range wire.ToolConfig.Tools {
				names = append(names, tool.ToolSpec.Name)
			}
			return names, nil
		},
		"gemini": func(req Request) ([]string, error) {
			req.Model = "gemini-2.5-pro"
			wire, _, err := NewGemini("token", "").(*geminiClient).buildRequest(req)
			if err != nil {
				return nil, err
			}
			var names []string
			for _, group := range wire.Tools {
				for _, tool := range group.FunctionDeclarations {
					names = append(names, tool.Name)
				}
			}
			return names, nil
		},
		"openai-codex": func(req Request) ([]string, error) {
			req.Model = "gpt-5.5"
			wire, err := NewOpenAICodex("token", "account", "").(*codexClient).buildRequest(req)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(wire.Tools))
			for _, tool := range wire.Tools {
				names = append(names, tool.Name)
			}
			return names, nil
		},
	}

	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			initial, err := build(Request{Model: "gpt-5", Tools: tools})
			if err != nil {
				t.Fatal(err)
			}
			if len(initial) != 1 || initial[0] != "base" {
				t.Fatalf("initial tools = %v, want [base]", initial)
			}

			activated, err := build(Request{
				Model:    "gpt-5",
				Tools:    tools,
				Messages: []Message{{Role: RoleTool, AddedToolNames: []string{"late"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(activated) != 2 || activated[0] != "base" || activated[1] != "late" {
				t.Fatalf("activated tools = %v, want [base late]", activated)
			}
		})
	}
}

func TestKimiK3AndGrok45CatalogMetadata(t *testing.T) {
	for _, tc := range []struct {
		provider string
		model    string
	}{
		{"kimi", "k3"},
		{"moonshotai", "kimi-k3"},
		{"moonshotai-cn", "kimi-k3"},
		{"openrouter", "moonshotai/kimi-k3"},
		{"vercel-ai-gateway", "moonshotai/kimi-k3"},
	} {
		m, err := FindModel(tc.provider, tc.model)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.provider, tc.model, err)
		}
		if m.MaxOutput != 131072 || !m.Reasoning {
			t.Fatalf("%s/%s metadata = %+v", tc.provider, tc.model, m)
		}
	}
	m, err := FindModel("xai", "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Reasoning || m.MaxOutput != 30000 || m.API != APIResponses {
		t.Fatalf("xai/grok-4.5 metadata = %+v", m)
	}
}
