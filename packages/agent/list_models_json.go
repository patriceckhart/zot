package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// catalogEntry is one row of the --list-models-json output. It describes
// a model the /model picker would offer, for external tools that cannot
// call the interactive picker directly.
type catalogEntry struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	Reasoning     bool   `json:"reasoning"`
	ContextWindow int    `json:"context_window"`
	MaxOutput     int    `json:"max_output"`
	Source        string `json:"source"`
}

// modelPickerCatalog keeps the models whose provider is in providers,
// sorted by provider then model id.
func modelPickerCatalog(models []provider.Model, providers []string) []catalogEntry {
	out := []catalogEntry{}
	for _, m := range models {
		if !slices.Contains(providers, m.Provider) {
			continue
		}
		source := m.Source
		if source == "" {
			source = "catalog"
		}
		if m.Speculative {
			source = "speculative"
		}
		out = append(out, catalogEntry{
			Provider: m.Provider, ID: m.ID, Name: m.DisplayName,
			Reasoning: m.Reasoning, ContextWindow: m.ContextWindow,
			MaxOutput: m.MaxOutput, Source: source,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// printModelCatalogJSON writes the picker-visible catalog as a
// two-space-indented JSON array. An empty catalog prints [].
func printModelCatalogJSON(w io.Writer) error {
	return printModelCatalogJSONContext(context.Background(), w, os.Stderr)
}

func printModelCatalogJSONContext(ctx context.Context, w, diagnostics io.Writer) error {
	// These catalogs are transient. Unlike the interactive picker, a
	// short-lived CLI cannot rely on startup's background refresh finishing.
	// Never execute credential commands just to enumerate models.
	for _, refresh := range []struct {
		name     string
		provider string
		fn       func(context.Context, apiKeyCommandMode) error
	}{
		{"LM Studio", provider.LMStudioProviderID, refreshLMStudioModels},
		{"llama.cpp", provider.LlamaCPPProviderID, refreshLlamaCPPModels},
		{"custom providers", "", refreshCustomProviderModels},
	} {
		if refresh.provider != "" && CheckProviderEnabled(refresh.provider) != nil {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := refresh.fn(refreshCtx, apiKeyCommandSkip)
		cancel()
		if err != nil {
			fmt.Fprintf(diagnostics, "zot: %s model discovery: %v\n", refresh.name, err)
		}
	}
	// Discovery failures keep any previously loaded entries. Diagnostics
	// must stay separate from the machine-readable JSON on stdout.
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(modelPickerCatalog(visibleModelCatalog(), modelPickerProviders()))
}
