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

func TestCopilotCatalogDefersUnverifiedPreviews(t *testing.T) {
	// Do not bundle preview entries with guessed limits or unknown pricing.
	// Revisit these IDs once their Copilot metadata can be verified.
	for _, id := range []string{"mai-code-2-flash", "mai-code-2.x1"} {
		for _, m := range Catalog {
			if m.Provider == "github-copilot" && m.ID == id {
				t.Errorf("unverified preview %q is bundled in the catalog", id)
			}
		}
	}
}
