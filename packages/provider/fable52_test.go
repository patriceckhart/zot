package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestFable52CatalogAndThinking(t *testing.T) {
	const id = "claude-fable-5-2"
	m, err := FindModel("anthropic", id)
	if err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "Claude Fable 5.2" || m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
		t.Fatalf("unexpected capabilities: %+v", m)
	}
	if m.PriceInput != 8 || m.PriceOutput != 40 || m.PriceCacheRead != 0.2 || m.PriceCacheWrite != 10 {
		t.Fatalf("unexpected pricing: %+v", m)
	}
	if !slices.Equal(m.Input, []string{"text", "image"}) {
		t.Fatalf("input = %q", m.Input)
	}
	wantLevels := []string{"low", "medium", "high", "xhigh", "max"}
	if got := AvailableReasoningLevels(m); !slices.Equal(got, wantLevels) {
		t.Fatalf("levels = %q, want %q", got, wantLevels)
	}
	for _, client := range []*anthropicClient{
		NewAnthropic("synthetic-key", "").(*anthropicClient),
		NewAnthropicOAuth("synthetic-token", "").(*anthropicClient),
	} {
		for _, level := range []string{"", "off", "minimum", "low", "medium", "high", "xhigh", "max"} {
			effort := level
			if level == "" || level == "off" || level == "minimum" {
				effort = "low"
			}
			temperature := float32(0.5)
			wire, err := client.buildRequest(Request{
				Model: id, Reasoning: level, Temperature: &temperature,
				Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if wire.Model != id || wire.MaxTokens != 128000 || wire.Temperature != nil {
				t.Fatalf("unexpected request: %+v", wire)
			}
			if wire.Thinking == nil || wire.Thinking.Type != "adaptive" || wire.Thinking.BudgetTokens != 0 || wire.Thinking.BlockBinding == nil || wire.Thinking.BlockBinding.PrefixMismatchBehavior != "drop_block" {
				t.Fatalf("thinking = %+v", wire.Thinking)
			}
			if wire.OutputConfig == nil || wire.OutputConfig.Effort != "low" {
				t.Fatalf("top-level effort must stay fixed: %+v", wire.OutputConfig)
			}
			if len(wire.Messages) != 2 {
				t.Fatalf("messages = %+v", wire.Messages)
			}
			marker := wire.Messages[1]
			if marker.Role != "system" || marker.Content == nil || len(marker.Content) != 0 || marker.OutputConfig == nil || marker.OutputConfig.Effort != effort {
				t.Fatalf("marker for %q = %+v", level, marker)
			}
			text := wire.Messages[0].Content[0].(anthTextBlock)
			if text.CacheControl == nil {
				t.Fatal("effort marker displaced user cache breakpoint")
			}
		}
	}
}

func TestFable52ModelIDs(t *testing.T) {
	for _, id := range []string{"claude-fable-5-2", "claude-fable-5.2", "anthropic/claude-fable-5.2", "~anthropic/claude-fable-5.2", "claude-fable-5-2-20261109", "global.anthropic.claude-fable-5-2"} {
		if !isClaudeFable52(id) {
			t.Errorf("not recognized: %s", id)
		}
		m := Model{ID: id, Reasoning: true}
		if got := ClampReasoningForModel(m, "off"); got != "low" {
			t.Errorf("%s: off clamped to %q", id, got)
		}
	}
	for _, id := range []string{"claude-fable-5", "claude-fable-5-1", "claude-fable-5-20", "claude-fable-5-2-invalid", "other/claude-fable-5-2"} {
		if isClaudeFable52(id) {
			t.Errorf("unexpected match: %s", id)
		}
	}
}

func TestFable52StreamHeadersAndWire(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		t.Run(map[bool]string{false: "api-key", true: "oauth"}[oauth], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				beta := r.Header.Get("anthropic-beta")
				for _, want := range []string{"mid-conversation-output-config-2026-07-01", "thinking-binding-controls-2026-08-01"} {
					if !slices.Contains(strings.Split(beta, ","), want) {
						t.Errorf("missing beta %q: %q", want, beta)
					}
				}
				if oauth && !strings.Contains(beta, "oauth-2025-04-20") {
					t.Error("lost OAuth beta")
				}
				if oauth && r.UserAgent() != "claude-cli/2.1.290" {
					t.Errorf("Fable 5.2 requires Claude Code 2.1.290 or newer, got %q", r.UserAgent())
				}
				var wire anthRequest
				if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
					t.Error(err)
				}
				if len(wire.Messages) != 2 || wire.Messages[1].OutputConfig == nil || wire.Messages[1].OutputConfig.Effort != "max" {
					t.Errorf("unexpected wire messages: %+v", wire.Messages)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			}))
			defer server.Close()
			client := NewAnthropic("synthetic-key", server.URL)
			if oauth {
				client = NewAnthropicOAuth("synthetic-token", server.URL)
			}
			events, err := client.Stream(context.Background(), Request{
				Model: "claude-fable-5-2", Reasoning: "max",
				Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for event := range events {
				if e, ok := event.(EventDone); ok && e.Err != nil {
					t.Fatalf("stream error: %+v", e)
				}
			}
		})
	}
}
