package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const openRouterLiveGeneralModel = "openai/gpt-4.1"

// Live OpenRouter server-tool batteries. Skipped unless OPENROUTER_API_KEY
// is set so CI and unpaid checkouts never hit the network.
func TestOpenRouterServerToolsLiveDeepSeekFlash(t *testing.T) {
	runOpenRouterServerToolBattery(t, "deepseek/deepseek-v4-flash")
}

func TestOpenRouterServerToolsLivePresetFlash(t *testing.T) {
	runOpenRouterServerToolBattery(t, "@preset/flash")
}

func TestOpenRouterServerToolsLiveGeneral(t *testing.T) {
	runOpenRouterServerToolBatteryOnNativeAPIs(t, openRouterLiveGeneralModel)
}

func TestOpenRouterServerToolsLiveGeneralWithSettingsEnabled(t *testing.T) {
	// Same multidisciplinary model as the settings-on agent path. Chat
	// Completions-capable tools are advertised together (the setting's
	// default-on registry); shell/bash/apply_patch/tool_search still use
	// their native APIs.
	if testing.Short() {
		t.Skip("skip live OpenRouter battery in -short mode")
	}
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	client := NewOpenRouter(key, "")
	settingsTools := make([]Tool, 0, len(openRouterServerToolCases))
	for _, tc := range openRouterServerToolCases {
		if !tc.chatCompletionsUnsupported {
			settingsTools = append(settingsTools, tc.tool)
		}
	}

	for _, tc := range openRouterServerToolCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			switch tc.name {
			case "openrouter:shell", "openrouter:apply_patch", "openrouter:tool_search":
				postOpenRouterJSON(t, ctx, key, openrouterDefaultBaseURL+"/responses", nativeResponsesBody(openRouterLiveGeneralModel, tc))
			case "openrouter:bash":
				postOpenRouterJSON(t, ctx, key, openrouterDefaultBaseURL+"/messages", nativeMessagesBody(openRouterLiveGeneralModel, tc))
			default:
				stream, err := client.Stream(ctx, Request{
					Model:        openRouterLiveGeneralModel,
					Tools:        settingsTools,
					MaxTokens:    256,
					MaxToolCalls: 4,
					SessionID:    "zot-live-settings-" + strings.TrimPrefix(tc.name, "openrouter:"),
					Messages: []Message{{
						Role:    RoleUser,
						Content: []Content{TextBlock{Text: tc.prompt}},
					}},
				})
				if err != nil {
					t.Fatalf("Stream(%s): %v", tc.name, err)
				}
				var done EventDone
				for ev := range stream {
					if e, ok := ev.(EventDone); ok {
						done = e
					}
				}
				if done.Err != nil {
					t.Fatalf("EventDone.Err for %s: %v", tc.name, done.Err)
				}
				if done.Stop == StopError {
					t.Fatalf("stop=error for %s", tc.name)
				}
			}
		})
	}
}

type openRouterServerToolCase struct {
	name   string
	tool   Tool
	prompt string
	// chatCompletionsUnsupported is true when OpenRouter documents the
	// tool as Chat Completions-incompatible (400).
	chatCompletionsUnsupported bool
}

var openRouterServerToolCases = []openRouterServerToolCase{
	{
		name:   "openrouter:web_search",
		tool:   Tool{Name: "openrouter:web_search", Schema: json.RawMessage(`{"max_results":1}`)},
		prompt: "Search the web for 'OpenRouter AI'. You MUST call the web search tool, then reply with one sentence.",
	},
	{
		name:   "openrouter:web_fetch",
		tool:   Tool{Name: "openrouter:web_fetch"},
		prompt: "Fetch https://example.com and quote the h1. You MUST call the web fetch tool.",
	},
	{
		name:   "openrouter:advisor",
		tool:   Tool{Name: "openrouter:advisor", Schema: json.RawMessage(`{"model":"openai/gpt-4.1-mini"}`)},
		prompt: "Ask the advisor whether 17 is prime. You MUST call the advisor tool, then answer in one word.",
	},
	{
		name:   "openrouter:subagent",
		tool:   Tool{Name: "openrouter:subagent", Schema: json.RawMessage(`{"model":"openai/gpt-4.1-mini"}`)},
		prompt: "Delegate to a subagent: return the sum of 21 and 21. You MUST call the subagent tool.",
	},
	{
		name:   "openrouter:fusion",
		tool:   Tool{Name: "openrouter:fusion", Schema: json.RawMessage(`{"analysis_models":["openai/gpt-4.1-mini"],"model":"openai/gpt-4.1-mini"}`)},
		prompt: "Use fusion to answer in one sentence: what is 1+1? You MUST call the fusion tool.",
	},
	{
		name:                       "openrouter:shell",
		tool:                       Tool{Name: "openrouter:shell"},
		prompt:                     "Run `echo hi` in the hosted shell. You MUST call the shell tool.",
		chatCompletionsUnsupported: true,
	},
	{
		name:                       "openrouter:bash",
		tool:                       Tool{Name: "openrouter:bash"},
		prompt:                     "Run `echo hi` with bash. You MUST call the bash tool.",
		chatCompletionsUnsupported: true,
	},
	{
		name:                       "openrouter:apply_patch",
		tool:                       Tool{Name: "openrouter:apply_patch"},
		prompt:                     "Propose a patch that creates hello.txt with hello. You MUST call apply_patch.",
		chatCompletionsUnsupported: true,
	},
	{
		name:   "openrouter:image_generation",
		tool:   Tool{Name: "openrouter:image_generation"},
		prompt: "Generate a tiny image of a red square. You MUST call image generation, then describe the result in one sentence.",
	},
	{
		name:   "openrouter:datetime",
		tool:   Tool{Name: "openrouter:datetime"},
		prompt: "What is the current UTC datetime? You MUST call the datetime tool and then print only the datetime.",
	},
	{
		name:                       "openrouter:tool_search",
		tool:                       Tool{Name: "openrouter:tool_search"},
		prompt:                     "Find a tool that can add numbers. You MUST call tool_search first.",
		chatCompletionsUnsupported: true,
	},
	{
		name:   "openrouter:experimental__search_models",
		tool:   Tool{Name: "openrouter:experimental__search_models", Schema: json.RawMessage(`{"max_results":3}`)},
		prompt: "Search the OpenRouter catalog for cheap flash models. You MUST call the search models tool and list up to 3 ids.",
	},
}

func runOpenRouterServerToolBattery(t *testing.T, model string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skip live OpenRouter battery in -short mode")
	}
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	client := NewOpenRouter(key, "")
	for _, tc := range openRouterServerToolCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			tools := []Tool{tc.tool}
			if tc.name == "openrouter:tool_search" {
				tools = append(tools, Tool{
					Name:        "add_numbers",
					Description: "Add two integers.",
					Schema:      json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
					Deferred:    true,
				})
			}

			stream, err := client.Stream(ctx, Request{
				Model:        model,
				Tools:        tools,
				MaxTokens:    256,
				MaxToolCalls: 4,
				SessionID:    "zot-live-" + strings.ReplaceAll(model, "/", "-") + "-" + strings.TrimPrefix(tc.name, "openrouter:"),
				Messages: []Message{{
					Role:    RoleUser,
					Content: []Content{TextBlock{Text: tc.prompt}},
				}},
			})
			if tc.chatCompletionsUnsupported {
				if err == nil {
					// Drain so the HTTP body is not leaked if the
					// server accepted the request anyway.
					for range stream {
					}
					t.Fatalf("expected chat-completions rejection for %s, got stream", tc.name)
				}
				if !strings.Contains(strings.ToLower(err.Error()), "chat-completions") &&
					!strings.Contains(strings.ToLower(err.Error()), "not available") {
					t.Fatalf("unexpected error for unsupported tool %s: %v", tc.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Stream(%s, %s): %v", model, tc.name, err)
			}
			var done EventDone
			for ev := range stream {
				if e, ok := ev.(EventDone); ok {
					done = e
				}
			}
			if done.Err != nil {
				t.Fatalf("EventDone.Err for %s/%s: %v", model, tc.name, done.Err)
			}
			if done.Stop == StopError {
				t.Fatalf("stop=error for %s/%s", model, tc.name)
			}
		})
	}
}

func runOpenRouterServerToolBatteryOnNativeAPIs(t *testing.T, model string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skip live OpenRouter battery in -short mode")
	}
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	client := NewOpenRouter(key, "")
	for _, tc := range openRouterServerToolCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			switch tc.name {
			case "openrouter:shell", "openrouter:apply_patch", "openrouter:tool_search":
				postOpenRouterJSON(t, ctx, key, openrouterDefaultBaseURL+"/responses", nativeResponsesBody(model, tc))
			case "openrouter:bash":
				postOpenRouterJSON(t, ctx, key, openrouterDefaultBaseURL+"/messages", nativeMessagesBody(model, tc))
			default:
				runOpenRouterChatTool(t, ctx, client, model, tc)
			}
		})
	}
}

func runOpenRouterChatTool(t *testing.T, ctx context.Context, client Client, model string, tc openRouterServerToolCase) {
	t.Helper()
	stream, err := client.Stream(ctx, Request{
		Model:        model,
		Tools:        []Tool{tc.tool},
		MaxTokens:    256,
		MaxToolCalls: 4,
		SessionID:    "zot-live-general-" + strings.TrimPrefix(tc.name, "openrouter:"),
		Messages: []Message{{
			Role:    RoleUser,
			Content: []Content{TextBlock{Text: tc.prompt}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream(%s, %s): %v", model, tc.name, err)
	}
	var done EventDone
	for ev := range stream {
		if e, ok := ev.(EventDone); ok {
			done = e
		}
	}
	if done.Err != nil {
		t.Fatalf("EventDone.Err for %s/%s: %v", model, tc.name, done.Err)
	}
	if done.Stop == StopError {
		t.Fatalf("stop=error for %s/%s", model, tc.name)
	}
}

func nativeResponsesBody(model string, tc openRouterServerToolCase) map[string]any {
	tool := map[string]any{"type": tc.name}
	switch tc.name {
	case "openrouter:shell":
		tool["parameters"] = map[string]any{"engine": "openrouter"}
	case "openrouter:tool_search":
		return map[string]any{
			"model": model,
			"input": tc.prompt,
			"tools": []any{
				map[string]any{"type": "openrouter:tool_search"},
				map[string]any{
					"type":        "function",
					"name":        "add_numbers",
					"description": "Add two integers.",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"a": map[string]any{"type": "integer"}, "b": map[string]any{"type": "integer"}},
						"required":   []string{"a", "b"},
					},
					"defer_loading": true,
				},
			},
		}
	}
	return map[string]any{
		"model": model,
		"input": tc.prompt,
		"tools": []any{tool},
	}
}

func nativeMessagesBody(model string, tc openRouterServerToolCase) map[string]any {
	return map[string]any{
		"model":      model,
		"max_tokens": 256,
		"messages":   []any{map[string]any{"role": "user", "content": tc.prompt}},
		"tools": []any{map[string]any{
			"type":       tc.name,
			"parameters": map[string]any{"engine": "openrouter"},
		}},
	}
}

func postOpenRouterJSON(t *testing.T, ctx context.Context, key, url string, body map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s http %d: %s", url, resp.StatusCode, truncateLiveBody(raw))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v\n%s", url, err, truncateLiveBody(raw))
	}
	if errObj, ok := decoded["error"]; ok && errObj != nil {
		t.Fatalf("%s error: %v", url, errObj)
	}
}

func truncateLiveBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 400 {
		return s[:400] + fmt.Sprintf("... (%d bytes)", len(s))
	}
	return s
}
