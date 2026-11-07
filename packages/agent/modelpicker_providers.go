package agent

import "github.com/patriceckhart/zot/packages/provider"

// modelPickerProviders returns providers whose models can be selected without
// prompting for credentials. Keyless custom endpoints need a configured URL.
func modelPickerProviders() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range knownProviders {
		if CredentialAvailable(p) && !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	for p, cfg := range provider.CustomProviders() {
		if seen[p] || CheckProviderEnabled(p) != nil {
			continue
		}
		if CredentialAvailable(p) || (!isBuiltinProvider(p) && (cfg.BaseURL != "" || customProviderHasModelURL(p))) {
			out = append(out, p)
			seen[p] = true
		}
	}
	if !seen["ollama"] && CheckProviderEnabled("ollama") == nil {
		out = append(out, "ollama")
	}
	return out
}

func customProviderHasModelURL(name string) bool {
	for _, m := range provider.ModelsForProvider(name) {
		if m.Source == "user" && m.BaseURL != "" {
			return true
		}
	}
	return false
}
