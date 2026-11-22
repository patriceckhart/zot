package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestUberLLMPreservesRoutedSelection(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("UBERLLM_API_KEY", "test-key")
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	for _, id := range []string{"z-ai/glm-5.3", "vendor/new-model"} {
		if err := SaveConfig(Config{Provider: "uberllm", Model: id}); err != nil {
			t.Fatal(err)
		}
		ValidateAndRepairConfig()
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model != id {
			t.Errorf("saved model = %q, want %q", cfg.Model, id)
		}
		for _, explicit := range []string{"", id} {
			r, err := Resolve(Args{Provider: "uberllm", Model: explicit, CWD: t.TempDir(), NoTools: true, NoContextFiles: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			if r.Model != id || r.Provider != "uberllm" {
				t.Errorf("resolved %s/%s, want uberllm/%s", r.Provider, r.Model, id)
			}
			if r.MaxOutput != 4096 {
				t.Errorf("fallback output limit = %d, want 4096", r.MaxOutput)
			}
			if r.Credential != "test-key" || r.NewClient().Name() != "uberllm" {
				t.Error("provider credential or client wiring is incorrect")
			}
		}
	}
}

func TestUberLLMDefaultResolvesOffline(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("UBERLLM_API_KEY", "test-key")
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	r, err := Resolve(Args{Provider: "uberllm", CWD: t.TempDir(), NoTools: true, NoContextFiles: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "deepseek/deepseek-v4-flash" || r.BaseURL != "https://api.uberllm.dev/v1" {
		t.Errorf("resolved %s at %s", r.Model, r.BaseURL)
	}
	if envVarName("uberllm") != "UBERLLM" || provider.ProviderLabel("uberllm") != "UberLLM" {
		t.Error("provider naming is incorrect")
	}
}

func TestUberLLMRefreshGatesPublicCatalogOnCredential(t *testing.T) {
	for _, freshCache := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh-shared-cache=%t", freshCache), func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "KIMI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY", "GONDOLA_API_KEY", "YOLO_AUTO_API_KEY", "CHEAPER_INFERENCE_API_KEY", "OPPER_API_KEY", "RUNCRATE_API_KEY", "CLOUDGPU_API_KEY", "UBERLLM_API_KEY",
				"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_BEARER_TOKEN_BEDROCK"} {
				t.Setenv(name, "")
			}
			provider.SetLiveModels(nil)
			original := http.DefaultTransport
			t.Cleanup(func() {
				http.DefaultTransport = original
				provider.SetLiveModels(nil)
			})
			calls := 0
			http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.uberllm.dev" || r.URL.Path != "/v1/models" {
					return nil, fmt.Errorf("unexpected discovery request: %s", r.URL)
				}
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"z-ai/glm-5.3","display_name":"GLM 5.3","type":"chat","available":true,"input_modalities":["text"],"output_modalities":["text"],"context_length":200000,"max_output_tokens":131072,"supports_tools":true,"supports_reasoning":true,"pricing":{"input_usd":0.5,"output_usd":2.2}}]}`))}, nil
			})

			fetchedAt := time.Now().UTC().Add(-time.Hour)
			if freshCache {
				if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{
					FetchedAt: fetchedAt,
					Models:    []provider.Model{{Provider: "openai", ID: "cached-model"}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			refreshModels()
			if calls != 0 {
				t.Fatalf("discovery calls without a credential = %d, want 0", calls)
			}
			t.Setenv("UBERLLM_API_KEY", "test-key")
			refreshModels()
			if calls != 1 {
				t.Fatalf("discovery calls = %d, want 1", calls)
			}
			m, err := provider.FindModel("uberllm", "z-ai/glm-5.3")
			if err != nil {
				t.Fatal(err)
			}
			if m.ContextWindow != 200000 || m.MaxOutput != 131072 || !m.Reasoning || m.PriceInput != 0.5 || m.Source != "live" {
				t.Errorf("discovered metadata = %+v", m)
			}
			refreshModels()
			if calls != 1 {
				t.Errorf("discovery calls with a cached UberLLM catalog = %d, want 1", calls)
			}
			if freshCache {
				if _, err := provider.FindModel("openai", "cached-model"); err != nil {
					t.Errorf("unrelated cached model was lost: %v", err)
				}
				cached, err := provider.LoadCache(ModelCachePath())
				if err != nil {
					t.Fatal(err)
				}
				if !cached.FetchedAt.Equal(fetchedAt) {
					t.Errorf("shared cache timestamp = %v, want %v", cached.FetchedAt, fetchedAt)
				}
			}
		})
	}
}
