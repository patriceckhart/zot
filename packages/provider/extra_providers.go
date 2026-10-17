package provider

// Extra third-party providers.
//
// Most are OpenAI Chat Completions–compatible, so they reuse `openaiClient`
// with a different name + base URL. A handful speak the Anthropic Messages
// API and reuse `anthropicClient` via NewAnthropicCompat below.
//
// Providers with a non-trivial protocol (Bedrock Converse, Vertex SSE, Azure
// Responses, Mistral Conversations) are stubbed so the host wiring compiles.
// Filling those in is its own work — Bedrock needs SigV4 + Converse stream
// parsing, Vertex needs ADC, Azure needs the Responses API shape, Mistral
// needs its bespoke Conversations API.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// ----------------------------------------------------------------------
// OpenAI Chat Completions–compatible providers. All of these just route
// Chat Completions to a different base URL with a different name. The
// default Authorization: Bearer header from openaiClient is correct for
// every one of them.
// ----------------------------------------------------------------------

// NewOpenAICompat is the shared constructor for OpenAI-completions clones.
// Exported so build.go can instantiate clients for user-defined providers.
func NewOpenAICompat(name, apiKey, baseURL, fallbackBaseURL string) Client {
	if baseURL == "" {
		baseURL = fallbackBaseURL
	}
	return &openaiClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		name:    name,
		http:    &http.Client{Timeout: 0},
	}
}

// NewMoonshot is the global Moonshot AI endpoint (Kimi-K2 family by id).
// Provider id is `moonshotai`.
func NewMoonshot(apiKey, baseURL string) Client {
	return NewOpenAICompat("moonshotai", apiKey, baseURL, "https://api.moonshot.ai/v1")
}

// NewMoonshotCN is the China-region Moonshot endpoint. Same model ids as
// the global flavor, different base URL.
func NewMoonshotCN(apiKey, baseURL string) Client {
	return NewOpenAICompat("moonshotai-cn", apiKey, baseURL, "https://api.moonshot.cn/v1")
}

// NewCerebras: ultra-fast inference (Llama/Qwen/GPT-OSS/GLM).
func NewCerebras(apiKey, baseURL string) Client {
	return NewOpenAICompat("cerebras", apiKey, baseURL, "https://api.cerebras.ai/v1")
}

// NewGroq: LPU inference (Llama/Kimi/Qwen/GPT-OSS).
func NewGroq(apiKey, baseURL string) Client {
	return NewOpenAICompat("groq", apiKey, baseURL, "https://api.groq.com/openai/v1")
}

// NewXAI: xAI Grok.
func NewXAI(apiKey, baseURL string) Client {
	return NewOpenAICompat("xai", apiKey, baseURL, "https://api.x.ai/v1")
}

// NewTogether: Together.ai aggregator.
func NewTogether(apiKey, baseURL string) Client {
	return NewOpenAICompat("together", apiKey, baseURL, "https://api.together.ai/v1")
}

// NewHuggingFace: HF inference router.
func NewHuggingFace(apiKey, baseURL string) Client {
	return NewOpenAICompat("huggingface", apiKey, baseURL, "https://router.huggingface.co/v1")
}

// NewZAI: Z.AI GLM family.
func NewZAI(apiKey, baseURL string) Client {
	return NewOpenAICompat("zai", apiKey, baseURL, "https://api.z.ai/api/coding/paas/v4")
}

// NewXiaomi: Xiaomi MiMo family (default endpoint).
func NewXiaomi(apiKey, baseURL string) Client {
	return NewOpenAICompat("xiaomi", apiKey, baseURL, "https://api.xiaomimimo.com/v1")
}

// NewXiaomiTokenPlan creates a regional Xiaomi token-plan client.
// region must be "ams", "cn", or "sgp", matching the three
// `xiaomi-token-plan-*` provider ids.
func NewXiaomiTokenPlan(region, apiKey, baseURL string) Client {
	var fallback string
	var name string
	switch region {
	case "ams":
		fallback = "https://token-plan-ams.xiaomimimo.com/v1"
		name = "xiaomi-token-plan-ams"
	case "cn":
		fallback = "https://token-plan-cn.xiaomimimo.com/v1"
		name = "xiaomi-token-plan-cn"
	case "sgp":
		fallback = "https://token-plan-sgp.xiaomimimo.com/v1"
		name = "xiaomi-token-plan-sgp"
	default:
		panic(fmt.Sprintf("xiaomi token-plan: unknown region %q", region))
	}
	return NewOpenAICompat(name, apiKey, baseURL, fallback)
}

// NewOpenRouter: OpenRouter aggregator. Unlocks dozens of upstream
// models with one key.
func NewOpenRouter(apiKey, baseURL string) Client {
	return NewOpenAICompat("openrouter", apiKey, baseURL, openrouterDefaultBaseURL)
}

// NewGondola: Gondola, a USDC-paid marketplace gateway for Venice AI
// inference. OpenAI Chat Completions-compatible.
func NewGondola(apiKey, baseURL string) Client {
	return NewOpenAICompat("gondola", apiKey, baseURL, gondolaDefaultBaseURL)
}

// NewYoloAuto: Yolo-Auto, an OpenAI Chat Completions-compatible gateway
// addressed by the `yolo` and `yolo-small` model aliases.
func NewYoloAuto(apiKey, baseURL string) Client {
	return NewOpenAICompat("yolo-auto", apiKey, baseURL, yoloAutoDefaultBaseURL)
}

// NewOpenCode is the opencode.ai Zen endpoint. Mixed APIs upstream; this
// constructor wires the openai-completions flavor only. Models that need
// the anthropic-messages flavor under the same provider should be built
// with NewAnthropicCompat against the same base URL.
func NewOpenCode(apiKey, baseURL string) Client {
	return NewOpenAICompat("opencode", apiKey, baseURL, "https://opencode.ai/zen/v1")
}

// NewOpenCodeGo routes each model through the wire API exposed by OpenCode Go.
// Chat-completions models use baseURL as-is; Responses models share it via
// the Responses client; Anthropic-messages models (claude, minimax, qwen)
// target the sibling /anthropic tree documented at
// https://opencode.ai/v2/docs/console/go/. The session header is stamped
// per-request on both OpenAI wires; the Anthropic client sends none (Go
// recognizes the native Anthropic session headers instead).
func NewOpenCodeGo(apiKey, baseURL string) Client {
	const defaultBaseURL = "https://opencode.ai/zen/go/v1"
	root := firstNonEmptyString(baseURL, defaultBaseURL)
	return NewModelRouter("opencode-go",
		NewOpenAICompat("opencode-go", apiKey, baseURL, defaultBaseURL),
		map[string]Client{
			APIResponses:         NewOpenAIResponsesNamed(apiKey, root, "opencode-go"),
			APIAnthropicMessages: NewAnthropicCompat("opencode-go", apiKey, anthropicGoBase(root)),
		})
}

// anthropicGoBase maps an opencode-go OpenAI base URL to its sibling
// Anthropic-messages tree (…/go/anthropic serves /v1/messages).
func anthropicGoBase(openaiBase string) string {
	if strings.HasSuffix(openaiBase, "/zen/go/v1") {
		return strings.TrimSuffix(openaiBase, "/zen/go/v1") + "/inference/go/anthropic"
	}
	return strings.TrimSuffix(openaiBase, "/") + "/anthropic"
}

// NewMinimaxOpenAI is the OpenAI-completions flavor of MiniMax, in case
// downstream models switch from anthropic-messages. The main MiniMax route
// uses anthropic-messages; see NewMinimaxAnthropic below.
func NewMinimaxOpenAI(apiKey, baseURL string) Client {
	return NewOpenAICompat("minimax", apiKey, baseURL, "https://api.minimax.io/v1")
}

// NewMinimaxCNOpenAI is the CN-region MiniMax (openai-completions).
func NewMinimaxCNOpenAI(apiKey, baseURL string) Client {
	return NewOpenAICompat("minimax-cn", apiKey, baseURL, "https://api.minimaxi.com/v1")
}

// NewFireworksOpenAI is the OpenAI-completions flavor of Fireworks.
// The main route uses the anthropic-messages variant on
// api.fireworks.ai/inference; see NewFireworksAnthropic.
func NewFireworksOpenAI(apiKey, baseURL string) Client {
	return NewOpenAICompat("fireworks", apiKey, baseURL, "https://api.fireworks.ai/inference/v1")
}

// NewVercelGatewayOpenAI is Vercel AI Gateway's OpenAI-compat shim. The
// main route uses anthropic-messages; see NewVercelGatewayAnthropic.
func NewVercelGatewayOpenAI(apiKey, baseURL string) Client {
	return NewOpenAICompat("vercel-ai-gateway", apiKey, baseURL, "https://ai-gateway.vercel.sh/v1")
}

// ----------------------------------------------------------------------
// Anthropic Messages–compatible providers. These speak Anthropic's wire
// format but live behind a third party's base URL. They reuse
// anthropicClient with a custom name.
// ----------------------------------------------------------------------

// NewAnthropicCompat returns an anthropicClient pinned to a non-default
// base URL and identifying as `name` for cost / logging purposes. Auth is
// API key (x-api-key header). For OAuth-fronted compatibles (rare) use
// NewAnthropicOAuth and rename via NameClient.
func NewAnthropicCompat(name, apiKey, baseURL string) Client {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	return &anthropicClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		name:    name,
		http:    &http.Client{Timeout: 0},
	}
}

// NewKimiCoding is the Kimi Code client: Kimi behind the Anthropic
// Messages API at https://api.kimi.com/coding. Replaces the older
// OpenAI-completions-on-/coding/v1 wiring.
func NewKimiCoding(apiKey, baseURL string) Client {
	return NewKimiCodingWithHeaders(apiKey, baseURL, nil)
}

// NewKimiCodingWithHeaders is the headered variant used by OAuth.
func NewKimiCodingWithHeaders(apiKey, baseURL string, headers map[string]string) Client {
	if baseURL == "" {
		baseURL = "https://api.kimi.com/coding"
	}
	return &anthropicClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		name:    "kimi",
		headers: headers,
		http:    &http.Client{Timeout: 0},
	}
}

// NewMinimaxAnthropic is the anthropic-messages flavor on
// api.minimax.io/anthropic, catalogued under provider=minimax.
func NewMinimaxAnthropic(apiKey, baseURL string) Client {
	return NewAnthropicCompat("minimax", apiKey, firstNonEmptyString(baseURL, "https://api.minimax.io/anthropic"))
}

// NewMinimaxCNAnthropic is the CN-region MiniMax (anthropic-messages).
func NewMinimaxCNAnthropic(apiKey, baseURL string) Client {
	return NewAnthropicCompat("minimax-cn", apiKey, firstNonEmptyString(baseURL, "https://api.minimaxi.com/anthropic"))
}

// NewFireworksAnthropic is the main Fireworks route. The
// anthropic-messages-compatible endpoint at api.fireworks.ai/inference
// expects Anthropic-style request bodies; use this rather than the
// OpenAI flavor.
func NewFireworksAnthropic(apiKey, baseURL string) Client {
	return NewAnthropicCompat("fireworks", apiKey, firstNonEmptyString(baseURL, "https://api.fireworks.ai/inference"))
}

// NewVercelGatewayAnthropic — Vercel AI Gateway anthropic-messages route.
func NewVercelGatewayAnthropic(apiKey, baseURL string) Client {
	return NewAnthropicCompat("vercel-ai-gateway", apiKey, firstNonEmptyString(baseURL, "https://ai-gateway.vercel.sh"))
}

// ----------------------------------------------------------------------
// Stubbed providers — bigger protocol surface, follow-up work.
// ----------------------------------------------------------------------

type unimplementedClient struct {
	name string
	hint string
}

func (c *unimplementedClient) Name() string { return c.name }

func (c *unimplementedClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	return nil, fmt.Errorf("provider %q not yet implemented: %s", c.name, c.hint)
}

// NewBedrock returns an AWS Bedrock client. See amazon_bedrock.go for the
// hand-rolled Converse-Stream wire-format parser. Auth is via
// AWS_BEARER_TOKEN_BEDROCK (the modern Bedrock API key flow); SigV4
// signing for IAM access-key + secret credentials is not yet wired.
func NewBedrock(apiKey, baseURL string) Client {
	return NewBedrockClient(apiKey, baseURL)
}

// NewGoogleVertex returns a Vertex AI client. See google_vertex.go for
// the full auth + URL-rewrite implementation. Requires GOOGLE_CLOUD_PROJECT,
// GOOGLE_CLOUD_LOCATION, and either GOOGLE_CLOUD_API_KEY or a service-
// account JSON file pointed to by GOOGLE_APPLICATION_CREDENTIALS (or the
// default ADC location ~/.config/gcloud/application_default_credentials.json).
func NewGoogleVertex(apiKey, baseURL string) Client {
	return NewVertex(apiKey, baseURL)
}

// NewMistral returns a Mistral client using their OpenAI-compatible Chat
// Completions endpoint at https://api.mistral.ai/v1. Mistral also offers a
// bespoke "Conversations" API, but the OpenAI-compat endpoint supports
// the same models with tool calling and streaming, so we use that for
// simplicity (no extra wire format to maintain).
func NewMistral(apiKey, baseURL string) Client {
	return NewOpenAICompat("mistral", apiKey, baseURL, "https://api.mistral.ai/v1")
}

// Cloudflare endpoints carry `{CLOUDFLARE_ACCOUNT_ID}` and (for the AI
// Gateway) `{CLOUDFLARE_GATEWAY_ID}` placeholders that we substitute
// from env vars at client-construction time. Workers AI uses standard
// `Authorization: Bearer <key>`; AI Gateway uses `cf-aig-authorization`
// (the upstream Authorization header is passed through to whichever
// downstream provider the gateway forwards to).

func resolveCloudflareURL(template string) (string, error) {
	out := template
	for _, key := range []string{"CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"} {
		placeholder := "{" + key + "}"
		if !strings.Contains(out, placeholder) {
			continue
		}
		v := os.Getenv(key)
		if v == "" {
			return "", fmt.Errorf("%s is required but not set in env", key)
		}
		out = strings.ReplaceAll(out, placeholder, v)
	}
	return out, nil
}

// NewCloudflareWorkersAI returns an OpenAI-compatible client pinned to
// the Workers AI base URL with {CLOUDFLARE_ACCOUNT_ID} substituted.
// Returns an erroring client (deferred error on first Stream call) if
// the env var is missing, so the constructor signature stays the same.
func NewCloudflareWorkersAI(apiKey, baseURL string) Client {
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai/v1"
	}
	resolved, err := resolveCloudflareURL(baseURL)
	if err != nil {
		return &unimplementedClient{name: "cloudflare-workers-ai", hint: err.Error()}
	}
	return NewOpenAICompat("cloudflare-workers-ai", apiKey, resolved, "")
}

// NewCloudflareAIGateway returns the AI Gateway client (OpenAI compat
// route). Sends `cf-aig-authorization` instead of `Authorization` so
// the gateway authenticates the caller (downstream-provider auth is
// configured per-gateway in the Cloudflare dashboard).
func NewCloudflareAIGateway(apiKey, baseURL string) Client {
	if baseURL == "" {
		baseURL = "https://gateway.ai.cloudflare.com/v1/{CLOUDFLARE_ACCOUNT_ID}/{CLOUDFLARE_GATEWAY_ID}/compat"
	}
	resolved, err := resolveCloudflareURL(baseURL)
	if err != nil {
		return &unimplementedClient{name: "cloudflare-ai-gateway", hint: err.Error()}
	}
	return &openaiClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(resolved, "/"),
		name:    "cloudflare-ai-gateway",
		headers: map[string]string{"cf-aig-authorization": "Bearer " + apiKey},
		http:    &http.Client{Timeout: 0},
	}
}

// NewGithubCopilot returns a GitHub Copilot client. The provided
// credential must be a GitHub Personal Access Token (PAT) with Copilot
// access enabled; zot trades it for a short-lived Copilot token on
// every inference request (cached in memory until ~5min before expiry).
//
// Wire formats: Anthropic Messages, OpenAI Responses, and Chat Completions,
// selected by model family. Copilot-specific headers
// (X-Initiator, Openai-Intent, Editor-Version, Editor-Plugin-Version,
// Copilot-Integration-Id, User-Agent) are added by the refresh
// transport. The model id passes through unchanged.
//
// baseURL is ignored: the canonical host is read from `proxy-ep=...` in
// the short-lived token's value.
func NewGithubCopilot(apiKey, _ string) Client {
	if apiKey == "" {
		return &unimplementedClient{name: "github-copilot", hint: "set COPILOT_GITHUB_TOKEN"}
	}
	return NewGithubCopilotClient(apiKey)
}

// ----------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
