package agent

import (
	"encoding/json"
	"io"
	"slices"
	"sort"

	"github.com/patriceckhart/zot/packages/provider"
)

// catalogEntry is one row of the --list-models-json output. It describes
// a model the /models picker would offer, for external tools that cannot
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
// sorted by provider then model id. It matches the set the /models
// picker shows for the same provider list.
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
			Provider:      m.Provider,
			ID:            m.ID,
			Name:          m.DisplayName,
			Reasoning:     m.Reasoning,
			ContextWindow: m.ContextWindow,
			MaxOutput:     m.MaxOutput,
			Source:        source,
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

// printModelCatalogJSON writes the /models-visible catalogue as a
// two-space-indented JSON array. An empty catalogue prints [].
func printModelCatalogJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(modelPickerCatalog(provider.Active(), modelPickerProviders()))
}
