# zot providers

zot ships with built-in providers and a model catalog. You can select models
with `/model`, list them with `zot --list-models`, and add private models in
`$ZOT_HOME/models.json`.

## HTTP proxy

Set one global proxy for zot-managed HTTP and HTTPS traffic with the `http_proxy` key in `$ZOT_HOME/config.json`:

```json
{
  "http_proxy": "http://127.0.0.1:7890"
}
```

zot applies this setting at startup. Existing `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`, and `https_proxy` environment variables take precedence for their corresponding protocol. Standard `NO_PROXY` and `no_proxy` bypass lists remain effective. Restart zot after changing the setting.

`config.json` is not a credential store. If the proxy URL contains a username or password, prefer setting the proxy environment variables in a protected shell or service configuration rather than saving those credentials in `config.json`.

## Login methods

Use `/login` in interactive mode. Type in either provider picker to filter the list by provider ID or display name.

- `api key`: stores an API key in `$ZOT_HOME/auth.json` when the provider uses a normal key.
- `subscription`: stores OAuth credentials for subscription-backed providers.

Use `/logout` to remove stored credentials.

Some providers need more than a single pasted key. For those providers,
`/login` shows setup instructions instead of opening a localhost browser form.
This avoids broken browser flows in SSH, containers, and `kubectl exec`
sessions.

### Command-backed API keys

A provider can obtain its API key from a password manager or another local program. Configure this directly in `$ZOT_HOME/auth.json`:

```json
{
  "openai": {
    "api_key_command": {
      "program": "op",
      "args": ["read", "op://Work/OpenAI/credential"],
      "timeout_ms": 120000
    }
  }
}
```

Custom provider credentials use the same object under `additional_api_key_creds`:

```json
{
  "additional_api_key_creds": {
    "my-company": {
      "api_key_command": {
        "program": "secret-tool",
        "args": ["read", "my-company-api-key"]
      }
    }
  }
}
```

The program is started directly rather than through a shell. The timeout defaults to 120 seconds. It must write one non-empty line to stdout, with no more than 64 KiB of output. Trailing CR/LF characters are removed. Successful results stay only in process memory and are reused until zot exits.

Command-backed credentials count as logged in without being executed. zot materializes one only when selecting that provider; background model discovery skips it. `/login` with a normal key replaces the command, while `/logout` clears it. Because `auth.json` can cause program execution, keep it user-writable only and do not use files from untrusted sources.

Setup-instruction providers:

- Amazon Bedrock
- Google Vertex AI
- Cloudflare Workers AI
- Cloudflare AI Gateway
- Azure OpenAI Responses

## Subscription providers

These providers support subscription login:

| Provider | Notes |
| --- | --- |
| Anthropic | Claude Pro/Max OAuth credentials. |
| OpenAI Codex | ChatGPT Plus/Pro Codex subscription route. Separate from the OpenAI API-key provider. |
| Kimi | Kimi subscription login. |
| xAI | SuperGrok or X Premium device-code login. The browser URL is prefilled with the device code. |
| GitHub Copilot | GitHub Copilot token flow. |

OAuth tokens are stored in `$ZOT_HOME/auth.json` and refreshed when refresh is
available.

## GitHub Copilot model availability

The built-in Copilot catalog excludes retired models and utility-only models
that are not selectable for chat. The default is `claude-sonnet-5`.
Availability still depends on your plan and organization policies. At startup,
zot queries Copilot's authenticated `/models` endpoint in the background and
filters the runtime catalog using picker visibility, policy state, and tool-call
support. Individual accounts with no eligible picker entries can fall back to
explicitly enabled model policies; other account types cannot. zot never enables
policies automatically.

Account availability stays in memory, not the shared model cache. Until discovery
succeeds, the static catalog remains visible; failed refreshes retain the previous
snapshot. Command-backed credentials are not executed for background discovery.
Restart zot after changing accounts to refresh the snapshot. This filter is a
selection aid, not an authorization boundary. The website displays the public
catalog, not any individual account's availability.

Claude uses the Anthropic Messages API with Copilot Bearer authentication, without
Claude Code identity injection. GPT, Grok, and MAI use Responses. Gemini and Kimi
use Chat Completions without `reasoning_effort`; their reasoning is managed by the
endpoint. All three routes send user/agent initiator metadata and image-request
headers where appropriate.

As of September 22, 2026, the catalog includes 30 public chat models: Claude
Fable 5/5.1, Haiku 4.5, Opus 4.7/4.8/4.8 Fast/5/5.5, Sonnet 5, Gemini
3.5/3.6/3.7/3.8 Flash, GPT-5 mini, GPT-5.3-Codex, GPT-5.4/5.4 mini/5.5,
GPT-5.6 Luna/Sol/Terra, GPT-6 Astra/Luna/Sol, Grok 4.5/4.6, Kimi K2.7 Code/K3,
and MAI-Code-1-Flash/1.1-Flash. MAI-Code-1-Flash uses the public picker ID
`mai-code-1-flash-picker` and is scheduled to retire on September 10, 2026.

Internal search, execution, and compaction models are not chat choices.
GPT-5.4 nano is excluded because GitHub offers it in the Codex VS Code
extension, not Copilot Chat. New entries use Copilot-specific limits and token
prices from [models.dev](https://models.dev); these are not a quote for your
subscription bill. Existing zero-priced entries do not imply unlimited free use.

Claude Fable models have different data-retention terms: Anthropic retains
prompts and outputs by default for safety classifiers. Eligible enterprises
can request a time-limited zero-data-retention exception from GitHub; enabling
a model does not itself grant that exception. Review
[GitHub's model availability and data-retention notes](https://docs.github.com/en/copilot/reference/ai-models/supported-models)
before using Fable with sensitive data.

Sonnet 4.6 is also excluded from the general catalog: GitHub retired it on
September 1, 2026, except for individual annual Copilot Pro and Pro+ subscribers.
Eligible subscribers can add it through `$ZOT_HOME/models.json`.
See [GitHub's model retirement history and plan exceptions](https://docs.github.com/en/copilot/reference/ai-models/supported-models#model-retirement-history).

## GPT-6 models

Select `gpt-6-astra`, `gpt-6-sol`, or `gpt-6-luna` with provider `openai`,
`openai-responses`, `openai-codex`, or `github-copilot`, for example:

```bash
zot --provider github-copilot --model gpt-6-sol
```

All three use the Responses API, accept image input, support `low`, `medium`,
`high`, `xhigh`, and `max` reasoning, and allow up to 128,000 output tokens.
OpenAI API-key and Codex subscription entries use a 272,000-token context
window. Copilot uses 1,050,000 tokens for Astra and 1,000,000 tokens for Sol
and Luna. Standard per-million-token prices are $10 input and $50 output for
Astra, $2 input and $10 output for Sol, and $0.10 input and $0.50 output for
Luna. Prompts above 272,000 tokens use each model's long-context price tier.
Subscription access depends on your plan, rollout, and organization policy.
Catalog availability does not grant access.

GPT-6.1 Sol (`gpt-6.1-sol`) is available in zot with `openai`,
`openai-responses`, and `openai-codex`, subject to account access. It uses the
Responses API, supports `low` through `max` reasoning and up to 128,000 output
tokens. Zot currently uses a 272,000-token context window for these entries.
Standard per-million-token prices are $2 input, $10 output, $0.10 cache reads,
and $2.50 cache writes. Prompts above 272,000 input tokens use the long-context
price tier. GPT-6.1 Sol is not listed for GitHub Copilot in zot.

## API-key providers

These providers can use environment variables. Simple API-key providers can
also be configured through `/login`. Providers that require extra cloud setup
show instructions and should be configured with environment variables.

| Provider | Environment variable | Stored key |
| --- | --- | --- |
| Anthropic | `ANTHROPIC_API_KEY` | `anthropic` |
| OpenAI | `OPENAI_API_KEY` | `openai` |
| OpenAI Responses | `OPENAI_API_KEY` | `openai-responses` |
| Kimi | `KIMI_API_KEY` or `MOONSHOT_API_KEY` | `kimi` |
| Google Gemini | `GEMINI_API_KEY` or `GOOGLE_API_KEY` | `google` |
| DeepSeek | `DEEPSEEK_API_KEY` | `deepseek` |
| Moonshot AI | `MOONSHOT_API_KEY` | `moonshotai` |
| Moonshot AI China | `MOONSHOT_API_KEY` | `moonshotai-cn` |
| Groq | `GROQ_API_KEY` | `groq` |
| xAI | `XAI_API_KEY` | `xai` |
| Cerebras | `CEREBRAS_API_KEY` | `cerebras` |
| Together AI | `TOGETHER_API_KEY` | `together` |
| Hugging Face | `HF_TOKEN` | `huggingface` |
| OpenRouter | `OPENROUTER_API_KEY` | `openrouter` |
| Gondola | `GONDOLA_API_KEY` | `gondola` |
| Yolo-Auto | `YOLO_AUTO_API_KEY` | `yolo-auto` |
| Mistral | `MISTRAL_API_KEY` | `mistral` |
| ZAI | `ZAI_API_KEY` | `zai` |
| Xiaomi MiMo | `XIAOMI_API_KEY` | `xiaomi` |
| Xiaomi Token Plan Amsterdam | `XIAOMI_TOKEN_PLAN_AMS_API_KEY` | `xiaomi-token-plan-ams` |
| Xiaomi Token Plan China | `XIAOMI_TOKEN_PLAN_CN_API_KEY` | `xiaomi-token-plan-cn` |
| Xiaomi Token Plan Singapore | `XIAOMI_TOKEN_PLAN_SGP_API_KEY` | `xiaomi-token-plan-sgp` |
| MiniMax | `MINIMAX_API_KEY` | `minimax` |
| MiniMax China | `MINIMAX_CN_API_KEY` or `MINIMAX_API_KEY` | `minimax-cn` |
| Fireworks | `FIREWORKS_API_KEY` | `fireworks` |
| Vercel AI Gateway | `AI_GATEWAY_API_KEY` | `vercel-ai-gateway` |
| OpenCode Zen | `OPENCODE_API_KEY` | `opencode` |
| OpenCode Go | `OPENCODE_API_KEY` | `opencode-go` |
| GitHub Copilot token | `COPILOT_GITHUB_TOKEN` or `GITHUB_COPILOT_TOKEN` | `github-copilot` |
| Cloudflare Workers AI | `CLOUDFLARE_API_KEY` | `cloudflare-workers-ai` |
| Cloudflare AI Gateway | `CLOUDFLARE_API_KEY` | `cloudflare-ai-gateway` |
| Azure OpenAI Responses | `AZURE_OPENAI_API_KEY` | `azure-openai-responses` |

When Gondola credentials are available, zot refreshes its public text-model
catalog in the background and adds the discovered models to `/model`.

Yolo-Auto is an OpenAI-compatible gateway with flat-rate plans, so no per-token
prices are shown. The default model, `qwen3.8-flash`, is available on free and
paid plans. The optional `yolo` route requires a paid plan. These models and
`yolo-small` ship with conservative client limits.

With `YOLO_AUTO_API_KEY` set or a key saved through `/login`, zot lists
`/v1/models` in the background on each catalog refresh, independently of the
six-hour public catalog cache. Discovered models use the key's plan-bounded
context windows and advertised reasoning levels, replacing the seed metadata.
The last discovered metadata is cached for startup resolution until the
background refresh completes. If discovery fails, zot falls back to the seed
models and conservative limits instead of retaining the previous key's live
metadata. Create a key at https://yolo-auto.com and see
https://yolo-auto.com/docs for the API reference.

Example:

```bash
export OPENROUTER_API_KEY=...
zot --provider openrouter
```

## Local LM Studio server

Register the `lmstudio` provider through `/login`, choose `api key`, and select
`LM Studio`. Enter the server URL, for example `http://localhost:1234`, followed
by an optional API key. Zot validates the URL and stores the connection in
`$ZOT_HOME/auth.json`. Saving it does not require the server to be online.

A registered server URL is required for discovery. Zot does not probe localhost,
and an API key alone does not enable discovery. Unlike llama.cpp's
`LLAMA_BASE_URL`, `LMSTUDIO_BASE_URL` is not a registration mechanism. Your
current provider and selected model do not change when registering a server.

Open `/model` to refresh and select a discovered model. Zot also refreshes after
saving the connection, in the background on startup, and when selecting or
resuming an LM Studio model. After registration, you can select a model directly:

```sh
zot --provider lmstudio --model my-local-model
```

Discovery prefers `/api/v1/models` and falls back to `/v1/models` when the native
endpoint returns 404, 405, or 501. Authentication errors do not trigger fallback.
The native list excludes entries explicitly marked as embedding models. The
fallback only supplies IDs, so embedding filtering is best-effort.

Loaded models use their instance IDs and configured context lengths. Unloaded
models use their keys and advertised maximum context lengths. Quantization-specific
keys are preserved, but the separate native variant list is not expanded into
additional choices. Listing models does not load or download them. Inference
with an unloaded model depends on LM Studio's auto-loading settings. There is
no `/llama`-style management interface for LM Studio.

Missing context metadata defaults to 32,768 tokens. Output defaults to at most
4,096 tokens, or a quarter of the context window for smaller models. These are
estimates, not server guarantees. Discovered models have zero API pricing, and
token usage comes from inference responses. Reasoning controls are not enabled
automatically from capability metadata, which does not mean server-side reasoning
is disabled. Override metadata under `lmstudio` in `$ZOT_HOME/models.json`,
especially when an unloaded model will use a smaller context than advertised.
User entries take precedence over discovery.

The discovered catalog stays in memory, separate from the six-hour cloud model
cache and llama.cpp's entries. Failed refreshes retain the previous snapshot,
and `/model` reports discovery errors while keeping other providers accessible.
Use `/logout lmstudio` to remove the connection and stop discovery. Background
refresh never executes API-key commands.

The existing coding loop, tools, permissions, and built-in provider pricing
remain unchanged. For older builds without `LM Studio` in `/login`, the manual
OpenAI-compatible setup in [README.md](../README.md#local-models-with-ollama)
remains available.

## Local llama.cpp router

The `llama.cpp` provider connects to a multi-model router and is separate from the `ollama` provider. Ollama normally uses `http://localhost:11434`; entering that URL for llama.cpp management produces a 404 because Ollama does not implement the router endpoints.

Install a current llama.cpp build. On macOS:

```bash
brew install llama.cpp
# use `brew upgrade llama.cpp` for an existing installation
```

Start `llama-server` without `--model`, `-m`, or `-hf`. Those flags select one model and disable the router behavior zot expects.

```bash
mkdir -p ~/llama-models

llama-server \
  --models-dir ~/llama-models \
  --no-models-autoload \
  --jinja \
  --host 127.0.0.1 \
  --port 8080 \
  -ngl 999 \
  -c 32768
```

This configuration discovers GGUF files below `~/llama-models`, leaves model loading under explicit user control, enables chat templates, requests GPU offload, and sets a 32K context. Tune the GPU layers and context size for the available memory.

Check the management API before opening zot:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/models
```

`/models` must return JSON with a `data` array. A 404 indicates the wrong URL, an older server build, or single-model mode.

To save the connection, run `/login`, choose `api key`, and select `llama.cpp`. Enter `http://127.0.0.1:8080`, without `/v1`, followed by an optional bearer key. zot validates the URL and stores both values in `$ZOT_HOME/auth.json`. Environment variables override the saved connection:

```bash
export LLAMA_BASE_URL=http://127.0.0.1:8080
export LLAMA_API_KEY=optional-secret
```

If a key is configured, start the router with the same `--api-key` value. A server bound only to `127.0.0.1` normally does not need authentication.

After login, `/llama` becomes visible. It shows model state, searches Hugging Face for GGUF repositories, offers available quantizations, reports download and load progress, and explicitly loads or unloads models. Press `d` on a router-downloaded cache model to ask the router to remove it after confirmation. Models discovered through `--models-dir` or a preset are not removable through the router API and must be deleted from their configured source. Some llama.cpp versions retain shared Hugging Face repository artifacts such as `mmproj` files after removing the selected GGUF; remove that repository's cache directory manually if those artifacts are no longer needed. `HF_TOKEN` raises Hugging Face API limits and may be required for repository metadata. For gated files, approve access first and provide the authorized token to the `llama-server` process because the router performs the download.

Opening `/model` refreshes the router and displays its loaded models under provider `llama.cpp`. Unloaded models cannot handle inference and therefore remain exclusive to `/llama` until loaded. Inference is sent to the router's derived `/v1` endpoint.

Ollama downloads are stored in Ollama's internal layout and do not automatically become llama.cpp models. Obtain a GGUF copy through `/llama` or place GGUF files under `~/llama-models`, then restart the router after adding files manually.

## Cloud providers

### Amazon Bedrock

Bedrock is configured with AWS credentials, not a generic zot API-key entry.
Use one of these credential sources:

```bash
# AWS profile
export AWS_PROFILE=your-profile

# IAM access keys
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_SESSION_TOKEN=... # only for temporary credentials

# Bedrock API key bearer token
export AWS_BEARER_TOKEN_BEDROCK=bedrock-api-key-...

# Region
export AWS_REGION=us-east-1
```

ECS task roles, IRSA, and other AWS SDK credential-chain sources are also
supported.

Example:

```bash
AWS_BEARER_TOKEN_BEDROCK=bedrock-api-key-... AWS_REGION=us-east-1 \
  zot --provider amazon-bedrock --model anthropic.claude-sonnet-4-5-20250929-v1:0
```

Some Bedrock models require regional inference-profile IDs for on-demand
throughput, such as `us.` or `eu.` prefixed model IDs. zot rewrites known
families automatically where possible. Claude Opus 5 uses
`global.anthropic.claude-opus-5`; zot maps the bare foundation-model ID to that
global profile and supports its adaptive reasoning levels. Explicit profile IDs
and ARNs are left unchanged.

### Google Vertex AI

Vertex can use a Google API key when available:

```bash
export GOOGLE_CLOUD_API_KEY=...
zot --provider google-vertex
```

For service-account or application-default credentials, set the standard
Google environment variables used by your deployment.

### Cloudflare AI Gateway

Cloudflare AI Gateway needs a Cloudflare token plus account and gateway IDs:

```bash
export CLOUDFLARE_API_KEY=...
export CLOUDFLARE_ACCOUNT_ID=...
export CLOUDFLARE_GATEWAY_ID=...
zot --provider cloudflare-ai-gateway
```

### Cloudflare Workers AI

Workers AI needs a Cloudflare token and account ID:

```bash
export CLOUDFLARE_API_KEY=...
export CLOUDFLARE_ACCOUNT_ID=...
zot --provider cloudflare-workers-ai
```

### Azure OpenAI Responses

```bash
export AZURE_OPENAI_API_KEY=...
export AZURE_OPENAI_BASE_URL=https://your-resource.openai.azure.com
export AZURE_OPENAI_API_VERSION=v1 # optional, v1 is the default
zot --provider azure-openai-responses
```

The provider uses Azure's Responses API. If deployment names differ from zot
model IDs, map them without changing the catalog:

```bash
export AZURE_OPENAI_DEPLOYMENT_NAME_MAP='gpt-5.6-luna=luna-preview,gpt-5.6-sol=sol-preview'
```

## Auth file

Credentials are stored in `$ZOT_HOME/auth.json` with user-only permissions
when zot creates the file.

Example:

```json
{
  "anthropic": { "api_key": "sk-ant-..." },
  "openai": { "api_key": "sk-..." },
  "google": { "api_key": "..." },
  "additional_api_key_creds": {
    "openrouter": { "api_key": "..." },
    "mistral": { "api_key": "..." }
  }
}
```

The top-level keys are used for providers with dedicated credential fields.
Other API-key providers are stored under `additional_api_key_creds`. Prefer
`/login` so zot writes the correct schema.

## Custom providers and models

Use `$ZOT_HOME/models.json` for private models, deployment aliases, local
servers, or OpenAI-compatible gateways that are not in the built-in catalog.
User entries override built-in entries with the same provider and model ID, and
adding a `models.json` no longer hides the built-in catalog: your entries are
merged on top of the baked-in and live-discovered models.

A top-level provider key that is not a built-in id defines a custom provider.
Give it a provider-level `baseUrl` and an `api` wire format (`openai` for
OpenAI-compatible Chat Completions, the default, `openai-responses` for the
OpenAI Responses API, or `anthropic` for the Anthropic Messages API). A
model-level `baseUrl` overrides the provider-level one for that model, and an
unknown `api` value falls back to `openai` with a warning.

```json
{
  "providers": {
    "my-company": {
      "baseUrl": "https://llm.mycompany.com/v1",
      "api": "openai",
      "models": [
        { "id": "company-llm-v2", "name": "Company LLM v2" }
      ]
    }
  }
}
```

Custom providers are first-class: they appear in `--list-models`, `/model`, and
`/login`. `models.json` never stores secrets. If the endpoint requires a key,
supply it through `/login`, `--api-key`, or a derived environment variable in
upper snake case, so `my-company` reads `MY_COMPANY_API_KEY`. Because many
self-hosted gateways do not expose a model-list endpoint, custom provider keys
are accepted and stored without a verification probe; an invalid key surfaces
on the first model call. Custom providers with a configured base URL also work
without a key. If a stored, environment, or CLI key must be ignored for a
keyless endpoint, set `"auth": "none"` on that provider in `models.json`. This
also suppresses credentials during live model discovery. Use only with trusted
endpoints, since requests will be unauthenticated even if a key is available.

To retrieve this custom provider's key from a password manager, add a matching
entry to `$ZOT_HOME/auth.json`:

```json
{
  "additional_api_key_creds": {
    "my-company": {
      "api_key_command": {
        "program": "op",
        "args": ["read", "op://Work/OpenAI/credential"],
        "timeout_ms": 120000
      }
    }
  }
}
```

The provider IDs in `models.json` and `auth.json` must match. Then select the
custom model directly:

```sh
zot --provider my-company --model company-llm-v2
```

### Live model discovery

A custom provider can list its models from the OpenAI-compatible `/models`
endpoint instead of, or in addition to, a static `models` array. Set
`"discover": true` at the provider level. Discovery is off by default so
existing configurations never gain network traffic, and it requires a
provider-level `baseUrl`. A `discover` flag without one is ignored with a
warning.

```json
{
  "providers": {
    "m4": {
      "baseUrl": "http://127.0.0.1:48000/v1",
      "discover": true
    },
    "m5": {
      "baseUrl": "http://127.0.0.1:48001/v1",
      "discover": true,
      "models": [
        { "id": "big-model", "contextWindow": 200000, "maxTokens": 16000 }
      ]
    }
  }
}
```

zot requests `<baseUrl>/models`, or `<baseUrl>/v1/models` when the base URL
has no trailing version segment, with a 3 second timeout. It refreshes in the
background on startup, when `/model` opens, and when launching with a
discovered model that is not yet in the catalog. The API key from the
environment or `auth.json` is sent as a bearer token when present. A missing key
is not an error because local servers commonly accept unauthenticated requests.
Background refresh never executes an `api_key_command`; opening `/model` does.

Only model IDs are read from the response. Discovered models default to a
32,768 token context and 4,096 output tokens, have zero pricing, and no
reasoning controls. IDs containing `embed` are skipped. Entries in the
provider's `models` array override discovered metadata for the same ID, which
is how to raise limits or enable reasoning for a specific model.

The discovered list stays in memory, separate from the six-hour cloud model
cache. A failed refresh keeps the previous snapshot for that provider and
`/model` shows the error while other providers stay usable. A persisted
selection of a discovered model survives restarts even though the model is not
in `models.json`. The `/models` wire format is assumed to be OpenAI style
(`{"data": [{"id": ...}]}`) regardless of the provider's `api` setting.

`/model` also lists custom providers with a configured base URL and no key.
`--list-models` prints the catalog before the background refresh completes,
so discovered models may be missing there.

## Credential resolution

For each request, zot checks credentials in this order:

1. Explicit CLI key, such as `--api-key`.
2. Provider-specific environment variables (including derived custom-provider
   variables such as `MY_COMPANY_API_KEY`).
3. `$ZOT_HOME/auth.json`, including custom provider keys saved by `/login`.

`models.json` itself never stores credentials; it only describes models and
their endpoints.

Bedrock then uses the AWS SDK credential chain for the actual request.
