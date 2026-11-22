package provider

import (
	"context"
	"slices"
	"testing"
)

func TestGPT62CatalogAndReasoning(t *testing.T) {
	for _, tc := range []struct {
		id, name                   string
		input, output, read, write float64
	}{
		{"gpt-6.2-sol", "GPT-6.2 Sol", 2, 10, 0.1, 2.5},
		{"gpt-6.2-astra", "GPT-6.2 Astra", 8, 40, 0.4, 10},
	} {
		for _, name := range []string{"openai", "openai-responses", "openai-codex", "azure-openai-responses"} {
			t.Run(name+"/"+tc.id, func(t *testing.T) {
				m, err := FindModel(name, tc.id)
				if err != nil {
					t.Fatal(err)
				}
				wantContext := 272000
				if name == "openai-codex" && tc.id == "gpt-6.2-sol" {
					wantContext = 500000
				} else if name == "azure-openai-responses" {
					wantContext = 1050000
				}
				if m.DisplayName != tc.name || m.API != APIResponses || m.ContextWindow != wantContext || m.MaxOutput != 128000 || !m.Reasoning || m.Speculative || !slices.Equal(m.Input, []string{"text", "image"}) {
					t.Fatalf("capabilities = %+v", m)
				}
				if m.PriceInput != tc.input || m.PriceOutput != tc.output || m.PriceCacheRead != tc.read || m.PriceCacheWrite != tc.write || m.PriceTierInputTokens != 272000 || m.PriceInputAbove != tc.input*2 || m.PriceOutputAbove != tc.output*1.5 || m.PriceCacheReadAbove != tc.read*2 || m.PriceCacheWriteAbove != tc.write*2 {
					t.Fatalf("pricing = %+v", m)
				}
				levels := []string{"low", "medium", "high", "xhigh", "max"}
				if got := AvailableReasoningLevels(m); !slices.Equal(got, levels) {
					t.Fatalf("reasoning levels = %v, want %v", got, levels)
				}
				client := &codexClient{providerName: name}
				for _, level := range append(levels, "", "off", "minimum") {
					want := level
					if level == "" || level == "off" || level == "minimum" {
						want = "low"
					}
					if got := ClampReasoningForModel(m, level); got != want {
						t.Fatalf("clamp %q = %q, want %q", level, got, want)
					}
					wire, err := client.buildRequest(Request{Model: m.ID, Reasoning: level})
					if err != nil {
						t.Fatal(err)
					}
					if wire.Model != m.ID || wire.Reasoning == nil || wire.Reasoning.Effort != want {
						t.Fatalf("%q: request = %+v", level, wire)
					}
				}
			})
		}
	}
}

func TestGPT62OpenAIRouting(t *testing.T) {
	router := NewModelRouter("openai", &routeCaptureClient{name: "openai"}, map[string]Client{APIResponses: &routeCaptureClient{name: "openai"}}).(*modelRouter)
	for _, id := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		stream, err := router.Stream(context.Background(), Request{Model: id})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}
	responses := router.byAPI[APIResponses].(*routeCaptureClient)
	if !slices.Equal(responses.models, []string{"gpt-6.2-sol", "gpt-6.2-astra"}) || len(router.fallback.(*routeCaptureClient).models) != 0 {
		t.Fatalf("Responses models = %v, fallback models = %v", responses.models, router.fallback.(*routeCaptureClient).models)
	}
}
