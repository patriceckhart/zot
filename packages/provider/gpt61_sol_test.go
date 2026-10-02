package provider

import (
	"slices"
	"testing"
)

func TestGPT61SolCatalog(t *testing.T) {
	for _, name := range []string{"openai", "openai-responses", "openai-codex"} {
		t.Run(name, func(t *testing.T) {
			m, err := FindModel(name, "gpt-6.1-sol")
			if err != nil {
				t.Fatal(err)
			}
			if m.API != APIResponses || m.ContextWindow != 272000 || m.MaxOutput != 128000 || !m.Reasoning {
				t.Fatalf("model metadata: %+v", m)
			}
			if m.PriceInput != 2 || m.PriceOutput != 10 || m.PriceCacheRead != 0.1 || m.PriceCacheWrite != 2.5 ||
				m.PriceTierInputTokens != 272000 || m.PriceInputAbove != 4 || m.PriceOutputAbove != 15 ||
				m.PriceCacheReadAbove != 0.2 || m.PriceCacheWriteAbove != 5 {
				t.Fatalf("model pricing: %+v", m)
			}
			if got := AvailableReasoningLevels(m); !slices.Equal(got, []string{"", "low", "medium", "high", "xhigh", "max"}) {
				t.Fatalf("reasoning levels = %v", got)
			}
		})
	}
}

func TestGPT61SolResponsesRequest(t *testing.T) {
	clients := map[string]*codexClient{
		"openai":           NewOpenAIResponsesNamed("test-token", "", "openai").(*renamedClient).inner.(*codexClient),
		"openai-responses": NewOpenAIResponsesNamed("test-token", "", "openai-responses").(*renamedClient).inner.(*codexClient),
		"openai-codex":     NewOpenAICodex("test-token", "test-account", "").(*codexClient),
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
				wire, err := client.buildRequest(Request{Model: "gpt-6.1-sol", Reasoning: effort})
				if err != nil {
					t.Fatal(err)
				}
				if wire.Model != "gpt-6.1-sol" {
					t.Fatalf("wire model = %q", wire.Model)
				}
				if effort == "" {
					if wire.Reasoning != nil {
						t.Fatalf("unexpected reasoning config: %+v", wire.Reasoning)
					}
				} else if wire.Reasoning == nil || wire.Reasoning.Effort != effort {
					t.Fatalf("reasoning for %q = %+v", effort, wire.Reasoning)
				}
			}
		})
	}
}
