package provider

// Hand-curated catalog supplements that the bulk builtin catalog
// (catalog_builtin.go) doesn't cover.
//
//   - openai-responses: zot-specific provider id for the public OpenAI
//     Responses API (api.openai.com/v1/responses). Separate from
//     `openai` (Chat Completions) and `openai-codex` (ChatGPT subscription).
//   - kimi/kimi-k2-thinking: matches the Anthropic-Messages endpoint
//     at api.kimi.com/coding; not present in the bulk builtin file
//     because the `kimi` provider lives in models.go alongside the
//     other curated providers.

func init() { Catalog = append(Catalog, supplementCatalog...) }

var supplementCatalog = []Model{
	// ----- kimi (anthropic-messages on api.kimi.com/coding) -----
	{Provider: "kimi", ID: "kimi-k2-thinking", DisplayName: "Kimi K2 Thinking",
		ContextWindow: 262144, MaxOutput: 32000, Reasoning: true,
		BaseURL: "https://api.kimi.com/coding"},
	{Provider: "kimi", ID: "k3", DisplayName: "Kimi K3",
		ContextWindow: 262144, MaxOutput: 131072, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3,
		BaseURL: "https://api.kimi.com/coding"},

	// ----- Kimi K3 on OpenAI-compatible providers -----
	{Provider: "moonshotai", ID: "kimi-k3", DisplayName: "Kimi K3",
		ContextWindow: 262144, MaxOutput: 131072, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3,
		BaseURL: "https://api.moonshot.ai/v1"},
	{Provider: "moonshotai-cn", ID: "kimi-k3", DisplayName: "Kimi K3",
		ContextWindow: 262144, MaxOutput: 131072, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3,
		BaseURL: "https://api.moonshot.cn/v1"},
	{Provider: "openrouter", ID: "moonshotai/kimi-k3", DisplayName: "Kimi K3",
		ContextWindow: 262144, MaxOutput: 131072, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3,
		BaseURL: "https://openrouter.ai/api/v1"},
	{Provider: "vercel-ai-gateway", ID: "moonshotai/kimi-k3", DisplayName: "Kimi K3",
		ContextWindow: 262144, MaxOutput: 131072, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3,
		BaseURL: "https://ai-gateway.vercel.sh"},

	// ----- xAI Responses API -----
	{Provider: "xai", ID: "grok-4.5", DisplayName: "Grok 4.5", API: APIResponses,
		ContextWindow: 2000000, MaxOutput: 30000, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 2.5, PriceCacheRead: 0.2,
		BaseURL: "https://api.x.ai/v1"},

	// ----- openai-responses (public OpenAI Responses API) -----
	{Provider: "openai-responses", ID: "gpt-6.1-sol", DisplayName: "GPT-6.1 Sol (Responses)", API: APIResponses,
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2, PriceOutput: 10, PriceCacheRead: 0.1, PriceCacheWrite: 2.5,
		PriceTierInputTokens: 272000,
		PriceInputAbove:      4, PriceOutputAbove: 15, PriceCacheReadAbove: 0.2, PriceCacheWriteAbove: 5},
	{Provider: "openai-responses", ID: "gpt-6-astra", DisplayName: "GPT-6 Astra (Responses)", API: APIResponses,
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 10, PriceOutput: 50, PriceCacheRead: 1, PriceCacheWrite: 12.5,
		PriceTierInputTokens: 272000,
		PriceInputAbove:      20, PriceOutputAbove: 75, PriceCacheReadAbove: 2, PriceCacheWriteAbove: 25},
	{Provider: "openai-responses", ID: "gpt-6-sol", DisplayName: "GPT-6 Sol (Responses)", API: APIResponses,
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2, PriceOutput: 10, PriceCacheRead: 0.2, PriceCacheWrite: 2.5,
		PriceTierInputTokens: 272000,
		PriceInputAbove:      4, PriceOutputAbove: 15, PriceCacheReadAbove: 0.4, PriceCacheWriteAbove: 5},
	{Provider: "openai-responses", ID: "gpt-6-luna", DisplayName: "GPT-6 Luna (Responses)", API: APIResponses,
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.1, PriceOutput: 0.5, PriceCacheRead: 0.01, PriceCacheWrite: 0.125,
		PriceTierInputTokens: 272000,
		PriceInputAbove:      0.2, PriceOutputAbove: 0.75, PriceCacheReadAbove: 0.02, PriceCacheWriteAbove: 0.25},
	{Provider: "openai-responses", ID: "gpt-5", DisplayName: "GPT-5 (Responses)",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 10.00, PriceCacheRead: 0.125},
	{Provider: "openai-responses", ID: "gpt-5-mini", DisplayName: "GPT-5 mini (Responses)",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.25, PriceOutput: 2.00, PriceCacheRead: 0.025},
	{Provider: "openai-responses", ID: "gpt-5-nano", DisplayName: "GPT-5 nano (Responses)",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.05, PriceOutput: 0.40, PriceCacheRead: 0.005},
	{Provider: "openai-responses", ID: "gpt-5-codex", DisplayName: "GPT-5 Codex (Responses)",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 10.00, PriceCacheRead: 0.125},
	{Provider: "openai-responses", ID: "o3", DisplayName: "o3 (Responses)",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 2.00, PriceOutput: 8.00, PriceCacheRead: 0.50},
	{Provider: "openai-responses", ID: "o4-mini", DisplayName: "o4-mini (Responses)",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 1.10, PriceOutput: 4.40, PriceCacheRead: 0.275},
}
