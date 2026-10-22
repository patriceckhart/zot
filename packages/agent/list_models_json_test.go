package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestModelPickerCatalogFiltersByProvider(t *testing.T) {
	models := []provider.Model{
		{Provider: "openai", ID: "gpt-a"},
		{Provider: "anthropic", ID: "claude-a"},
		{Provider: "kimi", ID: "kimi-a"},
	}
	got := modelPickerCatalog(models, []string{"anthropic", "openai"})
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Provider == "kimi" {
			t.Fatalf("kimi is not a picker provider, but was listed: %+v", e)
		}
	}
}

func TestModelPickerCatalogSortsByProviderThenID(t *testing.T) {
	models := []provider.Model{
		{Provider: "openai", ID: "zeta"},
		{Provider: "anthropic", ID: "beta"},
		{Provider: "openai", ID: "alpha"},
		{Provider: "anthropic", ID: "alpha"},
	}
	got := modelPickerCatalog(models, []string{"anthropic", "openai"})
	want := []string{"anthropic/alpha", "anthropic/beta", "openai/alpha", "openai/zeta"}
	for i, e := range got {
		if e.Provider+"/"+e.ID != want[i] {
			t.Fatalf("index %d: want %s, got %s/%s", i, want[i], e.Provider, e.ID)
		}
	}
}

func TestModelPickerCatalogSourceDefaults(t *testing.T) {
	models := []provider.Model{
		{Provider: "openai", ID: "plain"},
		{Provider: "openai", ID: "user", Source: "user"},
		{Provider: "openai", ID: "guess", Speculative: true},
	}
	got := modelPickerCatalog(models, []string{"openai"})
	byID := map[string]string{}
	for _, e := range got {
		byID[e.ID] = e.Source
	}
	if byID["plain"] != "catalog" {
		t.Errorf("empty source: want catalog, got %q", byID["plain"])
	}
	if byID["user"] != "user" {
		t.Errorf("user source: want user, got %q", byID["user"])
	}
	if byID["guess"] != "speculative" {
		t.Errorf("speculative: want speculative, got %q", byID["guess"])
	}
}

func TestModelPickerCatalogEmptyMarshalsAsArray(t *testing.T) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(modelPickerCatalog(nil, nil)); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("empty catalogue: want [], got %q", buf.String())
	}
}

func TestPrintModelCatalogJSONUsesSnakeCaseKeys(t *testing.T) {
	entries := modelPickerCatalog([]provider.Model{{
		Provider: "openai", ID: "gpt-x", DisplayName: "GPT X",
		Reasoning: true, ContextWindow: 100, MaxOutput: 10,
	}}, []string{"openai"})
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, key := range []string{`"provider"`, `"id"`, `"name"`, `"reasoning"`, `"context_window"`, `"max_output"`, `"source"`} {
		if !strings.Contains(s, key) {
			t.Errorf("missing key %s in %s", key, s)
		}
	}
}
