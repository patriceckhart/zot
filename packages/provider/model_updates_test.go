package provider

import (
	"slices"
	"testing"
)

func TestGPT61TerraCatalogAndRequest(t *testing.T) {
	for _, name := range []string{"openai", "openai-responses", "openai-codex", "azure-openai-responses"} {
		t.Run(name, func(t *testing.T) {
			m, err := FindModel(name, "gpt-6.1-terra")
			if err != nil {
				t.Fatal(err)
			}
			contextWindow := 272000
			if name == "openai-codex" {
				contextWindow = 500000
			}
			if m.API != APIResponses || m.ContextWindow != contextWindow || m.MaxOutput != 128000 || !m.Reasoning || m.Speculative {
				t.Fatalf("capabilities = %+v", m)
			}
			if m.PriceInput != 1 || m.PriceOutput != 5 || m.PriceCacheRead != 0.05 || m.PriceCacheWrite != 1.25 ||
				m.PriceTierInputTokens != 272000 || m.PriceInputAbove != 2 || m.PriceOutputAbove != 7.5 || m.PriceCacheReadAbove != 0.1 || m.PriceCacheWriteAbove != 2.5 {
				t.Fatalf("pricing = %+v", m)
			}
			levels := []string{"", "low", "medium", "high", "xhigh", "max"}
			if got := AvailableReasoningLevels(m); !slices.Equal(got, levels) {
				t.Fatalf("levels = %q", got)
			}
			client := &codexClient{providerName: name}
			for _, level := range append(levels, "off", "minimum") {
				wire, err := client.buildRequest(Request{Model: m.ID, Reasoning: level})
				if err != nil {
					t.Fatal(err)
				}
				want := level
				if level == "" || level == "off" {
					want = "none"
				} else if level == "minimum" {
					want = "low"
				}
				if wire.Reasoning == nil || wire.Reasoning.Effort != want {
					t.Fatalf("%q: reasoning = %+v, want %q", level, wire.Reasoning, want)
				}
			}
		})
	}
}

func TestHaiku55AnthropicCatalogAndRequest(t *testing.T) {
	m, err := FindModel("anthropic", "claude-haiku-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
		t.Fatalf("capabilities = %+v", m)
	}
	if m.PriceInput != 0.1 || m.PriceOutput != 0.5 || m.PriceCacheRead != 0.01 || m.PriceCacheWrite != 0.125 ||
		m.PriceTierInputTokens != 100000 || m.PriceInputAbove != 0.5 || m.PriceOutputAbove != 2.5 || m.PriceCacheReadAbove != 0.05 || m.PriceCacheWriteAbove != 0.625 {
		t.Fatalf("pricing = %+v", m)
	}
	levels := []string{"", "low", "medium", "high", "xhigh", "max"}
	if got := AvailableReasoningLevels(m); !slices.Equal(got, levels) {
		t.Fatalf("levels = %q", got)
	}
	for _, client := range []*anthropicClient{
		NewAnthropic("synthetic-key", "").(*anthropicClient),
		NewAnthropicOAuth("synthetic-token", "").(*anthropicClient),
	} {
		for _, level := range append(levels, "off", "minimum") {
			temperature := float32(0.5)
			wire, err := client.buildRequest(Request{Model: m.ID, Reasoning: level, Temperature: &temperature, StreamReasoning: true})
			if err != nil {
				t.Fatal(err)
			}
			if wire.Temperature != nil || wire.MaxTokens != 128000 {
				t.Fatalf("request = %+v", wire)
			}
			if level == "" || level == "off" {
				if wire.Thinking == nil || wire.Thinking.Type != "disabled" || wire.Thinking.Display != "" || wire.OutputConfig != nil {
					t.Fatalf("off: thinking = %+v, output = %+v", wire.Thinking, wire.OutputConfig)
				}
				continue
			}
			want := level
			if level == "minimum" {
				want = "low"
			}
			if wire.Thinking == nil || wire.Thinking.Type != "adaptive" || wire.Thinking.BudgetTokens != 0 || wire.OutputConfig == nil || wire.OutputConfig.Effort != want {
				t.Fatalf("%q: thinking = %+v, output = %+v", level, wire.Thinking, wire.OutputConfig)
			}
		}
	}
}

func TestGPT56DirectPricing(t *testing.T) {
	for _, tc := range []struct {
		id                                      string
		input, output, read, write, outputAbove float64
	}{
		{"gpt-5.6-luna", 0.2, 1.2, 0.02, 0.25, 1.8},
		{"gpt-5.6-sol", 4, 20, 0.4, 5, 30},
		{"gpt-5.6-terra", 2, 12, 0.2, 2.5, 18},
	} {
		for _, name := range []string{"openai", "openai-codex", "azure-openai-responses"} {
			t.Run(name+"/"+tc.id, func(t *testing.T) {
				m, err := FindModel(name, tc.id)
				if err != nil {
					t.Fatal(err)
				}
				if m.PriceInput != tc.input || m.PriceOutput != tc.output || m.PriceCacheRead != tc.read || m.PriceCacheWrite != tc.write ||
					m.PriceTierInputTokens != 272000 || m.PriceInputAbove != tc.input*2 || m.PriceOutputAbove != tc.outputAbove || m.PriceCacheReadAbove != tc.read*2 || m.PriceCacheWriteAbove != tc.write*2 {
					t.Fatalf("pricing = %+v", m)
				}
			})
		}
	}
}
