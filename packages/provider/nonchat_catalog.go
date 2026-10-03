package provider

// Auxiliary catalog snapshot, verified against the public provider listings on
// 2026-10-02. It is deliberately separate from selectable chat models.
func builtinNonChatModels() []NonChatModel {
	models := []NonChatModel{
		{Model: Model{Provider: "typesafe", ID: "jev-latest", DisplayName: "Jev", API: "typesafe-system-one", BaseURL: "https://api.typesafe.ai/v1", ContextWindow: 64000}, Type: "classifier", Input: []string{"text"}},
		{Model: Model{Provider: "opencode", ID: "jev-1.13", DisplayName: "Jev 1.13", API: "typesafe-system-one", BaseURL: "https://opencode.ai/zen/v1", ContextWindow: 32000, PriceInput: 0.042}, Type: "classifier", Input: []string{"text"}},
		{Model: Model{Provider: "opencode", ID: "jev-1.13-free", DisplayName: "Jev 1.13 Free", API: "typesafe-system-one", BaseURL: "https://opencode.ai/zen/v1", ContextWindow: 32000}, Type: "classifier", Input: []string{"text"}},
		{Model: Model{Provider: "cloudflare-workers-ai", ID: "typesafe/jev", DisplayName: "Jev", API: "cloudflare-workers-ai-system-one", BaseURL: "https://api.cloudflare.com/client/v4/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai", ContextWindow: 32000}, Type: "classifier", Input: []string{"text"}},
	}
	for _, entry := range []struct {
		id, name string
		context  int
		price    float64
	}{
		{"convaiinnovations/laya", "Laya", 8192, 0},
		{"convaiinnovations/laya-free", "Laya (Free)", 8192, 0},
		{"liquid/d1", "Liquid d1", 65536, 0.04},
		{"typesafe-ai/jev", "Jev", 32000, 0.042},
	} {
		models = append(models, NonChatModel{Model: Model{Provider: "vercel-ai-gateway", ID: entry.id, DisplayName: entry.name, API: "typesafe-system-one", BaseURL: "https://ai-gateway.vercel.sh/typesafe/v1", ContextWindow: entry.context, PriceInput: entry.price}, Type: "classifier", Input: []string{"text"}})
	}
	for _, entry := range []struct {
		id, name     string
		context      int
		price, cache float64
	}{
		{"liquid/d1", "LiquidAI: D1", 65536, 0.04, 0.04},
		{"togethercomputer/tev1-4b-experimental", "Together: Tev1 4B Experimental", 32768, 0.042, 0},
		{"inception/mercury-decide:free", "Inception: Mercury Decide (free)", 32768, 0, 0},
		{"upstage/solar-decide", "Upstage: Solar Decide", 524288, 0.05, 0.05},
		{"respan/span-01", "Respan: Span-01", 0, 0.02, 0},
		{"respan/span-01-lite", "Respan: Span-01 Lite", 0, 0, 0},
		{"respan/span-01-lite:free", "Respan: Span-01 Lite (free)", 0, 0, 0},
		{"jaredpalmer/kev-4b", "Jared Palmer: Kev 4B", 8192, 0.042, 0},
		{"~typesafe/jev-latest", "TypeSafe: Jev Latest", 32000, 0.042, 0},
		{"typesafe/jev-1.13", "TypeSafe: Jev 1.13", 32000, 0.042, 0},
	} {
		models = append(models, NonChatModel{Model: Model{Provider: "openrouter", ID: entry.id, DisplayName: entry.name, API: "typesafe-system-one", BaseURL: openrouterDefaultBaseURL, ContextWindow: entry.context, PriceInput: entry.price, PriceCacheRead: entry.cache}, Type: "classifier", Input: []string{"text"}})
	}
	for _, entry := range []struct {
		id, name                    string
		context                     int
		input, output, cache, write float64
		textOnly, mixed             bool
	}{
		{id: "bytedance-seed/seedream-5-0-flash", name: "ByteDance Seed: Seedream 5.0 Flash"},
		{id: "black-forest-labs/flux-3-image", name: "Black Forest Labs: FLUX.3 Image", context: 46864},
		{id: "inclusionai/ming-image-0.1-design-layer", name: "inclusionAI: Ming Image 0.1 Design Layer"},
		{id: "recraft/recraft-v4.1-flash", name: "Recraft: Recraft V4.1 Flash", context: 65536, textOnly: true},
		{id: "inclusionai/ming-image-0.1-design", name: "inclusionAI: Ming Image 0.1 Design", textOnly: true},
		{id: "openai/gpt-image-2.5-sunburst", name: "OpenAI: GPT Image 2.5 Sunburst", context: 400000, input: 8, output: 8, cache: 2},
		{id: "openai/gpt-image-2.5-flare", name: "OpenAI: GPT Image 2.5 Flare", context: 400000, input: 8, output: 8, cache: 2},
		{id: "microsoft/mai-image-2.6", name: "Microsoft AI: MAI-Image-2.6", context: 4096, input: 5},
		{id: "microsoft/mai-image-2.6-flash", name: "Microsoft AI: MAI-Image-2.6 Flash", context: 4096, input: 1.75},
		{id: "meta/muse-image", name: "Meta: Muse Image", context: 65536},
		{id: "recraft/recraft-v4-styles-pro", name: "Recraft: Recraft V4 Styles Pro", context: 65536},
		{id: "recraft/recraft-v4-styles-vector", name: "Recraft: Recraft V4 Styles Vector", context: 65536},
		{id: "recraft/recraft-v4-styles-pro-vector", name: "Recraft: Recraft V4 Styles Pro Vector", context: 65536},
		{id: "recraft/recraft-v4-styles", name: "Recraft: Recraft V4 Styles", context: 65536},
		{id: "bytedance-seed/seedream-5-0-lite", name: "ByteDance Seed: Seedream 5.0 Lite"},
		{id: "bytedance-seed/seedream-5-0-pro", name: "ByteDance Seed: Seedream 5.0 Pro"},
		{id: "x-ai/grok-imagine-image-2.0", name: "xAI: Grok Imagine Image 2.0", context: 65536},
		{id: "qwen/qwen-image-3", name: "Qwen: Qwen Image 3", context: 65536},
		{id: "qwen/qwen-image-3-pro", name: "Qwen: Qwen Image 3 Pro", context: 65536},
		{id: "microsoft/mai-image-2.5-pro", name: "Microsoft AI: MAI-Image-2.5 Pro", context: 4096, input: 5},
		{id: "krea/krea-2-large", name: "Krea: Krea 2 Large", context: 65536},
		{id: "krea/krea-2-medium", name: "Krea: Krea 2 Medium", context: 65536},
		{id: "krea/krea-2-medium-turbo", name: "Krea: Krea 2 Medium Turbo", context: 65536},
		{id: "openrouter/auto-beta", name: "Auto Router (Beta)", context: 2000000, mixed: true},
		{id: "google/gemini-3.1-flash-lite-image", name: "Google: Nano Banana 2 Lite (Gemini 3.1 Flash Lite Image)", context: 65536, input: 0.25, output: 1.5, mixed: true},
		{id: "openai/gpt-image-2", name: "OpenAI: GPT Image 2", context: 400000, input: 8, output: 8, cache: 2},
		{id: "openai/gpt-image-1", name: "OpenAI: GPT Image 1", context: 400000, input: 10, output: 10, cache: 1.25},
		{id: "openai/gpt-image-1-mini", name: "OpenAI: GPT Image 1 Mini", context: 400000, input: 2.5, output: 2.5, cache: 0.25},
		{id: "google/gemini-3.1-flash-image", name: "Google: Nano Banana 2 (Gemini 3.1 Flash Image)", context: 131072, input: 0.5, output: 3, mixed: true},
		{id: "google/gemini-3-pro-image", name: "Google: Nano Banana Pro (Gemini 3 Pro Image)", context: 131072, input: 2, output: 12, cache: 0.2, write: 0.375, mixed: true},
		{id: "sourceful/riverflow-v2.5-pro", name: "Sourceful: Riverflow V2.5 Pro", context: 32768},
		{id: "sourceful/riverflow-v2.5-fast", name: "Sourceful: Riverflow V2.5 Fast", context: 32768},
		{id: "microsoft/mai-image-2.5", name: "Microsoft AI: MAI-Image-2.5", context: 4096, input: 5},
		{id: "x-ai/grok-imagine-image-quality", name: "SpaceXAI: Grok Imagine Image Quality", context: 65536},
		{id: "recraft/recraft-v4.1-pro-vector", name: "Recraft: Recraft V4.1 Pro Vector", context: 65536},
		{id: "recraft/recraft-v4.1-vector", name: "Recraft: Recraft V4.1 Vector", context: 65536},
		{id: "recraft/recraft-v4.1-utility-pro", name: "Recraft: Recraft V4.1 Utility Pro", context: 65536},
		{id: "recraft/recraft-v4.1-utility", name: "Recraft: Recraft V4.1 Utility", context: 65536},
		{id: "recraft/recraft-v4.1-pro", name: "Recraft: Recraft V4.1 Pro", context: 65536},
		{id: "recraft/recraft-v4.1", name: "Recraft: Recraft V4.1", context: 65536},
		{id: "recraft/recraft-v4-pro-vector", name: "Recraft: Recraft V4 Pro Vector", context: 65536},
		{id: "recraft/recraft-v4-vector", name: "Recraft: Recraft V4 Vector", context: 65536},
		{id: "recraft/recraft-v4-pro", name: "Recraft: Recraft V4 Pro", context: 65536},
		{id: "recraft/recraft-v4", name: "Recraft: Recraft V4", context: 65536},
		{id: "recraft/recraft-v3", name: "Recraft: Recraft V3", context: 65536},
		{id: "openai/gpt-5.4-image-2", name: "OpenAI: GPT-5.4 Image 2", context: 272000, input: 8, output: 15, cache: 2, mixed: true},
		{id: "google/gemini-3.1-flash-image-preview", name: "Google: Nano Banana 2 (Gemini 3.1 Flash Image Preview)", context: 65536, input: 0.5, output: 3, mixed: true},
		{id: "sourceful/riverflow-v2-pro", name: "Sourceful: Riverflow V2 Pro", context: 8192},
		{id: "sourceful/riverflow-v2-fast", name: "Sourceful: Riverflow V2 Fast", context: 8192},
		{id: "black-forest-labs/flux.2-klein-4b", name: "Black Forest Labs: FLUX.2 Klein 4B", context: 40960},
		{id: "bytedance-seed/seedream-4.5", name: "ByteDance Seed: Seedream 4.5", context: 4096},
		{id: "black-forest-labs/flux.2-max", name: "Black Forest Labs: FLUX.2 Max", context: 46864},
		{id: "black-forest-labs/flux.2-flex", name: "Black Forest Labs: FLUX.2 Flex", context: 67344},
		{id: "black-forest-labs/flux.2-pro", name: "Black Forest Labs: FLUX.2 Pro", context: 46864},
		{id: "google/gemini-3-pro-image-preview", name: "Google: Nano Banana Pro (Gemini 3 Pro Image Preview)", context: 65536, input: 2, output: 12, cache: 0.2, write: 0.375, mixed: true},
		{id: "openai/gpt-5-image-mini", name: "OpenAI: GPT-5 Image Mini", context: 400000, input: 2.5, output: 2, cache: 0.25, mixed: true},
		{id: "openai/gpt-5-image", name: "OpenAI: GPT-5 Image", context: 400000, input: 10, output: 10, cache: 1.25, mixed: true},
		{id: "google/gemini-2.5-flash-image", name: "Google: Nano Banana (Gemini 2.5 Flash Image)", context: 32768, input: 0.3, output: 2.5, cache: 0.03, write: 0.0833333333333333, mixed: true},
		{id: "openrouter/auto", name: "Auto Router", context: 2000000, mixed: true},
	} {
		input := []string{"text", "image"}
		if entry.textOnly {
			input = []string{"text"}
		}
		output := []string{"image"}
		if entry.mixed {
			output = append(output, "text")
		}
		models = append(models, NonChatModel{Model: Model{Provider: "openrouter", ID: entry.id, DisplayName: entry.name, API: "openrouter-images", BaseURL: openrouterDefaultBaseURL, ContextWindow: entry.context, PriceInput: entry.input, PriceOutput: entry.output, PriceCacheRead: entry.cache, PriceCacheWrite: entry.write}, Type: "image", Input: input, Output: output})
	}
	return models
}
