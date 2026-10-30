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

func TestCloudGPUPreservesPlainSelection(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("CLOUDGPU_API_KEY", "test-key")
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	for _, id := range []string{"kimi-k2.6", "glm-5.2"} {
		if err := SaveConfig(Config{Provider: "cloudgpu", Model: id}); err != nil {
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
			r, err := Resolve(Args{Provider: "cloudgpu", Model: explicit, CWD: t.TempDir(), NoTools: true, NoContextFiles: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			if r.Model != id || r.Provider != "cloudgpu" {
				t.Errorf("resolved %s/%s, want cloudgpu/%s", r.Provider, r.Model, id)
			}
			if r.MaxOutput != 4096 {
				t.Errorf("fallback output limit = %d, want 4096", r.MaxOutput)
			}
			if r.Credential != "test-key" || r.NewClient().Name() != "cloudgpu" {
				t.Error("provider credential or client wiring is incorrect")
			}
		}
	}
}

func TestCloudGPUDefaultResolvesOffline(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("CLOUDGPU_API_KEY", "test-key")
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	r, err := Resolve(Args{Provider: "cloudgpu", CWD: t.TempDir(), NoTools: true, NoContextFiles: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "deepseek-v4-flash" || r.BaseURL != "https://cloudgpu.app/v1" {
		t.Errorf("resolved %s at %s", r.Model, r.BaseURL)
	}
	if envVarName("cloudgpu") != "CLOUDGPU" || provider.ProviderLabel("cloudgpu") != "CloudGPU" {
		t.Error("provider naming is incorrect")
	}
}

func TestCloudGPURefreshGatesPublicCatalogOnCredential(t *testing.T) {
	for _, freshCache := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh-shared-cache=%t", freshCache), func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "KIMI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY", "GONDOLA_API_KEY", "YOLO_AUTO_API_KEY", "CHEAPER_INFERENCE_API_KEY", "OPPER_API_KEY", "RUNCRATE_API_KEY", "CLOUDGPU_API_KEY",
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
				if r.URL.Host != "cloudgpu.app" || r.URL.Path != "/v1/models" {
					return nil, fmt.Errorf("unexpected discovery request: %s", r.URL)
				}
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"glm-5.2","name":"GLM 5.2","input_usd_per_1m":"1.26","output_usd_per_1m":"3.96","cached_input_usd_per_1m":"0.23"}]}`))}, nil
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
			t.Setenv("CLOUDGPU_API_KEY", "test-key")
			refreshModels()
			if calls != 1 {
				t.Fatalf("discovery calls = %d, want 1", calls)
			}
			m, err := provider.FindModel("cloudgpu", "glm-5.2")
			if err != nil {
				t.Fatal(err)
			}
			if m.ContextWindow != 32768 || m.MaxOutput != 4096 || m.Reasoning || m.PriceInput != 1.26 || m.Source != "live" {
				t.Errorf("discovered metadata = %+v", m)
			}
			refreshModels()
			if calls != 1 {
				t.Errorf("discovery calls with a cached CloudGPU catalog = %d, want 1", calls)
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
