package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
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
// Any validation issues (bad provider id, empty model id, malformed
// JSON, negative widths) are surfaced as one warning per line on stderr;
// the well-formed entries from the rest of the file are still loaded.
func LoadUserModels() {
	models, warnings := provider.LoadUserModelsWithWarnings(UserModelsPath())
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "zot:", w)
	}
	provider.SetUserModels(models)
}

// isGatewayProvider returns true for providers whose OpenAI-compatible
// endpoint can accept routed model IDs that are not present in zot's local
// catalog. Vercel AI Gateway is intentionally not listed here: zot currently
// talks to it through the Anthropic-compatible client, which still requires
// catalog metadata for request shaping.
func isGatewayProvider(prov string) bool {
	switch prov {
	case "openrouter", "cloudflare-ai-gateway":
		return true
	default:
		return false
	}
}

// isGatewayRoutedModelID reports whether a model looks like the routed IDs
// used by gateway providers, for example "deepseek/deepseek-v4-flash".
// Non-routed typos like "deepseek-v4-flashh" should still be repaired to a
// known default instead of being silently accepted.
func isGatewayRoutedModelID(model string) bool {
	return strings.Contains(strings.TrimSpace(model), "/")
}

// ValidateAndRepairConfig checks the persisted config.json's
// (Provider, Model) pair against the active catalog and repairs any
// mismatch in-place (and on disk) before any UI renders. Three failure
// modes are handled:
//
//   - cfg.Provider is empty or unknown -> reset to "anthropic".
//   - cfg.Model is empty -> set to the provider's default.
//   - cfg.Model belongs to a different provider than cfg.Provider
//     (e.g. provider=anthropic + model=kimi-for-coding from a stale
//     half-applied switch) -> reset model to the provider's default.
//
// Gateway providers are exempt from the cross-provider model check for routed
// model IDs because those IDs can be valid even when absent from zot's catalog.
//
// Silent on success; one stderr line per repair. Errors loading or
// saving the file are non-fatal — the caller continues with defaults.
func ValidateAndRepairConfig() {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zot: config.json: %v (using defaults)\n", err)
		return
	}
	changed := false

	if cfg.Provider != "" && !isKnownProvider(cfg.Provider) {
		fmt.Fprintf(os.Stderr, "zot: config.json: unknown provider %q reset to \"anthropic\"\n", cfg.Provider)
		cfg.Provider = "anthropic"
		cfg.Model = ""
		changed = true
	}

	if cfg.Provider != "" && cfg.Model != "" {
		if _, err := provider.FindModel(cfg.Provider, cfg.Model); err != nil {
			// Gateway providers can serve routed model ids like
			// "deepseek/deepseek-v4-flash" even when the local catalog does not
			// know them. Preserve only routed ids; plain typos are still repaired.
			if cfg.Provider == "cheaperinference" || cfg.Provider == "opper" || cfg.Provider == "cloudgpu" || cfg.Provider == provider.LMStudioProviderID || isDiscoverableCustomProvider(cfg.Provider) || ((isGatewayProvider(cfg.Provider) || cfg.Provider == "runcrate") && isGatewayRoutedModelID(cfg.Model)) {
				// Local discovery is transient. Preserve the selected LM Studio
				// or discovery-enabled custom provider ID even if another
				// provider has an identically named model. Routed gateway and
				// Runcrate IDs are also valid without a catalog entry. Cheaper
				// Inference, Opper, and CloudGPU use plain IDs, which must
				// survive until discovery finishes.
			} else if m, err := provider.FindModel("", cfg.Model); err == nil {
				fix := defaultModelForProvider(cfg.Provider)
				fmt.Fprintf(os.Stderr,
					"zot: config.json: model %q belongs to provider %q (config has provider=%q); switched model to %q\n",
					cfg.Model, m.Provider, cfg.Provider, fix)
				cfg.Model = fix
				changed = true
			} else if cfg.Provider != "ollama" && cfg.Provider != provider.LlamaCPPProviderID && cfg.Provider != provider.LMStudioProviderID {
				// Model id not in any catalog. Reset to provider's default.
				fix := defaultModelForProvider(cfg.Provider)
				fmt.Fprintf(os.Stderr,
					"zot: config.json: model %q not found in the active catalog; switched to %q\n",
					cfg.Model, fix)
				cfg.Model = fix
				changed = true
			}
		}
	}

	if changed {
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "zot: config.json: failed to persist repair: %v\n", err)
		}
	}
}

// RefreshModelsAsync kicks a background discovery for every provider
// we have credentials for. Refreshed results are merged into the
// active catalog and persisted to the on-disk cache.
//
// Silent on error: discovery is a nice-to-have. Callers can still use
// the baked-in catalog if this fails.
func RefreshModelsAsync() {
	go refreshModels()
	go refreshCopilotModelAvailability()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = refreshLMStudioModels(ctx, apiKeyCommandSkip)
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = refreshLlamaCPPModels(ctx, apiKeyCommandSkip)
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = refreshCustomProviderModels(ctx, apiKeyCommandSkip)
	}()
}

// Copilot availability is account-specific and must not use the shared disk cache.
func refreshCopilotModelAvailability() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cred, _, err := resolveCredentialForBackground(ctx, "github-copilot")
	if err != nil {
		provider.SetModelAvailability("github-copilot", nil)
		return
	}
	ids, err := provider.DiscoverCopilotAvailableModels(ctx, cred)
	if err != nil {
		return // Keep the previous snapshot, or the catalog on first-load failure.
	}
	provider.SetModelAvailability("github-copilot", ids)
}

// RefreshLlamaCPPModels adds the router's currently loaded models to the
// active catalog. Unloaded models remain in the management UI and cannot be
// selected for inference until they are loaded.
func RefreshLlamaCPPModels(ctx context.Context) error {
	return refreshLlamaCPPModels(ctx, apiKeyCommandExecute)
}

func refreshLlamaCPPModels(ctx context.Context, commandMode apiKeyCommandMode) error {
	baseURL, apiKey, err := resolveLlamaCPPConfig(ctx, commandMode)
	if err != nil || baseURL == "" {
		return err
	}
	client, err := provider.NewLlamaCPPClient(baseURL, apiKey)
	if err != nil {
		return err
	}
	models, err := client.List(ctx, false)
	if err != nil {
		return err
	}
	provider.SetManagedModelsForProvider(provider.LlamaCPPProviderID, provider.LlamaCPPModels(models, client.ServerURL))
	return nil
}

func refreshModels() {
	cached, _ := provider.LoadCache(ModelCachePath())
	cacheFresh := cached.IsFresh()

	// Refresh per-key limits even while the public catalog cache is fresh.
	// Use a separate deadline so slow public discovery cannot starve this call.
	yoloCtx, yoloCancel := context.WithTimeout(context.Background(), 15*time.Second)
	var yoloModels []provider.Model
	if cred, _, err := resolveCredentialForBackground(yoloCtx, "yolo-auto"); err == nil {
		yoloModels, _ = provider.DiscoverYoloAuto(yoloCtx, cred, "")
	}
	yoloCancel()

	// The catalog is key-filtered. Refresh independently of the shared cache
	// so a fresh cache cannot hide models after login or a credential change.
	cheaperCtx, cheaperCancel := context.WithTimeout(context.Background(), 15*time.Second)
	var cheaperModels []provider.Model
	if cred, _, err := resolveCredentialForBackground(cheaperCtx, "cheaperinference"); err == nil {
		cheaperModels, _ = provider.DiscoverCheaperInference(cheaperCtx, cred, "")
	}
	cheaperCancel()

	// Opper's list is scoped to the key's policy, so it also refreshes
	// independently of the shared cache.
	opperCtx, opperCancel := context.WithTimeout(context.Background(), 15*time.Second)
	var opperModels []provider.Model
	if cred, _, err := resolveCredentialForBackground(opperCtx, "opper"); err == nil {
		opperModels, _ = provider.DiscoverOpper(opperCtx, cred, "")
	}
	opperCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var all []provider.Model
	var openrouterCred string
	var haveOpenRouter bool
	if cred, _, err := resolveCredentialForBackground(ctx, "openrouter"); err == nil {
		openrouterCred = cred
		haveOpenRouter = true
	}

	if cacheFresh {
		all = append(all, cached.Models...)
	} else {
		if cred, method, err := resolveCredentialForBackground(ctx, "anthropic"); err == nil && method == "apikey" {
			// /v1/models on Anthropic is API-key only; OAuth tokens can
			// also list models via the bearer header, but we skip OAuth
			// here to avoid surprise rate-limit hits on subscription keys.
			if live, err := provider.DiscoverAnthropic(ctx, cred, ""); err == nil {
				all = append(all, live...)
			}
		}
		if cred, method, err := resolveCredentialForBackground(ctx, "openai"); err == nil && method == "apikey" {
			if live, err := provider.DiscoverOpenAI(ctx, cred, ""); err == nil {
				all = append(all, live...)
			}
		}
		if cred, method, err := resolveCredentialForBackground(ctx, "kimi"); err == nil && method == "apikey" {
			if live, err := provider.DiscoverOpenAI(ctx, cred, "https://api.kimi.com/coding/v1"); err == nil {
				for i := range live {
					live[i].Provider = "kimi"
					live[i].Source = "live"
				}
				all = append(all, live...)
			}
		}
		if cred, method, err := resolveCredentialForBackground(ctx, "google"); err == nil && method == "apikey" {
			if live, err := provider.DiscoverGoogle(ctx, cred, ""); err == nil {
				all = append(all, live...)
			}
		}
		// Bedrock resolves SigV4 credentials independently of inference auth.
		// A default AWS CLI profile can be usable even when zot has no
		// Bedrock credential registered with its general resolver.
		if live, err := provider.DiscoverBedrock(ctx, ""); err == nil {
			all = append(all, live...)
		}
		if haveOpenRouter {
			// /models is public; gate on a credential so the picker only
			// fills with OpenRouter's hundreds of routes for users who use it.
			if live, err := provider.DiscoverOpenRouter(ctx, ""); err == nil {
				all = append(all, live...)
			}
		}
		if _, _, err := resolveCredentialForBackground(ctx, "gondola"); err == nil {
			// Gondola's catalog is also public. Gate discovery on a credential so
			// its text models only fill the picker for users of the provider.
			if live, err := provider.DiscoverGondola(ctx, ""); err == nil {
				all = append(all, live...)
			}
		}
	}

	// A fresh shared cache may predate Runcrate login. Discover its public
	// catalog when missing, without refreshing unrelated providers or their TTL.
	if !cacheFresh || !slices.ContainsFunc(cached.Models, func(m provider.Model) bool { return m.Provider == "runcrate" }) {
		if _, _, err := resolveCredentialForBackground(ctx, "runcrate"); err == nil {
			if live, err := provider.DiscoverRuncrate(ctx, ""); err == nil {
				all = append(all, live...)
			}
		}
	}

	// A fresh shared cache may predate CloudGPU login. Discover its public
	// catalog when missing, without refreshing unrelated providers or their TTL.
	if !cacheFresh || !slices.ContainsFunc(cached.Models, func(m provider.Model) bool { return m.Provider == "cloudgpu" }) {
		if _, _, err := resolveCredentialForBackground(ctx, "cloudgpu"); err == nil {
			if live, err := provider.DiscoverCloudGPU(ctx, ""); err == nil {
				all = append(all, live...)
			}
		}
	}

	// Presets are per-account and require auth. Fetch them even when the
	// public /models cache is still fresh so @preset/{slug} shows up in
	// the picker without waiting out CacheTTL.
	if haveOpenRouter {
		if presets, err := provider.DiscoverOpenRouterPresets(ctx, openrouterCred, ""); err == nil {
			all = mergeOpenRouterPresets(all, presets)
		}
	}

	// Do not carry forward another key's models or limits, even on failure.
	all = mergeYoloAutoModels(all, yoloModels)
	all = mergeCheaperInferenceModels(all, cheaperModels)
	all = mergeOpperModels(all, opperModels)
	provider.SetLiveModels(all)
	// Keep explicit metadata overrides above refreshed server catalogs.
	LoadUserModels()
	if len(all) == 0 {
		// Clear old account entries without marking an empty catalog fresh.
		_ = provider.SaveCache(ModelCachePath(), provider.ModelCache{})
		return
	}
	_ = provider.SaveCache(ModelCachePath(), provider.ModelCache{
		FetchedAt: modelCacheFetchedAt(cached, !cacheFresh, time.Now().UTC()),
		Models:    all,
	})
}

// mergeYoloAutoModels replaces account metadata without disturbing the public
// catalog. Cached entries aid offline resolution, but never suppress discovery.
func mergeYoloAutoModels(existing, live []provider.Model) []provider.Model {
	out := make([]provider.Model, 0, len(existing)+len(live))
	for _, model := range existing {
		if model.Provider != "yolo-auto" {
			out = append(out, model)
		}
	}
	return append(out, live...)
}

// mergeCheaperInferenceModels drops cached account entries even on discovery
// failure or logout. The static seed remains available as an offline fallback.
func mergeCheaperInferenceModels(existing, live []provider.Model) []provider.Model {
	out := make([]provider.Model, 0, len(existing)+len(live))
	for _, model := range existing {
		if model.Provider != "cheaperinference" {
			out = append(out, model)
		}
	}
	return append(out, live...)
}

// mergeOpperModels drops cached Opper entries even on discovery failure or
// logout. The static seed remains available as an offline fallback.
func mergeOpperModels(existing, live []provider.Model) []provider.Model {
	out := make([]provider.Model, 0, len(existing)+len(live))
	for _, model := range existing {
		if model.Provider != "opper" {
			out = append(out, model)
		}
	}
	return append(out, live...)
}

func modelCacheFetchedAt(cached provider.ModelCache, catalogRefreshed bool, now time.Time) time.Time {
	if catalogRefreshed {
		return now
	}
	return cached.FetchedAt
}

// mergeOpenRouterPresets replaces any previously cached OpenRouter preset
// models with the freshly listed set, keeping every non-preset entry.
func mergeOpenRouterPresets(existing, presets []provider.Model) []provider.Model {
	out := make([]provider.Model, 0, len(existing)+len(presets))
	for _, m := range existing {
		if m.Provider == "openrouter" && strings.HasPrefix(m.ID, "@preset/") {
			continue
		}
		out = append(out, m)
	}
	return append(out, presets...)
}
