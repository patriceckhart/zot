package auth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProbeAPIKey verifies that key is valid for provider by making a
// lightweight authenticated request. Returns nil on success.
func ProbeAPIKey(ctx context.Context, provider, key string) error {
	return probeAPIKey(ctx, provider, key, &http.Client{Timeout: 15 * time.Second})
}

func probeAPIKey(ctx context.Context, provider, key string, c *http.Client) error {
	if key == "" {
		return fmt.Errorf("empty key")
	}
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
	case "deepseek":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.deepseek.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "google":
		// Google Generative Language: list models with the API key.
		// Accepts the key via x-goog-api-key header (preferred over
		// the ?key= query param so it doesn't show up in proxy logs).
		req, err = http.NewRequestWithContext(ctx, "GET", "https://generativelanguage.googleapis.com/v1beta/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("x-goog-api-key", key)
	// OpenAI-compatible third parties: a GET /v1/models with bearer auth
	// is enough to validate the key. Branches kept explicit (rather than a
	// generic default) so the URL list is searchable and reviewable.
	case "moonshotai":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.moonshot.ai/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "moonshotai-cn":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.moonshot.cn/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "groq":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.groq.com/openai/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "cerebras":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.cerebras.ai/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "xai":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.x.ai/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "together":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.together.ai/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "openrouter":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://openrouter.ai/api/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "gondola":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.gondola-ai.com/v1/balance", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "yolo-auto":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://yolo-auto.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "cheaperinference":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.cheaperinference.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "opper":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.opper.ai/v3/compat/models?view=recommended", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "runcrate":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.runcrate.ai/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "cloudgpu":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://cloudgpu.app/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "huggingface":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://router.huggingface.co/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "zai":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.z.ai/api/coding/paas/v4/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "mistral":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.mistral.ai/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "xiaomi":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.xiaomimimo.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "xiaomi-token-plan-ams":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://token-plan-ams.xiaomimimo.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "xiaomi-token-plan-cn":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://token-plan-cn.xiaomimimo.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "xiaomi-token-plan-sgp":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://token-plan-sgp.xiaomimimo.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "minimax":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.minimax.io/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "minimax-cn":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.minimaxi.com/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "fireworks":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://api.fireworks.ai/inference/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "vercel-ai-gateway":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://ai-gateway.vercel.sh/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "opencode":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://opencode.ai/zen/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "opencode-go":
		req, err = http.NewRequestWithContext(ctx, "GET", "https://opencode.ai/zen/go/v1/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "Bearer "+key)
	case "azure-openai-responses":
		return nil
	case "amazon-bedrock":
		return nil
	case "google-vertex":
		return nil
	case "cloudflare-workers-ai", "cloudflare-ai-gateway":
		return nil
	case "github-copilot":
		return nil
	default:
		// Custom providers are registered with the auth package at startup
		// from models.json. Some self-hosted or enterprise gateways do not
		// expose a model-list endpoint, so accept and store the key without
		// probing just like Bedrock/Azure.
		if isExtraAPIKeyProvider(provider) {
			return nil
		}
		return fmt.Errorf("unknown provider %q", provider)
	}

	if strings.Contains(req.URL.String(), "{CLOUDFLARE_ACCOUNT_ID}") {
		if acct := os.Getenv("CLOUDFLARE_ACCOUNT_ID"); acct != "" {
			u := strings.ReplaceAll(req.URL.String(), "{CLOUDFLARE_ACCOUNT_ID}", acct)
			req.URL, _ = req.URL.Parse(u)
		}
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
