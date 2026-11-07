package agent

import (
	"context"
	"fmt"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/provider"
)

// continuousInference resolves only the selected provider's inference settings.
// Host credentials and endpoint overrides never cross provider boundaries.
func continuousInference(ctx context.Context, host Resolved, config continuous.AgentConfig) (Resolved, error) {
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}
	name := canonicalProvider(config.Provider)
	if name == "" {
		name = host.Provider
	}
	if err := CheckProviderEnabled(name); err != nil {
		return Resolved{}, err
	}
	if !isKnownProvider(name) {
		return Resolved{}, fmt.Errorf("unknown provider %q", name)
	}
	model := config.Model
	if model == "" {
		if name == host.Provider {
			model = host.Model
		} else {
			model = defaultModelForProvider(name)
		}
	}
	if name == host.Provider && model == host.Model {
		return host, nil
	}
	// Resolve's interactive stale-model fallback must not silently substitute a
	// different model for a durable conversation's explicit selection.
	if _, err := provider.FindModel(name, model); err != nil {
		_, custom := provider.CustomProviders()[name]
		local := name == "ollama" || name == provider.LlamaCPPProviderID || name == provider.LMStudioProviderID
		if !custom && !local && !(isGatewayProvider(name) && isGatewayRoutedModelID(model)) {
			return Resolved{}, fmt.Errorf("unknown model %q for provider %q", model, name)
		}
	}
	args := Args{Provider: name, Model: model}
	if name == host.Provider {
		args.BaseURL = host.BaseURL
		args.InsecureTLS = host.InsecureTLS
		args.inheritedCredential = host.Credential
		args.inheritedAuthMethod = host.AuthMethod
		args.inheritedAccountID = host.AccountID
	}
	return resolve(ctx, args, true, true)
}

// continuousAvailableModels lists usable connections without executing stored
// API key commands. Command-backed keys are resolved only when selected.
func continuousAvailableModels(ctx context.Context, host Resolved) ([]continuous.HostModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	available := map[string]bool{host.Provider: true}
	catalog := visibleModelCatalog()
	for _, model := range catalog {
		if _, checked := available[model.Provider]; checked {
			continue
		}
		custom, ok := provider.CustomProviders()[model.Provider]
		available[model.Provider] = model.Provider == "ollama" || (ok && custom.NoAuth) || CredentialAvailable(model.Provider)
	}
	models := []continuous.HostModel{}
	for _, model := range catalog {
		if available[model.Provider] {
			models = append(models, continuous.HostModel{Provider: model.Provider, ID: model.ID, DisplayName: model.DisplayName, ContextWindow: model.ContextWindow, MaxOutput: model.MaxOutput, Reasoning: model.Reasoning})
		}
	}
	return models, ctx.Err()
}
