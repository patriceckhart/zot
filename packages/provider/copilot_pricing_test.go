package provider

import (
	"math"
	"testing"
)

// Snapshot of the github-copilot cost fields from models.dev/api.json, 2026-10-19.
func TestCopilotPreviouslyUnpricedModels(t *testing.T) {
	cases := []struct {
		id                                             string
		input, output, read, write                     float64
		tier                                           int
		inputAbove, outputAbove, readAbove, writeAbove float64
	}{
		{"claude-haiku-4.5", 1, 5, 0.1, 1.25, 0, 0, 0, 0, 0},
		{"claude-opus-4.7", 5, 25, 0.5, 6.25, 0, 0, 0, 0, 0},
		{"claude-opus-4.8", 5, 25, 0.5, 6.25, 0, 0, 0, 0, 0},
		{"claude-sonnet-5", 2, 10, 0.2, 2.5, 0, 0, 0, 0, 0},
		{"gemini-3.5-flash", 1.5, 9, 0.15, 0, 0, 0, 0, 0, 0},
		{"gpt-5-mini", 0.25, 2, 0.025, 0, 0, 0, 0, 0, 0},
		{"gpt-5.3-codex", 1.75, 14, 0.175, 0, 0, 0, 0, 0, 0},
		{"gpt-5.4", 2.5, 15, 0.25, 0, 272000, 5, 22.5, 0.5, 0},
		{"gpt-5.4-mini", 0.75, 4.5, 0.075, 0, 0, 0, 0, 0, 0},
		{"gpt-5.5", 5, 30, 0.5, 0, 272000, 10, 45, 1, 0},
		{"gpt-5.6-sol", 4, 20, 0.4, 5, 272000, 8, 30, 0.8, 10},
		{"gpt-5.6-terra", 2, 12, 0.2, 2.5, 272000, 4, 18, 0.4, 5},
		{"gpt-5.6-luna", 0.2, 1.2, 0.02, 0.25, 200000, 0.4, 1.8, 0.04, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			m, err := FindModel("github-copilot", tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if m.PriceInput != tc.input || m.PriceOutput != tc.output || m.PriceCacheRead != tc.read || m.PriceCacheWrite != tc.write {
				t.Fatalf("incorrect base pricing: %+v", m)
			}
			if m.PriceTierInputTokens != tc.tier || m.PriceInputAbove != tc.inputAbove || m.PriceOutputAbove != tc.outputAbove || m.PriceCacheReadAbove != tc.readAbove || m.PriceCacheWriteAbove != tc.writeAbove {
				t.Fatalf("incorrect long-context pricing: %+v", m)
			}
			if tc.tier > 0 {
				for _, prompt := range []int{tc.tier, tc.tier + 1} {
					u := Usage{InputTokens: prompt - 2000, CacheReadTokens: 1000, CacheWriteTokens: 1000, OutputTokens: 1000}
					input, output, read, write := tc.input, tc.output, tc.read, tc.write
					if prompt > tc.tier {
						input, output, read, write = tc.inputAbove, tc.outputAbove, tc.readAbove, tc.writeAbove
					}
					want := (float64(u.InputTokens)*input + 1000*(output+read+write)) / 1e6
					if got := ComputeCost(m, u); math.Abs(got-want) > 1e-9 {
						t.Fatalf("prompt %d: cost = %g, want %g", prompt, got, want)
					}
				}
			}
		})
	}
}

func TestCopilotCatalogHasAPIEquivalentPrices(t *testing.T) {
	for _, m := range Catalog {
		if m.Provider == "github-copilot" && (m.PriceInput <= 0 || m.PriceOutput <= 0) {
			t.Errorf("%s lacks API-equivalent pricing", m.ID)
		}
	}
}

func TestCopilotSonnetUsageAddsCost(t *testing.T) {
	m, err := FindModel("github-copilot", "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	// One million tokens of each category must contribute its own price.
	for _, tc := range []struct {
		usage Usage
		want  float64
	}{
		{Usage{InputTokens: 1000000}, 2},
		{Usage{OutputTokens: 1000000}, 10},
		{Usage{CacheReadTokens: 1000000}, 0.2},
		{Usage{CacheWriteTokens: 1000000}, 2.5},
	} {
		if got := ComputeCost(m, tc.usage); got != tc.want {
			t.Errorf("usage %+v: cost = %g, want %g", tc.usage, got, tc.want)
		}
	}
}
