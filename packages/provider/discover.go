package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DiscoverAnthropic lists model ids visible to key on api.anthropic.com.
// The API returns a paginated list; we page through until has_more is false.
func DiscoverAnthropic(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var out []Model
	after := ""
	for {
		url := strings.TrimRight(baseURL, "/") + "/v1/models?limit=1000"
		if after != "" {
			url += "&after_id=" + after
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicAPIVersion)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("anthropic discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var page struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("anthropic discover parse: %w", err)
		}
		for _, d := range page.Data {
			out = append(out, Model{
				Provider:    "anthropic",
				ID:          d.ID,
				DisplayName: d.DisplayName,
				Source:      "live",
			})
		}
		if !page.HasMore || page.LastID == "" {
			break
		}
		after = page.LastID
	}
	return out, nil
}

// DiscoverOpenAI lists model ids visible to key on api.openai.com.
func DiscoverOpenAI(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	client := &http.Client{Timeout: 15 * time.Second}
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("openai discover parse: %w", err)
	}
	var out []Model
	for _, d := range page.Data {
		// Keep only chat-capable families. OpenAI's /v1/models returns
		// everything including embeddings, TTS, DALL-E, etc.
		if !looksLikeChatModel(d.ID) {
			continue
		}
		out = append(out, Model{
			Provider:    "openai",
			ID:          d.ID,
			DisplayName: d.ID,
			Source:      "live",
		})
	}
	return out, nil
}

// DiscoverGoogle lists Gemini model ids visible to key on
// generativelanguage.googleapis.com. The API paginates with
// nextPageToken; we follow it until exhausted.
func DiscoverGoogle(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var out []Model
	pageToken := ""
	for {
		url := strings.TrimRight(baseURL, "/") + "/v1beta/models?pageSize=200"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("google discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var page struct {
			Models []struct {
				Name                       string   `json:"name"` // "models/gemini-2.5-pro"
				DisplayName                string   `json:"displayName"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
				InputTokenLimit            int      `json:"inputTokenLimit"`
				OutputTokenLimit           int      `json:"outputTokenLimit"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("google discover parse: %w", err)
		}
		for _, m := range page.Models {
			// Strip the "models/" prefix Gemini uses on resource names.
			id := strings.TrimPrefix(m.Name, "models/")
			if !looksLikeGeminiChatModel(id, m.SupportedGenerationMethods) {
				continue
			}
			display := m.DisplayName
			if display == "" {
				display = id
			}
			out = append(out, Model{
				Provider:      "google",
				ID:            id,
				DisplayName:   display,
				ContextWindow: m.InputTokenLimit,
				MaxOutput:     m.OutputTokenLimit,
				Source:        "live",
			})
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

// looksLikeGeminiChatModel filters Gemini's /v1beta/models output
// down to entries usable with streamGenerateContent. The API also
// lists embedding models, AQA, and other non-chat artefacts.
func looksLikeGeminiChatModel(id string, methods []string) bool {
	if !strings.HasPrefix(id, "gemini-") && !strings.HasPrefix(id, "gemma-") {
		return false
	}
	if strings.Contains(id, "embedding") || strings.Contains(id, "aqa") {
		return false
	}
	if len(methods) > 0 {
		ok := false
		for _, m := range methods {
			if m == "generateContent" || m == "streamGenerateContent" {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// looksLikeChatModel returns true for OpenAI ids that can plausibly be
// used with the chat/completions endpoint. Errs on the side of inclusion.
func looksLikeChatModel(id string) bool {
	switch {
	case strings.HasPrefix(id, "gpt-"):
		return true
	case strings.HasPrefix(id, "o1"):
		return true
	case strings.HasPrefix(id, "o3"):
		return true
	case strings.HasPrefix(id, "o4"):
		return true
	case strings.HasPrefix(id, "o5"):
		return true
	case strings.HasPrefix(id, "chatgpt-"):
		return true
	}
	return false
}

const (
	openrouterDefaultBaseURL = "https://openrouter.ai/api/v1"
	gondolaDefaultBaseURL    = "https://api.gondola-ai.com/v1"
)

// DiscoverOpenRouter lists models from OpenRouter's public /models
// endpoint (no auth). Per-token USD prices are converted to USD per 1M
// tokens to match the rest of the catalog. baseURL defaults to the
// public endpoint.
func DiscoverOpenRouter(ctx context.Context, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = openrouterDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	url := baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openrouter discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
			TopProvider struct {
				ContextLength       int  `json:"context_length"`
				MaxCompletionTokens *int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("openrouter discover parse: %w", err)
	}
	var out []Model
	for _, d := range page.Data {
		if d.ID == "" {
			continue
		}
		display := d.Name
		if display == "" {
			display = d.ID
		}
		ctxWin := d.ContextLength
		if d.TopProvider.ContextLength > 0 && (ctxWin == 0 || d.TopProvider.ContextLength < ctxWin) {
			ctxWin = d.TopProvider.ContextLength
		}
		maxOut := 0
		if d.TopProvider.MaxCompletionTokens != nil {
			maxOut = *d.TopProvider.MaxCompletionTokens
		}
		out = append(out, Model{
			Provider:        "openrouter",
			ID:              d.ID,
			DisplayName:     display,
			ContextWindow:   ctxWin,
			MaxOutput:       maxOut,
			Reasoning:       openrouterSupportsReasoning(d.SupportedParameters),
			PriceInput:      perMillionTokens(d.Pricing.Prompt),
			PriceOutput:     perMillionTokens(d.Pricing.Completion),
			PriceCacheRead:  perMillionTokens(d.Pricing.InputCacheRead),
			PriceCacheWrite: perMillionTokens(d.Pricing.InputCacheWrite),
			BaseURL:         baseURL,
			Source:          "live",
		})
	}
	return out, nil
}

// DiscoverOpenRouterPresets lists the authenticated user's OpenRouter
// presets and returns them as selectable models with ids of the form
// "@preset/{slug}". Requires an API key; /presets is not public.
func DiscoverOpenRouterPresets(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = openrouterDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	var out []Model
	offset := 0
	for {
		url := fmt.Sprintf("%s/presets?limit=100&offset=%d", baseURL, offset)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		if apiKey != "" {
			req.Header.Set("authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("openrouter presets http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var page struct {
			Data []struct {
				Name        string `json:"name"`
				Slug        string `json:"slug"`
				Description string `json:"description"`
				Status      string `json:"status"`
			} `json:"data"`
			TotalCount int `json:"total_count"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("openrouter presets parse: %w", err)
		}
		for _, d := range page.Data {
			if d.Slug == "" || d.Status == "archived" || d.Status == "disabled" {
				continue
			}
			display := d.Name
			if display == "" {
				display = d.Slug
			}
			out = append(out, Model{
				Provider:    "openrouter",
				ID:          "@preset/" + d.Slug,
				DisplayName: display + " (preset)",
				BaseURL:     baseURL,
				Source:      "live",
			})
		}
		if len(page.Data) == 0 || offset+len(page.Data) >= page.TotalCount {
			break
		}
		offset += len(page.Data)
	}
	return out, nil
}

// openrouterSupportsReasoning reports whether OpenRouter's
// supported_parameters list marks the model as reasoning-capable.
func openrouterSupportsReasoning(params []string) bool {
	for _, p := range params {
		if p == "reasoning" || p == "include_reasoning" {
			return true
		}
	}
	return false
}

// DiscoverGondola lists text models from Gondola's public catalog. Gondola
// reports prices directly in USD per 1M tokens, matching Model's units.
func DiscoverGondola(ctx context.Context, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = gondolaDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gondola discover http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page struct {
		Data []struct {
			ID                  string `json:"id"`
			DisplayName         string `json:"display_name"`
			Modality            string `json:"modality"`
			Endpoint            string `json:"endpoint"`
			ContextLength       int    `json:"context_length"`
			MaxCompletionTokens int    `json:"max_completion_tokens"`
			SupportsReasoning   bool   `json:"supports_reasoning"`
			Pricing             struct {
				Input      float64 `json:"input_usd_per_mtok"`
				Output     float64 `json:"output_usd_per_mtok"`
				CacheRead  float64 `json:"cached_input_usd_per_mtok"`
				CacheWrite float64 `json:"cache_write_usd_per_mtok"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("gondola discover parse: %w", err)
	}
	out := make([]Model, 0, len(page.Data))
	for _, d := range page.Data {
		if d.ID == "" || d.Modality != "text" || d.Endpoint != "/v1/chat/completions" {
			continue
		}
		display := d.DisplayName
		if display == "" {
			display = d.ID
		}
		model := Model{ID: d.ID}
		out = append(out, Model{
			Provider:         "gondola",
			ID:               d.ID,
			DisplayName:      display,
			ContextWindow:    d.ContextLength,
			MaxOutput:        d.MaxCompletionTokens,
			Reasoning:        d.SupportsReasoning,
			AdaptiveThinking: usesAdaptiveThinking(model),
			PriceInput:       d.Pricing.Input,
			PriceOutput:      d.Pricing.Output,
			PriceCacheRead:   d.Pricing.CacheRead,
			PriceCacheWrite:  d.Pricing.CacheWrite,
			BaseURL:          baseURL,
			Source:           "live",
		})
	}
	return out, nil
}

// perMillionTokens converts a per-token USD price string to USD per 1M
// tokens. Empty or unparseable values become 0.
func perMillionTokens(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * 1_000_000
}
