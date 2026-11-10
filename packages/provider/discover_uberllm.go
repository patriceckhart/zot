package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// DiscoverUberLLM lists tool-capable chat models from UberLLM's public
// catalog. Models marked unavailable and non-text outputs are excluded.
// UberLLM reports pricing.input_usd and pricing.output_usd in USD per 1M
// tokens.
func DiscoverUberLLM(ctx context.Context, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = uberllmDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("uberllm discover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("uberllm discover http %d", resp.StatusCode)
	}
	var page struct {
		Data []struct {
			ID                string   `json:"id"`
			DisplayName       string   `json:"display_name"`
			Type              string   `json:"type"`
			Available         *bool    `json:"available"`
			InputModalities   []string `json:"input_modalities"`
			OutputModalities  []string `json:"output_modalities"`
			ContextLength     int      `json:"context_length"`
			MaxOutputTokens   int      `json:"max_output_tokens"`
			SupportsTools     bool     `json:"supports_tools"`
			SupportsReasoning bool     `json:"supports_reasoning"`
			Pricing           struct {
				InputUSD  float64 `json:"input_usd"`
				OutputUSD float64 `json:"output_usd"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page); err != nil {
		return nil, fmt.Errorf("uberllm discover parse: %w", err)
	}
	if page.Data == nil {
		return nil, fmt.Errorf("uberllm discover response is missing data")
	}
	out := make([]Model, 0, len(page.Data))
	seen := make(map[string]bool)
	for _, d := range page.Data {
		id := strings.TrimSpace(d.ID)
		chat := (d.Type == "" || d.Type == "chat") && slices.Equal(d.OutputModalities, []string{"text"}) && slices.Contains(d.InputModalities, "text")
		available := d.Available == nil || *d.Available
		if id == "" || seen[id] || !chat || !available || !d.SupportsTools {
			continue
		}
		seen[id] = true
		display := d.DisplayName
		if display == "" {
			display = id
		}
		window, output := d.ContextLength, d.MaxOutputTokens
		if window <= 0 {
			window = customDiscoveryContextWindow
		}
		if output <= 0 || output > window {
			output = min(customDiscoveryMaxOutput, window)
		}
		input := []string{"text"}
		if slices.Contains(d.InputModalities, "image") {
			input = append(input, "image")
		}
		out = append(out, Model{
			Provider:         "uberllm",
			ID:               id,
			DisplayName:      display,
			Input:            input,
			ContextWindow:    window,
			MaxOutput:        output,
			Reasoning:        d.SupportsReasoning,
			AdaptiveThinking: usesAdaptiveThinking(Model{ID: id}),
			PriceInput:       max(d.Pricing.InputUSD, 0),
			PriceOutput:      max(d.Pricing.OutputUSD, 0),
			BaseURL:          baseURL,
			Source:           "live",
		})
	}
	return out, nil
}
