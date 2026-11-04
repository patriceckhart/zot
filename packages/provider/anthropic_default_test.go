package provider

import (
	"reflect"
	"testing"
)

// Regression coverage for #240: defaults must not depend on legacy catalog order.
func TestAnthropicDefaultUsesSonnet55(t *testing.T) {
	if DefaultModel.Provider != "anthropic" || DefaultModel.ID != "claude-sonnet-5-5" {
		t.Fatalf("default = %s/%s", DefaultModel.Provider, DefaultModel.ID)
	}
	m, err := FindModel(DefaultModel.Provider, DefaultModel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, DefaultModel) {
		t.Fatalf("default metadata differs from catalog: %+v, %+v", DefaultModel, m)
	}
}

// Retirements in #240 apply to direct Anthropic, not partner endpoints.
func TestAnthropicCatalogOmitsRetiredModels(t *testing.T) {
	retired := map[string]bool{
		"claude-opus-4-1":            true,
		"claude-opus-4-0":            true,
		"claude-sonnet-4-0":          true,
		"claude-3-7-sonnet-20250219": true,
		"claude-3-5-sonnet-20241022": true,
		"claude-3-5-haiku-latest":    true,
		"claude-3-opus-20240229":     true,
	}
	for _, m := range Catalog {
		if m.Provider == "anthropic" && retired[m.ID] {
			t.Errorf("retired model still bundled: %s", m.ID)
		}
	}
	for _, tc := range []struct{ provider, id string }{
		{"anthropic", "claude-sonnet-4-5"},
		{"anthropic", "claude-haiku-4-5"},
		{"amazon-bedrock", "anthropic.claude-opus-4-1-20250805-v1:0"},
		{"cloudflare-ai-gateway", "claude-opus-4-1"},
	} {
		if _, err := FindModel(tc.provider, tc.id); err != nil {
			t.Errorf("unrelated entry removed: %v", err)
		}
	}
}
