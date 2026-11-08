package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func seedCopilotTestToken(t *testing.T, baseURL string) string {
	t.Helper()
	pat := "synthetic-" + t.Name()
	copilotCache.mu.Lock()
	copilotCache.tokens[pat] = copilotToken{value: "synthetic-session-token", baseURL: baseURL, expiresAt: time.Now().Add(time.Hour)}
	copilotCache.mu.Unlock()
	t.Cleanup(func() {
		copilotCache.mu.Lock()
		delete(copilotCache.tokens, pat)
		copilotCache.mu.Unlock()
	})
	return pat
}

func TestCopilotWireProtocols(t *testing.T) {
	for _, tc := range []struct{ model, path string }{
		{"claude-sonnet-5", "/v1/messages"},
		{"claude-opus-5.5", "/v1/messages"},
		{"claude-haiku-4.5", "/v1/messages"},
		{"gpt-5.4", "/responses"},
		{"grok-4.6", "/responses"},
		{"mai-code-1.1-flash", "/responses"},
		{"mai-code-2-flash", "/responses"},
		{"mai-code-2.x1", "/responses"},
		{"gpt-6.1-terra", "/responses"},
		{"gemini-3.8-flash", "/chat/completions"},
		{"gemini-4-argon", "/chat/completions"},
		{"kimi-k3", "/chat/completions"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.path)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-session-token" {
					t.Error("missing session Bearer auth")
				}
				if r.Header.Get("Copilot-Integration-Id") != "vscode-chat" || r.Header.Get("X-Initiator") != "user" || r.Header.Get("Openai-Intent") != "conversation-edits" {
					t.Error("missing Copilot identity headers")
				}
				for _, header := range []string{"x-api-key", "anthropic-beta", "x-app", "chatgpt-account-id", "openai-beta", "originator"} {
					if r.Header.Get(header) != "" {
						t.Errorf("unexpected identity header %s", header)
					}
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if string(body["model"]) != fmt.Sprintf("%q", tc.model) {
					t.Error("model ID changed on wire")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch tc.path {
				case "/v1/messages":
					if len(body["messages"]) == 0 || len(body["max_tokens"]) == 0 {
						t.Error("missing Messages fields")
					}
					if strings.Contains(string(body["system"]), "Claude Code") {
						t.Error("injected Claude Code system identity")
					}
					if tc.model == "claude-sonnet-5" || tc.model == "claude-opus-5.5" {
						if string(body["thinking"]) != `{"type":"adaptive"}` {
							t.Errorf("thinking = %s", body["thinking"])
						}
						if len(body["output_config"]) == 0 || len(body["temperature"]) != 0 {
							t.Error("invalid adaptive thinking payload")
						}
					}
					fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"id\":\"test\",\"usage\":{\"input_tokens\":1}}}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {}\n\n")
				case "/responses":
					if len(body["input"]) == 0 || len(body["reasoning"]) == 0 {
						t.Error("missing Responses fields")
					}
					if strings.HasPrefix(tc.model, "mai-code-2") && string(body["reasoning"]) != `{"effort":"high"}` {
						t.Errorf("MAI reasoning = %s, want high effort", body["reasoning"])
					}
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				default:
					if len(body["reasoning_effort"]) != 0 {
						t.Error("unsupported reasoning_effort sent")
					}
					if len(body["messages"]) == 0 {
						t.Error("missing chat messages")
					}
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer srv.Close()
			client := NewGithubCopilotClient(seedCopilotTestToken(t, srv.URL))
			stream, err := client.Stream(context.Background(), Request{Model: tc.model, Reasoning: "high", System: "Test system", Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "Hello"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			for event := range stream {
				if e, ok := event.(EventDone); ok && e.Err != nil {
					t.Fatalf("stream error: %v", e.Err)
				}
				if e, ok := event.(EventStart); ok && e.Provider != "github-copilot" {
					t.Errorf("event provider = %s", e.Provider)
				}
			}
		})
	}
}

type copilotMetadataCapture struct {
	metadata  copilotRequestMetadata
	reasoning string
}

func (c *copilotMetadataCapture) Name() string { return "github-copilot" }
func (c *copilotMetadataCapture) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	c.metadata = ctx.Value(copilotRequestKey{}).(copilotRequestMetadata)
	c.reasoning = req.Reasoning
	ch := make(chan Event)
	close(ch)
	return ch, nil
}

func TestCopilotDynamicRequestMetadata(t *testing.T) {
	client := NewGithubCopilotClient("unused").(*copilotClient)
	capture := &copilotMetadataCapture{}
	client.router.byAPI[APICompletions] = capture
	for _, tc := range []struct {
		messages  []Message
		initiator string
		vision    bool
	}{
		{nil, "user", false},
		{[]Message{{Role: RoleUser, Content: []Content{ImageBlock{}}}}, "user", true},
		{[]Message{{Role: RoleTool, Content: []Content{ToolResultBlock{Content: []Content{ImageBlock{}}}}}}, "agent", true},
		{[]Message{{Role: RoleAssistant}}, "agent", false},
		{[]Message{{Role: RoleUser}}, "user", false},
	} {
		_, err := client.Stream(context.Background(), Request{Model: "kimi-k3", Reasoning: "high", Messages: tc.messages})
		if err != nil {
			t.Fatal(err)
		}
		if capture.metadata.initiator != tc.initiator || capture.metadata.vision != tc.vision || capture.reasoning != "" {
			t.Errorf("unexpected metadata: %+v, reasoning %q", capture.metadata, capture.reasoning)
		}
	}
	// The transport must propagate vision metadata and remove it on the next text-only request.
	mock := &mockRoundTripper{}
	transport := &copilotRefreshTransport{inner: mock, pat: seedCopilotTestToken(t, copilotDefaultBaseURL)}
	for _, vision := range []bool{true, false} {
		ctx := context.WithValue(context.Background(), copilotRequestKey{}, copilotRequestMetadata{initiator: "agent", vision: vision})
		req, _ := http.NewRequestWithContext(ctx, "POST", copilotDefaultBaseURL+"/responses", nil)
		req.Header.Set("Copilot-Vision-Request", "stale")
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := ""
		if vision {
			want = "true"
		}
		if mock.lastReq.Header.Get("Copilot-Vision-Request") != want || mock.lastReq.Header.Get("X-Initiator") != "agent" {
			t.Error("incorrect dynamic transport headers")
		}
		if req.Header.Get("Copilot-Vision-Request") != "stale" {
			t.Error("transport mutated caller headers")
		}
	}
}
