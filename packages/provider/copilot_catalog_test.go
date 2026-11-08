package provider

import (
	"context"
	"testing"
)

func TestCopilotCatalogCoversPublicChatModels(t *testing.T) {
	groups := map[string][]string{
		APIAnthropicMessages: {"claude-fable-5", "claude-fable-5.1", "claude-haiku-4.5", "claude-opus-4.7", "claude-opus-4.8", "claude-opus-4.8-fast", "claude-opus-5", "claude-opus-5.5", "claude-sonnet-5"},
		APIResponses:         {"gpt-5-mini", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra", "gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.1-terra", "grok-4.5", "grok-4.6", "mai-code-1-flash-picker", "mai-code-1.1-flash", "mai-code-2-flash", "mai-code-2.x1"},
		APICompletions:       {"gemini-3.5-flash", "gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.8-flash", "gemini-4-argon", "kimi-k2.7-code", "kimi-k3"},
	}
	for api, ids := range groups {
		for _, id := range ids {
			t.Run(id, func(t *testing.T) {
				m, err := FindModel("github-copilot", id)
				if err != nil {
					t.Fatal(err)
				}
				if m.API != api {
					t.Fatalf("API = %q, want %q", m.API, api)
				}
				if m.ContextWindow <= 0 || m.MaxOutput <= 0 || m.MaxOutput >= m.ContextWindow {
					t.Fatalf("invalid context/output limits: %d/%d", m.ContextWindow, m.MaxOutput)
				}
				if m.BaseURL != copilotDefaultBaseURL || !m.Reasoning {
					t.Fatal("invalid endpoint or reasoning metadata")
				}
				client := NewGithubCopilotClient("synthetic-token").(*copilotClient)
				captures := make(map[string]*routeCaptureClient)
				for _, key := range []string{APIAnthropicMessages, APIResponses, APICompletions} {
					capture := &routeCaptureClient{name: "github-copilot"}
					captures[key] = capture
					client.router.byAPI[key] = capture
				}
				stream, err := client.Stream(context.Background(), Request{Model: id})
				if err != nil {
					t.Fatal(err)
				}
				for range stream {
				}
				for key, capture := range captures {
					want := 0
					if key == api {
						want = 1
					}
					if len(capture.models) != want {
						t.Errorf("%s routed to %s incorrectly", id, key)
					}
				}
			})
		}
	}
}

func TestCopilotCatalogExcludesRetiredAndUtilityModels(t *testing.T) {
	// https://docs.github.com/en/copilot/reference/ai-models/supported-models
	// Sonnet 4.6 is excluded from the general catalog despite its annual-plan exception.
	excluded := map[string]bool{
		"claude-opus-4.5": true, "claude-opus-4.6": true,
		"claude-sonnet-4.5": true, "claude-sonnet-4.6": true,
		"gemini-2.5-pro": true, "gemini-3-flash-preview": true, "gemini-3.1-pro-preview": true,
		"gpt-4.1": true, "gpt-4o": true, "gpt-5.2": true, "gpt-5.2-codex": true,
		"grok-code-fast-1": true, "gpt-5.4-nano": true,
	}
	for _, m := range Catalog {
		if m.Provider == "github-copilot" && excluded[m.ID] {
			t.Errorf("Copilot catalog still includes %q", m.ID)
		}
	}
}
