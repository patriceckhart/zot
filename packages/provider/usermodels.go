package provider

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
)

// UserModelsFile is the JSON format for user-defined models.
// Place a models.json in $ZOT_HOME to add models that aren't in the
// baked-in catalog or to override catalog entries. Custom providers
// (not in the built-in set) may specify a baseUrl and api format at
// the provider level:
//
//	{
//	  "providers": {
//	    "my-company": {
//	      "baseUrl": "https://llm.mycompany.com/v1",
//	      "api": "openai",
//	      "models": [
//	        {
//	          "id": "company-llm-v2",
//	          "name": "Company LLM v2",
//	          "contextWindow": 128000,
//	          "maxTokens": 32000
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
	BaseURL string `json:"baseUrl,omitempty"`
	API     string `json:"api,omitempty"`  // "openai" (default), "openai-responses", or "anthropic"
	Auth    string `json:"auth,omitempty"` // "none" disables credentials, including stored keys
	// Discover enables live model listing from the provider-level
	// baseUrl's /models endpoint. Off by default so existing custom
	// providers never gain unexpected network traffic.
	Discover bool        `json:"discover,omitempty"`
	Models   []UserModel `json:"models"`
}

// CustomProviderConfig holds runtime config for a user-defined provider
// that isn't part of the built-in catalog.
type CustomProviderConfig struct {
	BaseURL string
	API     string // "openai", "openai-responses", or "anthropic"
	NoAuth  bool   // explicitly configured for keyless access
	// Discover reports whether live model discovery is enabled. It is
	// only true when a provider-level BaseURL is present.
	Discover bool
}

var customProviders = map[string]CustomProviderConfig{}

// CustomProviders returns the set of user-defined providers loaded from
// models.json. Keys are provider names; values carry the base URL and
// wire-format hint.
func CustomProviders() map[string]CustomProviderConfig { return customProviders }

// UserModel is a single model entry in the user's models.json.
type UserModel struct {
	ID                string            `json:"id"`
	Type              string            `json:"type,omitempty"` // chat (default), classifier, or image
	Output            []string          `json:"output,omitempty"`
	Name              string            `json:"name"`
	Reasoning         bool              `json:"reasoning"`
	ReasoningLevelMap map[string]string `json:"reasoningLevelMap,omitempty"`
	ContextWindow     int               `json:"contextWindow"`
	MaxTokens         int               `json:"maxTokens"`
	PriceInput        float64           `json:"priceInput"`
	PriceOutput       float64           `json:"priceOutput"`
	PriceCacheRead    float64           `json:"priceCacheRead"`
	PriceCacheWrite   float64           `json:"priceCacheWrite"`
	BaseURL           string            `json:"baseUrl,omitempty"`
	Input             []string          `json:"input"` // informational only
	API               string            `json:"api"`   // informational only
	Compat            struct {
		SupportsOpenAIGrammarTools *bool `json:"supportsOpenAIGrammarTools,omitempty"`
	} `json:"compat,omitempty"`
}

func normalizeReasoningLevelMap(levelMap map[string]string, modelRef string) (map[string]string, []string) {
	if levelMap == nil {
		return nil, nil
	}
	normalized := make(map[string]string, len(levelMap))
	var warnings []string
	for rawLevel, rawTarget := range levelMap {
		level := NormalizeReasoning(rawLevel)
		if reasoningLevelRank(level) == 0 {
			warnings = append(warnings, fmt.Sprintf("models.json: %s reasoningLevelMap has unknown level %q; entry ignored", modelRef, rawLevel))
			continue
		}
		target := NormalizeReasoning(rawTarget)
		if rawTarget != "" && target == "" {
			// Explicit off aliases remove the level just like an empty value.
			normalized[level] = ""
			continue
		}
		if target != "" && reasoningLevelRank(target) == 0 {
			warnings = append(warnings, fmt.Sprintf("models.json: %s reasoningLevelMap has unknown target %q; entry ignored", modelRef, rawTarget))
			continue
		}
		normalized[level] = target
	}
	return normalized, warnings
}

// LoadUserModels reads a models.json file and returns the models
// converted to the internal Model type. Returns nil on any error
// (missing file, bad JSON, etc.) so the caller can treat it as
// optional without error handling.
func LoadUserModels(path string) []Model {
	models, _ := LoadUserModelsWithWarnings(path)
	return models
}

// LoadUserModelsWithWarnings is like LoadUserModels but also returns
// human-readable warnings about every recoverable issue it found in
// the file (unknown provider id, empty model id, malformed JSON for a
// single provider block, etc.). The caller is responsible for
// surfacing the warnings; the file is never rejected wholesale unless
// the top-level JSON itself fails to parse.
func LoadUserModelsWithWarnings(path string) ([]Model, []string) {
	setNonChatModels(nil)
	var warnings []string
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var file UserModelsFile
	if err := json.Unmarshal(data, &file); err != nil {
		warnings = append(warnings, fmt.Sprintf("models.json: parse error: %v (file ignored)", err))
		return nil, warnings
	}

	var out []Model
	var nonChat []NonChatModel
	// Reset custom providers on each load so removed entries don't linger.
	customProviders = map[string]CustomProviderConfig{}
	for providerName, prov := range file.Providers {
		if providerName == "" {
			warnings = append(warnings, "models.json: empty provider key skipped")
			continue
		}
		// Normalize legacy transport aliases to their provider names.
		normalized := providerName
		switch providerName {
		case "openai-responses":
			normalized = "openai"
		case "anthropic-messages":
			normalized = "anthropic"
		case "moonshot", "moonshot-ai", "kimi-code":
			normalized = "kimi"
		case "deepseek-chat", "deepseek-ai":
			normalized = "deepseek"
		}

		// Register custom providers that carry endpoint metadata either at
		// the provider level or on any model. Model-level baseUrl-only
		// configs still need a custom provider entry so Resolve/NewClient
		// accept the provider name and choose a wire format.
		hasModelBaseURL := false
		for _, um := range prov.Models {
			if um.BaseURL != "" {
				hasModelBaseURL = true
				break
			}
		}
		// Discovery needs one endpoint to list from, so it is tied to the
		// provider-level baseUrl rather than per-model overrides.
		discover := prov.Discover
		if discover && strings.TrimSpace(prov.BaseURL) == "" {
			warnings = append(warnings, fmt.Sprintf("models.json: provider %q sets discover without a provider-level baseUrl; discovery disabled", providerName))
			discover = false
		}
		if prov.Auth != "" && prov.BaseURL == "" && !hasModelBaseURL {
			warnings = append(warnings, fmt.Sprintf("models.json: provider %q sets auth without a baseUrl; ignored", providerName))
		}
		if prov.BaseURL != "" || prov.API != "" || hasModelBaseURL {
			auth := strings.ToLower(strings.TrimSpace(prov.Auth))
			if auth != "" && auth != "none" {
				warnings = append(warnings, fmt.Sprintf("models.json: provider %q has unknown auth %q; using credentials when available", providerName, prov.Auth))
			}
			if prov.BaseURL == "" && !hasModelBaseURL {
				auth = ""
			}
			api := strings.ToLower(strings.TrimSpace(prov.API))
			if api == "" {
				api = "openai"
			}
			// Normalize common aliases for the wire format.
			switch api {
			case "openai-completions", "openai-chat", "chat", "openai":
				api = "openai"
			case "openai-responses", "responses":
				api = APIResponses
			case "anthropic-messages", "messages", "anthropic":
				api = "anthropic"
			default:
				warnings = append(warnings, fmt.Sprintf("models.json: provider %q has unknown api %q; defaulting to openai", providerName, prov.API))
				api = "openai"
			}
			customProviders[normalized] = CustomProviderConfig{
				BaseURL:  prov.BaseURL,
				API:      api,
				NoAuth:   auth == "none",
				Discover: discover,
			}
		}

		for i, um := range prov.Models {
			if um.ID == "" {
				warnings = append(warnings, fmt.Sprintf("models.json: provider %q entry #%d has empty id; skipped", providerName, i))
				continue
			}
			if um.ContextWindow < 0 || um.MaxTokens < 0 {
				warnings = append(warnings, fmt.Sprintf("models.json: %s/%s has negative contextWindow/maxTokens; clamped to 0", normalized, um.ID))
				if um.ContextWindow < 0 {
					um.ContextWindow = 0
				}
				if um.MaxTokens < 0 {
					um.MaxTokens = 0
				}
			}
			levelMap, levelWarnings := normalizeReasoningLevelMap(um.ReasoningLevelMap, normalized+"/"+um.ID)
			warnings = append(warnings, levelWarnings...)
			if !um.Reasoning && len(levelMap) > 0 {
				warnings = append(warnings, fmt.Sprintf("models.json: %s reasoningLevelMap ignored because reasoning is false", normalized+"/"+um.ID))
				levelMap = nil
			}

			// Propagate provider-level BaseURL to models without their own.
			modelBaseURL := um.BaseURL
			if modelBaseURL == "" {
				modelBaseURL = prov.BaseURL
			}
			modelAPI := ""
			if cfg, ok := customProviders[normalized]; ok {
				modelAPI = cfg.API
			}
			m := Model{
				Provider:                   normalized,
				ID:                         um.ID,
				DisplayName:                um.Name,
				API:                        modelAPI,
				Input:                      append([]string(nil), um.Input...),
				ContextWindow:              um.ContextWindow,
				MaxOutput:                  um.MaxTokens,
				Reasoning:                  um.Reasoning,
				ReasoningLevelMap:          levelMap,
				SupportsOpenAIGrammarTools: um.Compat.SupportsOpenAIGrammarTools,
				PriceInput:                 um.PriceInput,
				PriceOutput:                um.PriceOutput,
				PriceCacheRead:             um.PriceCacheRead,
				PriceCacheWrite:            um.PriceCacheWrite,
				BaseURL:                    modelBaseURL,
				Source:                     "user",
			}
			if m.DisplayName == "" {
				m.DisplayName = m.ID
			}
			if um.Type == "image" || um.Type == "classifier" {
				m.API = um.API
				if m.API == "" {
					m.API = prov.API
				}
				if um.Type == "image" && m.API != "openrouter-images" || um.Type == "classifier" && m.API != "typesafe-system-one" && m.API != "llama-cpp-classify" && m.API != "cloudflare-workers-ai-system-one" {
					warnings = append(warnings, fmt.Sprintf("models.json: %s/%s has an unsupported non-chat api, skipped", normalized, um.ID))
					continue
				}
				input := um.Input
				if len(input) == 0 {
					input = []string{"text"}
				}
				output := um.Output
				if um.Type == "image" && len(output) == 0 {
					output = []string{"image", "text"}
				}
				nonChat = append(nonChat, NonChatModel{Model: m, Type: um.Type, Input: input, Output: output})
				continue
			}
			if um.Type != "" && um.Type != "chat" {
				warnings = append(warnings, fmt.Sprintf("models.json: %s/%s has an unknown model type, skipped", normalized, um.ID))
				continue
			}
			out = append(out, m)
		}
	}
	setNonChatModels(nonChat)
	return out, warnings
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

	// Ensure the active overlay starts from the full built-in catalog
	// when no live/cache overlay has been applied. Otherwise a fresh
	// install with only models.json would hide every built-in model.
	if !activeSet || active == nil {
		active = append([]Model(nil), Catalog...)
		activeSet = true
	}

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
			if um.ReasoningLevelMap != nil {
				existing.ReasoningLevelMap = maps.Clone(um.ReasoningLevelMap)
			}
			if um.API != "" {
				existing.API = um.API
			}
			if um.BaseURL != "" {
				existing.BaseURL = um.BaseURL
			}
			existing.Source = "user"
			existing.Speculative = false
			active[idx] = existing
		} else {
			// New model not in catalog.
			active = append(active, um)
		}
	}
}
