package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
)

func setupXAIDiscovery(t *testing.T) {
	t.Helper()
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "")
	var disabled []string
	for _, name := range knownProviders {
		if name != "xai" {
			disabled = append(disabled, name)
		}
	}
	if err := SaveConfig(Config{DisabledProviders: disabled}); err != nil {
		t.Fatal(err)
	}
	provider.SetLiveModels(nil)
	original := http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = original
		provider.SetLiveModels(nil)
	})
}

func TestXAIDiscoveryRefreshAndCache(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		for _, fresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("oauth=%v/fresh=%v", oauth, fresh), func(t *testing.T) {
				setupXAIDiscovery(t)
				if oauth {
					if err := AuthStoreFor().SetOAuth("xai", auth.OAuthToken{AccessToken: "synthetic-key"}); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Setenv("XAI_API_KEY", "synthetic-key")
				}
				cachedAt := time.Now().UTC().Add(-time.Hour)
				if !fresh {
					cachedAt = cachedAt.Add(-provider.CacheTTL)
				}
				if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{FetchedAt: cachedAt, Models: []provider.Model{
					{Provider: "openai", ID: "unrelated-cached"}, {Provider: "xai", ID: "old-account-model"},
				}}); err != nil {
					t.Fatal(err)
				}
				calls, status := 0, http.StatusOK
				http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.String() != "https://api.x.ai/v1/models" {
						t.Errorf("unexpected request: %s", r.URL)
						return nil, fmt.Errorf("unexpected discovery endpoint")
					}
					if r.Header.Get("Authorization") != "Bearer synthetic-key" {
						t.Error("discovery did not use the selected credential")
					}
					calls++
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"grok-future-live","context_length":900000,"prompt_text_token_price":30000,"capabilities":{"reasoning_effort":["low","high"]}}]}`))}, nil
				})
				refreshModels()
				if calls != 1 {
					t.Fatalf("calls=%d, want 1", calls)
				}
				m, err := provider.FindModel("xai", "grok-future-live")
				if err != nil || m.ContextWindow != 900000 || m.MaxOutput != 0 || m.PriceInput != 3 || m.API != provider.APIResponses {
					t.Fatalf("live model=%+v, err=%v", m, err)
				}
				if _, err := provider.FindModel("xai", "old-account-model"); err == nil {
					t.Fatal("previous account model survived refresh")
				}
				cached, err := provider.LoadCache(ModelCachePath())
				if err != nil {
					t.Fatal(err)
				}
				if fresh && !cached.FetchedAt.Equal(cachedAt) {
					t.Fatal("xAI refresh extended unrelated cache lifetime")
				}
				provider.SetLiveModels(nil)
				LoadCachedModels()
				if m, err := provider.FindModel("xai", "grok-future-live"); err != nil || m.MaxOutput != 0 || m.API != provider.APIResponses {
					t.Fatalf("cached model=%+v, err=%v", m, err)
				}
				refreshModels()
				if calls != 2 {
					t.Fatalf("fresh cache suppressed account discovery: calls=%d", calls)
				}
				status = http.StatusUnauthorized
				refreshModels()
				if _, err := provider.FindModel("xai", "grok-future-live"); err == nil {
					t.Fatal("account model survived failed refresh")
				}
				status = http.StatusOK
				refreshModels()
				if oauth {
					if err := AuthStoreFor().Clear("xai"); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Setenv("XAI_API_KEY", "")
				}
				beforeLogout := calls
				refreshModels()
				if calls != beforeLogout {
					t.Fatal("logout still made a credentialed discovery request")
				}
				if _, err := provider.FindModel("xai", "grok-future-live"); err == nil {
					t.Fatal("account model survived logout")
				}
				if _, err := provider.FindModel("xai", "grok-4.5"); err != nil {
					t.Fatal("offline default missing:", err)
				}
				if fresh {
					if _, err := provider.FindModel("openai", "unrelated-cached"); err != nil {
						t.Fatal("unrelated catalog was lost:", err)
					}
				}
			})
		}
	}
}

func TestXAIDiscoverySkipsCommandCredentialsAndDisabledProvider(t *testing.T) {
	setupXAIDiscovery(t)
	t.Setenv("ZOT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(t.TempDir(), "command-ran")
	creds := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		"xai": {APIKeyCommand: &auth.APIKeyCommand{Program: os.Args[0], Args: []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker}}},
	}}
	encoded, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
		t.Error("unexpected network request")
		return nil, fmt.Errorf("unexpected network request")
	})
	refreshModels()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("background credential command executed: %v", err)
	}
	t.Setenv("XAI_API_KEY", "synthetic-key")
	if err := SaveConfig(Config{DisabledProviders: knownProviders}); err != nil {
		t.Fatal(err)
	}
	refreshModels()
}

func TestXAIUnlistedSelectionReachesResponses(t *testing.T) {
	setupXAIDiscovery(t)
	t.Setenv("XAI_API_KEY", "synthetic-key")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider, cfg.Model = "xai", "grok-future-unlisted"
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ValidateAndRepairConfig()
	cfg, _ = LoadConfig()
	if cfg.Model != "grok-future-unlisted" {
		t.Fatal("saved selection was replaced before discovery")
	}
	calls := 0
	http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.x.ai/v1/responses" {
			t.Errorf("unexpected inference endpoint: %s", r.URL)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "grok-future-unlisted" {
			t.Errorf("request model=%q, err=%v", body.Model, err)
		}
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	for _, explicit := range []string{"", "grok-future-unlisted"} {
		r, err := Resolve(Args{Provider: "xai", Model: explicit, CWD: t.TempDir(), NoTools: true, NoContextFiles: true, NoSkill: true}, true)
		if err != nil {
			t.Fatal(err)
		}
		if r.Provider != "xai" || r.Model != cfg.Model || r.MaxOutput != 0 {
			t.Fatalf("resolved model=%s/%s, output=%d", r.Provider, r.Model, r.MaxOutput)
		}
		events, err := r.NewClient().Stream(context.Background(), provider.Request{Model: r.Model})
		if err != nil {
			t.Fatal(err)
		}
		for range events {
		}
	}
	if calls != 2 {
		t.Fatalf("inference calls=%d", calls)
	}
}

func TestXAIDiscoveryRespectsUserOverrides(t *testing.T) {
	setupXAIDiscovery(t)
	t.Setenv("XAI_API_KEY", "synthetic-key")
	if err := os.WriteFile(UserModelsPath(), []byte(`{"providers":{"xai":{"models":[{"id":"grok-user-model","contextWindow":123456,"maxTokens":8192,"reasoning":true}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"grok-user-model","context_length":900000}]}`))}, nil
	})
	refreshModels()
	m, err := provider.FindModel("xai", "grok-user-model")
	if err != nil || m.ContextWindow != 123456 || m.MaxOutput != 8192 || m.Source != "user" || m.API != provider.APIResponses {
		t.Fatalf("user override=%+v, err=%v", m, err)
	}
}
