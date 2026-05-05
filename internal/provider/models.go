package provider

import (
	"fmt"
	"sync"
)

// Model describes a single LLM we know about.
type Model struct {
	Provider      string // "anthropic" | "openai"
	ID            string // API id
	DisplayName   string
	ContextWindow int
	MaxOutput     int
	Reasoning     bool // supports reasoning/thinking

	// Prices are USD per 1M tokens.
	PriceInput      float64
	PriceOutput     float64
	PriceCacheRead  float64
	PriceCacheWrite float64

	// Speculative marks models whose ids are known from the upstream
	// vendor's CLI but not yet live on their public API. They'll 404
	// today but start working the moment the provider flips the switch.
	Speculative bool

	// BaseURL overrides the provider's default API endpoint for this
	// model. Optional; when empty the provider's default (or the
	// --base-url flag) is used. Useful for local models served by
	// ollama, vLLM, LM Studio, etc.
	BaseURL string

	// Source is where this model entry came from: "catalog" (baked in),
	// "live" (discovered via /v1/models), or "cache" (loaded from the
	// on-disk cache). Informational.
	Source string
}

// Catalog is the hardcoded, read-only list of supported models.
// Prices are USD per 1M tokens. The list is curated to what zot's
// clients (Anthropic Messages + OpenAI Chat Completions) can actually
// talk to; models that are only reachable through the OpenAI Responses
// API (o1-pro, o3-pro, gpt-5-pro) are omitted.
var Catalog = []Model{
	// ---- Anthropic / Claude 4.x ----
	{
		Provider: "anthropic", ID: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3.00, PriceOutput: 15.00, PriceCacheRead: 0.30, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-1", DisplayName: "Claude Opus 4.1",
		ContextWindow: 200000, MaxOutput: 32000, Reasoning: true,
		PriceInput: 15.00, PriceOutput: 75.00, PriceCacheRead: 1.50, PriceCacheWrite: 18.75,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-0", DisplayName: "Claude Opus 4",
		ContextWindow: 200000, MaxOutput: 32000, Reasoning: true,
		PriceInput: 15.00, PriceOutput: 75.00, PriceCacheRead: 1.50, PriceCacheWrite: 18.75,
	},
	{
		Provider: "anthropic", ID: "claude-sonnet-4-0", DisplayName: "Claude Sonnet 4",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3.00, PriceOutput: 15.00, PriceCacheRead: 0.30, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 1.00, PriceOutput: 5.00, PriceCacheRead: 0.10, PriceCacheWrite: 1.25,
	},

	// ---- Anthropic / Claude 3.x (legacy) ----
	{
		Provider: "anthropic", ID: "claude-3-7-sonnet-20250219", DisplayName: "Claude Sonnet 3.7",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3.00, PriceOutput: 15.00, PriceCacheRead: 0.30, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude Sonnet 3.5 v2",
		ContextWindow: 200000, MaxOutput: 8192, Reasoning: false,
		PriceInput: 3.00, PriceOutput: 15.00, PriceCacheRead: 0.30, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-3-5-haiku-latest", DisplayName: "Claude Haiku 3.5",
		ContextWindow: 200000, MaxOutput: 8192, Reasoning: false,
		PriceInput: 0.80, PriceOutput: 4.00, PriceCacheRead: 0.08, PriceCacheWrite: 1.00,
	},
	{
		Provider: "anthropic", ID: "claude-3-opus-20240229", DisplayName: "Claude Opus 3",
		ContextWindow: 200000, MaxOutput: 4096, Reasoning: false,
		PriceInput: 15.00, PriceOutput: 75.00, PriceCacheRead: 1.50, PriceCacheWrite: 18.75,
	},

	// ---- Kimi / Kimi Code ----
	{
		Provider: "kimi", ID: "kimi-for-coding", DisplayName: "Kimi-k2.6",
		ContextWindow: 262144, MaxOutput: 32000, Reasoning: true,
		BaseURL: "https://api.kimi.com/coding/v1",
	},

	// ---- OpenAI / GPT-5 family ----
	{
		Provider: "openai", ID: "gpt-5", DisplayName: "GPT-5",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 10.00, PriceCacheRead: 0.125,
	},
	{
		Provider: "openai", ID: "gpt-5-mini", DisplayName: "GPT-5 mini",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.25, PriceOutput: 2.00, PriceCacheRead: 0.025,
	},
	{
		Provider: "openai", ID: "gpt-5-nano", DisplayName: "GPT-5 nano",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.05, PriceOutput: 0.40, PriceCacheRead: 0.005,
	},

	// ---- OpenAI / GPT-4.1 family ----
	{
		Provider: "openai", ID: "gpt-4.1", DisplayName: "GPT-4.1",
		ContextWindow: 1047576, MaxOutput: 32768, Reasoning: false,
		PriceInput: 2.00, PriceOutput: 8.00, PriceCacheRead: 0.50,
	},
	{
		Provider: "openai", ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 mini",
		ContextWindow: 1047576, MaxOutput: 32768, Reasoning: false,
		PriceInput: 0.40, PriceOutput: 1.60, PriceCacheRead: 0.10,
	},
	{
		Provider: "openai", ID: "gpt-4.1-nano", DisplayName: "GPT-4.1 nano",
		ContextWindow: 1047576, MaxOutput: 32768, Reasoning: false,
		PriceInput: 0.10, PriceOutput: 0.40, PriceCacheRead: 0.03,
	},

	// ---- OpenAI / GPT-4o family ----
	{
		Provider: "openai", ID: "gpt-4o", DisplayName: "GPT-4o",
		ContextWindow: 128000, MaxOutput: 16384, Reasoning: false,
		PriceInput: 2.50, PriceOutput: 10.00, PriceCacheRead: 1.25,
	},
	{
		Provider: "openai", ID: "gpt-4o-mini", DisplayName: "GPT-4o mini",
		ContextWindow: 128000, MaxOutput: 16384, Reasoning: false,
		PriceInput: 0.15, PriceOutput: 0.60, PriceCacheRead: 0.08,
	},

	// ---- OpenAI / reasoning models ----
	{
		Provider: "openai", ID: "o4-mini", DisplayName: "o4-mini",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 1.10, PriceOutput: 4.40, PriceCacheRead: 0.275,
	},
	{
		Provider: "openai", ID: "o3", DisplayName: "o3",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 2.00, PriceOutput: 8.00, PriceCacheRead: 0.50,
	},
	{
		Provider: "openai", ID: "o3-mini", DisplayName: "o3-mini",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 1.10, PriceOutput: 4.40, PriceCacheRead: 0.55,
	},
	{
		Provider: "openai", ID: "o1", DisplayName: "o1",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 15.00, PriceOutput: 60.00, PriceCacheRead: 7.50,
	},

	// ---- Speculative: Anthropic ----
	{
		Provider: "anthropic", ID: "claude-opus-4-5", DisplayName: "Claude Opus 4.5",
		// 200k ctx / 64k maxOutput per Anthropic's published sizing
		// for the opus-4-5 family; the 1M context is a 4.6+ thing.
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 5.00, PriceOutput: 25.00, PriceCacheRead: 0.50, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 5.00, PriceOutput: 25.00, PriceCacheRead: 0.50, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 5.00, PriceOutput: 25.00, PriceCacheRead: 0.50, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6",
		ContextWindow: 1000000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3.00, PriceOutput: 15.00, PriceCacheRead: 0.30, PriceCacheWrite: 3.75,
		Speculative: true,
	},

	// ---- Speculative: OpenAI ----
	// Context windows on the OpenAI gpt-5.x family differ by route:
	// the direct API advertises 400k, the ChatGPT Codex OAuth backend
	// caps at 272k. zot serves both auth modes from one catalog row
	// per id, so we pin to the smaller number to keep the context-usage
	// meter honest under subscription auth. Users on the direct API
	// simply see a conservative headroom estimate.
	{
		Provider: "openai", ID: "gpt-5.1", DisplayName: "GPT-5.1",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 10.00, PriceCacheRead: 0.125,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.2", DisplayName: "GPT-5.2",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.75, PriceOutput: 14.00, PriceCacheRead: 0.175,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.3", DisplayName: "GPT-5.3",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.75, PriceOutput: 14.00, PriceCacheRead: 0.175,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.4", DisplayName: "GPT-5.4",
		// ContextWindow: 272k across every route we support (OpenAI
		// direct API and the ChatGPT Codex OAuth backend).
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2.50, PriceOutput: 15.00, PriceCacheRead: 0.25,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 mini",
		// ContextWindow: 400k on the OpenAI direct API, 272k on the
		// ChatGPT Codex OAuth backend. We pin to the smaller Codex
		// cap so the context-usage meter is honest under subscription
		// auth; direct-API users simply see a conservative headroom
		// estimate rather than an inflated one.
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.75, PriceOutput: 4.50, PriceCacheRead: 0.075,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.5", DisplayName: "GPT-5.5",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2.50, PriceOutput: 15.00, PriceCacheRead: 0.25,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.5-mini", DisplayName: "GPT-5.5 mini",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.75, PriceOutput: 4.50, PriceCacheRead: 0.075,
		Speculative: true,
	},
}

// DefaultModel is used when the user does not specify one.
var DefaultModel = Catalog[0] // claude-sonnet-4-5

// ----- active (merged) catalog -----
//
// Callers should use Active() / FindModel / ModelsForProvider for
// lookups. They return the baked-in Catalog merged with any live
// models loaded via SetLiveModels.

var (
	activeMu sync.RWMutex
	active   []Model = Catalog // default: just the static catalog
)

// SetLiveModels replaces the "live" overlay used by the active catalog.
// Typically called after a successful /v1/models discovery or on load
// from the on-disk cache.
func SetLiveModels(live []Model) {
	activeMu.Lock()
	defer activeMu.Unlock()
	if len(live) == 0 {
		active = Catalog
		return
	}
	active = MergeCatalog(live)
}

// Active returns the current merged catalog.
func Active() []Model {
	activeMu.RLock()
	defer activeMu.RUnlock()
	out := make([]Model, len(active))
	copy(out, active)
	return out
}

// FindModel returns a Model by id, optionally constrained by provider.
// If provider is empty, the first matching id is returned. Looks up
// against the merged active catalog.
func FindModel(provider, id string) (Model, error) {
	for _, m := range Active() {
		if m.ID == id && (provider == "" || m.Provider == provider) {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("unknown model %q (provider=%q)", id, provider)
}

// ModelsForProvider returns all models for the given provider, from the
// merged active catalog.
func ModelsForProvider(provider string) []Model {
	var out []Model
	for _, m := range Active() {
		if m.Provider == provider {
			out = append(out, m)
		}
	}
	return out
}

// ComputeCost returns the USD cost for the given usage on model m.
func ComputeCost(m Model, u Usage) float64 {
	const per = 1_000_000.0
	return float64(u.InputTokens)*m.PriceInput/per +
		float64(u.OutputTokens)*m.PriceOutput/per +
		float64(u.CacheReadTokens)*m.PriceCacheRead/per +
		float64(u.CacheWriteTokens)*m.PriceCacheWrite/per
}
