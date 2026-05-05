package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ProbeAPIKey verifies that key is valid for provider by making a
// lightweight authenticated request. Returns nil on success.
func ProbeAPIKey(ctx context.Context, provider, key string) error {
	if key == "" {
		return fmt.Errorf("empty key")
	}
	c := &http.Client{Timeout: 15 * time.Second}
	var req *http.Request
	var err error

	switch provider {
	case "anthropic":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "openai":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.openai.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "kimi":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.kimi.com/coding/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	default:
		return fmt.Errorf("unknown provider %q", provider)
	}

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", provider, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s rejected the key (http %d)", provider, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s http %d", provider, resp.StatusCode)
	}
	return nil
}
