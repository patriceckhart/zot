package agent

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// discoverableCustomProviders returns the models.json providers that opted
// into live discovery, sorted for deterministic refresh order.
func discoverableCustomProviders() []string {
	var out []string
	for name := range provider.CustomProviders() {
		if isDiscoverableCustomProvider(name) && CheckProviderEnabled(name) == nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// isDiscoverableCustomProvider reports whether name is a custom provider
// with live discovery enabled. Its catalog entries are transient, so config
// repair must not treat a missing model as stale.
func isDiscoverableCustomProvider(name string) bool {
	cfg, ok := provider.CustomProviders()[name]
	return ok && cfg.Discover && strings.TrimSpace(cfg.BaseURL) != "" && !isBuiltinProvider(name)
}

// CustomDiscoveryConfigured reports whether any custom provider has live
// discovery enabled. It reads only loaded configuration, never the network.
func CustomDiscoveryConfigured() bool {
	return len(discoverableCustomProviders()) > 0
}

// customProviderKey resolves the optional bearer key for a custom provider.
// A missing key is not an error because local servers commonly accept
// unauthenticated requests. skip is true when the only key is command-backed
// and mode forbids running the command.
func customProviderKey(ctx context.Context, name string, mode apiKeyCommandMode) (key string, skip bool, err error) {
	if err := CheckProviderEnabled(name); err != nil {
		return "", false, err
	}
	if cfg, ok := provider.CustomProviders()[name]; ok && cfg.NoAuth && !isBuiltinProvider(name) {
		return "", false, nil
	}
	if v := os.Getenv(normalizeCustomProviderEnvVar(name) + "_API_KEY"); v != "" {
		return v, false, nil
	}
	creds, err := AuthStoreFor().Load()
	if err != nil {
		return "", false, err
	}
	stored, ok := creds.AdditionalAPIKeyCreds[name]
	if !ok {
		return "", false, nil
	}
	if stored.APIKey == "" && stored.APIKeyCommand != nil && mode == apiKeyCommandSkip {
		return "", true, nil
	}
	key, _, err = resolveStoredAPIKey(ctx, name, stored, mode)
	return key, false, err
}

// RefreshCustomProviderModels lists models from every discovery-enabled
// custom provider and merges them into the transient managed catalog.
// A failing provider keeps its previous snapshot; errors are joined after
// all providers were attempted.
func RefreshCustomProviderModels(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return refreshCustomProviderModels(ctx, apiKeyCommandExecute)
}

func refreshCustomProviderModels(ctx context.Context, mode apiKeyCommandMode) error {
	var errs []error
	for _, name := range discoverableCustomProviders() {
		cfg := provider.CustomProviders()[name]
		key, skip, err := customProviderKey(ctx, name, mode)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if skip {
			continue
		}
		var models []provider.Model
		if cfg.API == "google" {
			models, err = provider.DiscoverGoogleNamed(ctx, name, key, cfg.BaseURL)
		} else {
			models, err = provider.DiscoverCustomProvider(ctx, name, cfg.BaseURL, key)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		provider.SetManagedModelsForProvider(name, models)
	}
	return errors.Join(errs...)
}
