package agent

import (
	"fmt"

	"github.com/patriceckhart/zot/packages/provider"
)

func (c Config) providerDisabled(name string) bool {
	name = canonicalProvider(name)
	for _, disabled := range c.DisabledProviders {
		if canonicalProvider(disabled) == name {
			return true
		}
	}
	return false
}

// CheckProviderEnabled rejects providers explicitly disabled in config.json.
// It does not inspect or resolve credentials.
func CheckProviderEnabled(name string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if cfg.providerDisabled(name) {
		return fmt.Errorf("provider %q is disabled in config.json (remove it from disabled_providers to enable it)", canonicalProvider(name))
	}
	return nil
}

func (c Config) visibleModels(models []provider.Model) []provider.Model {
	out := make([]provider.Model, 0, len(models))
	for _, model := range models {
		if c.providerDisabled(model.Provider) || (c.HideSpeculativeModels && model.Speculative) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// visibleModelCatalog filters a snapshot without changing the provider catalog.
// Explicit model lookup and saved model metadata remain available.
func visibleModelCatalog() []provider.Model {
	cfg, err := LoadConfig()
	if err != nil {
		return nil
	}
	return cfg.visibleModels(provider.Active())
}
