package agent

import (
	"context"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// resolveLMStudioConfig never assumes a localhost endpoint. Discovery is
// enabled only by a saved server URL, independently of whether a key is needed.
func resolveLMStudioConfig(ctx context.Context, mode apiKeyCommandMode) (string, string, error) {
	if err := CheckProviderEnabled(provider.LMStudioProviderID); err != nil {
		return "", "", err
	}
	creds, err := AuthStoreFor().Load()
	if err != nil {
		return "", "", err
	}
	stored, ok := creds.AdditionalAPIKeyCreds[provider.LMStudioProviderID]
	if !ok || strings.TrimSpace(stored.BaseURL) == "" {
		return "", "", nil
	}
	root, err := provider.NormalizeLMStudioURL(stored.BaseURL)
	if err != nil {
		return "", "", err
	}
	if stored.APIKeyCommand != nil && mode == apiKeyCommandSkip {
		return "", "", nil
	}
	key, _, err := resolveStoredAPIKey(ctx, provider.LMStudioProviderID, stored, mode)
	return root, key, err
}

// LMStudioConfigured checks only saved configuration, never the network.
func LMStudioConfigured() bool {
	if CheckProviderEnabled(provider.LMStudioProviderID) != nil {
		return false
	}
	root, _, err := AuthStoreFor().EndpointCredential(provider.LMStudioProviderID)
	return err == nil && strings.TrimSpace(root) != ""
}

func RefreshLMStudioModels(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return refreshLMStudioModels(ctx, apiKeyCommandExecute)
}

func refreshLMStudioModels(ctx context.Context, mode apiKeyCommandMode) error {
	root, key, err := resolveLMStudioConfig(ctx, mode)
	if err != nil {
		return err
	}
	if root == "" {
		if LMStudioConfigured() {
			return nil
		}
		provider.SetManagedModelsForProvider(provider.LMStudioProviderID, nil)
		return nil
	}
	models, err := provider.DiscoverLMStudio(ctx, root, key)
	if err != nil {
		return err
	}
	// A login or logout may have changed the endpoint during the request.
	currentRoot, currentKey, err := resolveLMStudioConfig(ctx, mode)
	if err != nil {
		return err
	}
	if currentRoot != root || currentKey != key {
		return nil
	}
	provider.SetManagedModelsForProvider(provider.LMStudioProviderID, models)
	return nil
}
