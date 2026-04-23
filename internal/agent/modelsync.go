package agent

import (
	"context"
	"path/filepath"
	"time"

	"github.com/patriceckhart/zot/internal/provider"
)

// ModelCachePath returns the on-disk location of the merged model cache.
func ModelCachePath() string {
	return filepath.Join(ZotHome(), "models-cache.json")
}

// UserModelsPath returns the path to the user's models.json override.
func UserModelsPath() string {
	return filepath.Join(ZotHome(), "models.json")
}

// LoadCachedModels loads the cache file and applies it to the provider
// package so FindModel / ModelsForProvider see live ids immediately.
// Safe to call before any credentials are known.
func LoadCachedModels() {
	c, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		return
	}
	if len(c.Models) > 0 {
		provider.SetLiveModels(c.Models)
	}
}

// LoadUserModels reads $ZOT_HOME/models.json and merges any user-defined
// models into the active catalog. User models take highest precedence.
func LoadUserModels() {
	models := provider.LoadUserModels(UserModelsPath())
	provider.SetUserModels(models)
}

// RefreshModelsAsync kicks a background discovery for every provider
// we have credentials for. Refreshed results are merged into the
// active catalog and persisted to the on-disk cache.
//
// Silent on error: discovery is a nice-to-have. Callers can still use
// the baked-in catalog if this fails.
func RefreshModelsAsync() {
	go refreshModels()
}

func refreshModels() {
	cached, _ := provider.LoadCache(ModelCachePath())
	if cached.IsFresh() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var all []provider.Model

	if cred, method, err := ResolveCredential("anthropic", ""); err == nil && method == "apikey" {
		// /v1/models on Anthropic is API-key only; OAuth tokens can
		// also list models via the bearer header, but we skip OAuth
		// here to avoid surprise rate-limit hits on subscription keys.
		if live, err := provider.DiscoverAnthropic(ctx, cred, ""); err == nil {
			all = append(all, live...)
		}
	}
	if cred, method, err := ResolveCredential("openai", ""); err == nil && method == "apikey" {
		if live, err := provider.DiscoverOpenAI(ctx, cred, ""); err == nil {
			all = append(all, live...)
		}
	}

	if len(all) == 0 {
		return
	}
	provider.SetLiveModels(all)
	_ = provider.SaveCache(ModelCachePath(), provider.ModelCache{
		FetchedAt: time.Now().UTC(),
		Models:    all,
	})
}
