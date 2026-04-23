package provider

import (
	"encoding/json"
	"os"
)

// UserModelsFile is the JSON format for user-defined models.
// Place a models.json in $ZOT_HOME to add models that aren't in the
// baked-in catalog or to override catalog entries.
//
// Example:
//
//	{
//	  "providers": {
//	    "openai": {
//	      "models": [
//	        {
//	          "id": "gpt-5.5",
//	          "name": "GPT-5.5",
//	          "reasoning": true,
//	          "contextWindow": 400000,
//	          "maxTokens": 128000,
//	          "priceInput": 2.50,
//	          "priceOutput": 15.00,
//	          "priceCacheRead": 0.25
//	        }
//	      ]
//	    }
//	  }
//	}
type UserModelsFile struct {
	Providers map[string]UserProvider `json:"providers"`
}

// UserProvider groups models under a provider key.
type UserProvider struct {
	Models []UserModel `json:"models"`
}

// UserModel is a single model entry in the user's models.json.
type UserModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Reasoning       bool     `json:"reasoning"`
	ContextWindow   int      `json:"contextWindow"`
	MaxTokens       int      `json:"maxTokens"`
	PriceInput      float64  `json:"priceInput"`
	PriceOutput     float64  `json:"priceOutput"`
	PriceCacheRead  float64  `json:"priceCacheRead"`
	PriceCacheWrite float64  `json:"priceCacheWrite"`
	Input           []string `json:"input"` // informational only
	API             string   `json:"api"`   // informational only
}

// LoadUserModels reads a models.json file and returns the models
// converted to the internal Model type. Returns nil on any error
// (missing file, bad JSON, etc.) so the caller can treat it as
// optional without error handling.
func LoadUserModels(path string) []Model {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file UserModelsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}

	var out []Model
	for providerName, prov := range file.Providers {
		// Normalize provider name: strip suffixes like "-codex" so
		// "openai-codex" maps to "openai".
		normalized := providerName
		switch providerName {
		case "openai-codex", "openai-responses":
			normalized = "openai"
		case "anthropic-messages":
			normalized = "anthropic"
		}

		for _, um := range prov.Models {
			m := Model{
				Provider:        normalized,
				ID:              um.ID,
				DisplayName:     um.Name,
				ContextWindow:   um.ContextWindow,
				MaxOutput:       um.MaxTokens,
				Reasoning:       um.Reasoning,
				PriceInput:      um.PriceInput,
				PriceOutput:     um.PriceOutput,
				PriceCacheRead:  um.PriceCacheRead,
				PriceCacheWrite: um.PriceCacheWrite,
				Source:          "user",
			}
			if m.DisplayName == "" {
				m.DisplayName = m.ID
			}
			out = append(out, m)
		}
	}
	return out
}

// SetUserModels merges user-defined models into the active catalog.
// User models take precedence over both the baked-in catalog and
// live-discovered models.
func SetUserModels(models []Model) {
	if len(models) == 0 {
		return
	}
	activeMu.Lock()
	defer activeMu.Unlock()

	// Build index of current active models.
	byKey := func(p, id string) string { return p + "/" + id }
	index := make(map[string]int, len(active))
	for i, m := range active {
		index[byKey(m.Provider, m.ID)] = i
	}

	for _, um := range models {
		k := byKey(um.Provider, um.ID)
		if idx, ok := index[k]; ok {
			// Override existing entry but keep prices from user if
			// they provided them, otherwise keep catalog prices.
			existing := active[idx]
			if um.PriceInput > 0 {
				existing.PriceInput = um.PriceInput
			}
			if um.PriceOutput > 0 {
				existing.PriceOutput = um.PriceOutput
			}
			if um.PriceCacheRead > 0 {
				existing.PriceCacheRead = um.PriceCacheRead
			}
			if um.PriceCacheWrite > 0 {
				existing.PriceCacheWrite = um.PriceCacheWrite
			}
			if um.DisplayName != "" {
				existing.DisplayName = um.DisplayName
			}
			if um.ContextWindow > 0 {
				existing.ContextWindow = um.ContextWindow
			}
			if um.MaxOutput > 0 {
				existing.MaxOutput = um.MaxOutput
			}
			existing.Reasoning = um.Reasoning
			existing.Source = "user"
			existing.Speculative = false
			active[idx] = existing
		} else {
			// New model not in catalog.
			active = append(active, um)
		}
	}
}
