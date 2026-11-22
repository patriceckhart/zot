package provider

import (
	"context"
	"math"
	"slices"
	"testing"
)

func TestXAIGrok46And47Catalog(t *testing.T) {
	for _, id := range []string{"grok-4.6", "grok-4.7"} {
		t.Run(id, func(t *testing.T) {
			m, err := FindModel("xai", id)
			if err != nil {
				t.Fatal(err)
			}
			if m.API != APIResponses || m.BaseURL != "https://api.x.ai/v1" || m.ContextWindow != 500000 || !m.Reasoning || m.Speculative {
				t.Fatalf("capabilities = %+v", m)
			}
			if m.MaxOutput != 0 {
				t.Fatalf("unpublished output limit must stay unset, got %d", m.MaxOutput)
			}
			if !slices.Equal(m.Input, []string{"text", "image"}) {
				t.Fatalf("input modalities = %q", m.Input)
			}
			if m.PriceInput != 2 || m.PriceOutput != 6 || m.PriceCacheRead != 0.5 || m.PriceCacheWrite != 0 ||
				m.PriceTierInputTokens != 199999 || m.PriceInputAbove != 4 || m.PriceOutputAbove != 12 || m.PriceCacheReadAbove != 1 {
				t.Fatalf("pricing = %+v", m)
			}
			// xAI charges the higher rate starting at exactly 200,000 prompt
			// tokens, including cached input, not just above that boundary.
			for _, prompt := range []int{199999, 200000, 200001} {
				usage := Usage{InputTokens: prompt - 100000, CacheReadTokens: 100000, OutputTokens: 1000}
				want := (float64(usage.InputTokens)*2 + 100000*0.5 + 1000*6) / 1e6
				if prompt >= 200000 {
					want *= 2
				}
				if got := ComputeCost(m, usage); math.Abs(got-want) > 1e-12 {
					t.Fatalf("prompt %d: cost=%v, want %v", prompt, got, want)
				}
			}
		})
	}
}

func TestXAIGrok46And47ResponsesRouting(t *testing.T) {
	completions := &routeCaptureClient{name: "xai"}
	responses := &routeCaptureClient{name: "xai"}
	router := NewModelRouter("xai", completions, map[string]Client{APIResponses: responses})
	client := NewOpenAIResponsesNamed("synthetic-token", "https://api.x.ai/v1", "xai").(*renamedClient).inner.(*codexClient)
	for _, id := range []string{"grok-4.6", "grok-4.7"} {
		stream, err := router.Stream(context.Background(), Request{Model: id})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
		for _, effort := range []string{"", "low", "medium", "high", "xhigh"} {
			wire, err := client.buildRequest(Request{Model: id, Reasoning: effort})
			if err != nil {
				t.Fatal(err)
			}
			if wire.Model != id || wire.Store || !slices.Contains(wire.Include, "reasoning.encrypted_content") {
				t.Fatalf("%s: request = %+v", id, wire)
			}
			if effort == "" {
				if wire.Reasoning != nil {
					t.Fatalf("unset effort should use xAI's default: %+v", wire.Reasoning)
				}
			} else if wire.Reasoning == nil || wire.Reasoning.Effort != effort {
				t.Fatalf("%s/%s: reasoning = %+v", id, effort, wire.Reasoning)
			}
		}
	}
	if len(completions.models) != 0 || !slices.Equal(responses.models, []string{"grok-4.6", "grok-4.7"}) {
		t.Fatalf("Completions=%v, Responses=%v", completions.models, responses.models)
	}
}
