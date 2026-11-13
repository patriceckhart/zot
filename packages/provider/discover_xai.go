package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

const xaiDefaultBaseURL = "https://api.x.ai/v1"

// UnlistedXAIModel preserves explicit selections before discovery completes.
// The API validates the ID. Context is a conservative estimate, not an upstream
// guarantee. Unknown prices, capabilities, and output limits remain unset.
func UnlistedXAIModel(id string) Model {
	return Model{
		Provider: "xai", ID: id, DisplayName: id, API: APIResponses,
		ContextWindow: 32768, BaseURL: xaiDefaultBaseURL,
	}
}

// DiscoverXAI lists chat models and aliases available to the credential via
// /v1/models. Prices are USD cents per 100 million tokens, not per-token USD.
// See https://docs.x.ai/developers/rest-api-reference/inference/models.
// Published catalog metadata fills missing fields. No output limit is invented.
func DiscoverXAI(ctx context.Context, credential, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = xaiDefaultBaseURL
	}
	endpoint, err := customModelsURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("xai discovery requires an http or https API root")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("xai discovery request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("xai discovery request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xai discovery HTTP %d", resp.StatusCode)
	}
	var page struct {
		Data []xaiDiscoveredModel `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&page); err != nil {
		return nil, fmt.Errorf("xai discovery returned invalid model data")
	}
	if page.Data == nil {
		return nil, fmt.Errorf("xai discovery response is missing data")
	}
	known := make(map[string]Model)
	for _, model := range Catalog {
		if model.Provider == "xai" {
			known[model.ID] = model
		}
	}
	primaryIDs := make(map[string]bool)
	for _, entry := range page.Data {
		primaryIDs[strings.TrimSpace(entry.ID)] = true
	}
	var models []Model
	seen := make(map[string]bool)
	for _, entry := range page.Data {
		id := strings.TrimSpace(entry.ID)
		_, catalogued := known[id]
		if id == "" || !entry.chatModel(catalogued) {
			continue
		}
		model, ok := known[id]
		if !ok {
			model = UnlistedXAIModel(id)
		}
		model.Input = slices.Clone(model.Input)
		model.ReasoningLevelMap = maps.Clone(model.ReasoningLevelMap)
		model.BaseURL = strings.TrimSuffix(endpoint, "/models")
		model.Source = "live"
		entry.apply(&model)
		for _, alias := range append([]string{id}, entry.Aliases...) {
			alias = strings.TrimSpace(alias)
			if alias == "" || seen[alias] || (alias != id && primaryIDs[alias]) {
				continue
			}
			seen[alias] = true
			item := model
			item.ID = alias
			if alias != id {
				item.DisplayName = alias
			}
			models = append(models, item)
		}
	}
	return models, nil
}

type xaiDiscoveredModel struct {
	ID               string   `json:"id"`
	Aliases          []string `json:"aliases"`
	ContextLength    *int     `json:"context_length"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	ImagePrice       *float64 `json:"image_price"`
	InputPrice       *float64 `json:"prompt_text_token_price"`
	OutputPrice      *float64 `json:"completion_text_token_price"`
	CachePrice       *float64 `json:"cached_prompt_text_token_price"`
	ImageTokenPrice  *float64 `json:"prompt_image_token_price"`
	Threshold        *int     `json:"long_context_threshold"`
	InputAbove       *float64 `json:"prompt_text_token_price_long_context"`
	OutputAbove      *float64 `json:"completion_text_token_price_long_context"`
	CacheAbove       *float64 `json:"cached_prompt_text_token_price_long_context"`
	Capabilities     *struct {
		ReasoningEffort []string `json:"reasoning_effort"`
	} `json:"capabilities"`
}

func (d xaiDiscoveredModel) chatModel(known bool) bool {
	id := strings.ToLower(d.ID)
	if d.ImagePrice != nil || (d.OutputModalities != nil && !slices.Contains(d.OutputModalities, "text")) {
		return false
	}
	for _, word := range []string{"imagine", "image", "video", "audio", "voice", "embed", "tts", "stt"} {
		if strings.Contains(id, word) {
			return false
		}
	}
	return known || strings.HasPrefix(id, "grok-") || (d.InputPrice != nil && d.OutputPrice != nil)
}

func (d xaiDiscoveredModel) apply(m *Model) {
	if d.ContextLength != nil && *d.ContextLength > 0 {
		m.ContextWindow = *d.ContextLength
	}
	if d.InputModalities != nil {
		m.Input = slices.Clone(d.InputModalities)
	} else if d.ImageTokenPrice != nil && *d.ImageTokenPrice > 0 {
		m.Input = []string{"text", "image"}
	}
	if d.Capabilities != nil {
		m.Reasoning = false
		m.ReasoningLevelMap = make(map[string]string)
		for _, level := range reasoningLevelOrder[1:] {
			m.ReasoningLevelMap[level] = ""
		}
		for _, effort := range d.Capabilities.ReasoningEffort {
			if level := NormalizeReasoning(effort); reasoningLevelRank(level) > 0 {
				m.Reasoning = true
				m.ReasoningLevelMap[level] = level
			}
		}
	}
	setXAIPrice(&m.PriceInput, d.InputPrice)
	setXAIPrice(&m.PriceOutput, d.OutputPrice)
	setXAIPrice(&m.PriceCacheRead, d.CachePrice)
	if d.Threshold != nil {
		m.PriceTierInputTokens = 0
		if *d.Threshold > 1 {
			m.PriceTierInputTokens = *d.Threshold - 1
		}
	}
	if m.PriceTierInputTokens > 0 {
		// Preserve catalog tiers when an ID-only response omits metadata.
		// xAI's explicit zero long-context rates mean use the ordinary rate.
		applyXAIAbovePrice(&m.PriceInputAbove, d.InputAbove, m.PriceInput)
		applyXAIAbovePrice(&m.PriceOutputAbove, d.OutputAbove, m.PriceOutput)
		applyXAIAbovePrice(&m.PriceCacheReadAbove, d.CacheAbove, m.PriceCacheRead)
	}
}

func applyXAIAbovePrice(price *float64, raw *float64, ordinary float64) {
	if raw == nil && *price != 0 {
		return
	}
	*price = ordinary
	if raw != nil && *raw > 0 {
		setXAIPrice(price, raw)
	}
}

func setXAIPrice(price *float64, raw *float64) {
	if raw != nil && *raw >= 0 {
		*price = *raw / 10000
	}
}
