package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DiscoverCloudGPU lists chat models from CloudGPU's public catalog.
// Image, speech, and transcription models and the auto alias are excluded.
// The catalog does not publish context, output, vision, or reasoning metadata.
func DiscoverCloudGPU(ctx context.Context, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = cloudgpuDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudgpu discover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudgpu discover http %d", resp.StatusCode)
	}
	var page struct {
		Data []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			InputPer1M  string `json:"input_usd_per_1m"`
			OutputPer1M string `json:"output_usd_per_1m"`
			CachedPer1M string `json:"cached_input_usd_per_1m"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page); err != nil {
		return nil, fmt.Errorf("cloudgpu discover parse: %w", err)
	}
	if page.Data == nil {
		return nil, fmt.Errorf("cloudgpu discover response is missing data")
	}
	out := make([]Model, 0, len(page.Data))
	seen := make(map[string]bool)
	for _, d := range page.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" || seen[id] || !cloudgpuChatModel(id) {
			continue
		}
		seen[id] = true
		display := strings.TrimSpace(d.Name)
		if display == "" {
			display = id
		}
		out = append(out, Model{
			Provider:         "cloudgpu",
			ID:               id,
			DisplayName:      display,
			Input:            []string{"text"},
			ContextWindow:    customDiscoveryContextWindow,
			MaxOutput:        customDiscoveryMaxOutput,
			AdaptiveThinking: usesAdaptiveThinking(Model{ID: id}),
			PriceInput:       cloudgpuPerMillion(d.InputPer1M),
			PriceOutput:      cloudgpuPerMillion(d.OutputPer1M),
			PriceCacheRead:   cloudgpuPerMillion(d.CachedPer1M),
			BaseURL:          baseURL,
			Source:           "live",
		})
	}
	return out, nil
}

// cloudgpuChatModel reports whether id is a chat model. The catalog mixes
// chat models with image, speech, and transcription models and the auto alias.
func cloudgpuChatModel(id string) bool {
	switch {
	case id == "auto":
		return false
	case strings.HasPrefix(id, "flux-"):
		return false
	case strings.HasSuffix(id, "-tts"):
		return false
	case strings.HasPrefix(id, "whisper"):
		return false
	case strings.HasSuffix(id, "-asr"):
		return false
	default:
		return true
	}
}

func cloudgpuPerMillion(raw string) float64 {
	rate, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0
	}
	return rate
}
