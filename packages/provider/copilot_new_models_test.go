package provider

import (
	"slices"
	"testing"
)

func TestCopilotArgonMetadata(t *testing.T) {
	m, err := FindModel("github-copilot", "gemini-4-argon")
	if err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "Gemini 4 Argon" || m.ContextWindow != 1048576 || m.MaxOutput != 65536 {
		t.Fatalf("metadata = %+v", m)
	}
	if m.PriceInput != 2 || m.PriceOutput != 10 || m.PriceCacheRead != 0.1 {
		t.Fatalf("pricing = %+v", m)
	}
	// Copilot manages Gemini thinking, so no effort selector is exposed.
	if got := AvailableReasoningLevels(m); !slices.Equal(got, []string{""}) {
		t.Fatalf("reasoning levels = %q", got)
	}
}

func TestCopilotMAI2Reasoning(t *testing.T) {
	for _, id := range []string{"mai-code-2-flash", "mai-code-2.x1"} {
		t.Run(id, func(t *testing.T) {
			m, err := FindModel("github-copilot", id)
			if err != nil {
				t.Fatal(err)
			}
			if m.DisplayName != map[string]string{
				"mai-code-2-flash": "MAI-Code-2-Flash (Preview)",
				"mai-code-2.x1":    "MAI-Code-2.x1 (Preview)",
			}[id] {
				t.Fatalf("display name = %q", m.DisplayName)
			}
			if got := AvailableReasoningLevels(m); !slices.Equal(got, []string{"", "low", "medium", "high"}) {
				t.Fatalf("reasoning levels = %q", got)
			}
			client := NewGithubCopilotClient("synthetic-token").(*copilotClient).router.byAPI[APIResponses].(*codexClient)
			for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
				wire, err := client.buildRequest(Request{Model: id, Reasoning: level})
				if err != nil {
					t.Fatal(err)
				}
				want := level
				if level == "xhigh" || level == "max" {
					want = "high"
				}
				if wire.Reasoning == nil || wire.Reasoning.Effort != want {
					t.Fatalf("%s: reasoning = %+v, want %s", level, wire.Reasoning, want)
				}
			}
		})
	}
}
