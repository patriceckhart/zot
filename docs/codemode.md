# Codemode

Codemode runs JavaScript to orchestrate the session's tools, filter intermediate results, and call classifier or image models. Only explicit output reaches the chat model.

## Enable

```sh
zot --codemode
zot --codemode-only
zot --tools read,glob,codemode
```

`--codemode` retains direct tool definitions. `--codemode-only` exposes client tools through codemode instead, with inline TypeScript declarations for nondeferred tools. The callable registry and permission checks do not change. `--no-tools` takes precedence. Explicit `--tools` selection restricts the built-in tools.

Persistent configuration in `$ZOT_HOME/config.json`:

```json
{
  "codemode": {
    "enabled": true,
    "mode": "only",
    "inlineBudget": 3000
  }
}
```

`mode` accepts `on` and `only`. `inlineBudget` is a non-negative estimated token budget for declarations, defaulting to 3000. A value of zero leaves discovery available without inline declarations. CLI mode overrides the configured mode.

These settings apply across interactive, print, JSON, RPC, and bot modes. Go SDK users can set `sdk.Config.Codemode` or select `"codemode"` in `Config.Tools`.

## Source and execution

The input is JavaScript source, run as an async function body with top-level `await` and `return`, without markdown fences. Supported GPT-5 and newer Responses endpoints advertise codemode as a native custom tool with a Lark grammar, so the model sends raw source rather than a JSON-escaped string. Other providers expose the source in a `code` argument:

```json
{"code":"const source = await tools.read({path:'package.json'}); return JSON.parse(source).name"}
```

The executor also accepts a JSON string or raw source. Native source deltas are translated into the same JSON `code` representation for guards, streamed events, RPC and stored sessions. Replays use native custom calls and matching custom results on supported endpoints, or JSON function calls elsewhere. User-defined models can opt in or out with `compat.supportsOpenAIGrammarTools` in `models.json`.

Optional first source line:

```js
// @options: {"max_output_tokens":2000,"timeout_ms":60000}
const results = await Promise.all([
  tools.read({path:"package.json"}),
  tools.read({path:"packages/api/package.json"})
])
return results.map(source => JSON.parse(source).name)
```

There is **no default script deadline**. Caller cancellation still terminates the VM. `timeout_ms` accepts positive integers up to 2147483647. The legacy JSON argument `timeout_ms` remains supported. Source options take precedence.

Every invocation starts a fresh VM. Ordinary variables do not survive. Use `store` and `load` for persistent JSON state.

## Script API

| API | Behavior |
|---|---|
| `await tools.name(args)` | Execute a tool with a JSON object and resolve its result. |
| `ALL_TOOLS` | Callable tool catalog, including deferred tools, as `{name, description}` entries. Descriptions include TypeScript declarations. |
| `await searchTools(query, {limit, namespace})` | BM25-ranked discovery, default limit 8. |
| `await describeTool(name)` | TypeScript declaration and description, or `undefined` if unknown. |
| `await describeNamespace(name)` | Namespace name, optional description and instructions, and tool names, or `undefined`. |
| `text(value)` | Emit text, with objects and arrays JSON-serialized. |
| `console.log(...values)` | Emit one line of space-separated values. `info`, `warn`, `error`, and `debug` behave the same way. |
| `return value` | Emit a final value unless it is `undefined`. |
| `image(value)` | Emit a supported local base64 image. |
| `exit()` | Immediately finish successfully and retain successful store writes. It cannot be intercepted by script `catch` blocks. |
| `store(key, value)` | Store a JSON copy under a string key. `undefined` deletes the key. |
| `load(key)` | Return a fresh JSON copy, or `undefined` for a missing key. |

Tool identifiers are normalized to ASCII JavaScript identifiers, replacing unsupported characters with underscores. Original names also work through `tools["custom-tool"]`. The first tool wins when normalized names collide. `tools`, `models`, `ALL_TOOLS` and `console` are immutable. Missing members report suggestions, while `"name" in tools` can test availability.

### Tool results

A tool declaring an output schema and returning `StructuredContent` resolves to that JSON value, including structured failure results. Otherwise its text blocks are joined with newlines. Failed or refused calls without a structured result reject with their error text. Tools without structured output do not automatically forward images.

Shell tools return:

```ts
{
  output: string
  truncated: boolean
  full_output_path?: string
  exit_code: number
  wall_time_seconds: number
}
```

`output` contains merged stdout and stderr. Beyond 1 MiB it keeps the first and last 512 KiB, trimmed to UTF-8 character boundaries, around a byte-omission marker. `truncated` describes that script output, not the smaller direct-tool display. The full stream is spilled to a private temporary file when the direct display is truncated. Structured `full_output_path` is present only when the script output is truncated and the spill succeeded. Nonzero exits resolve to the object so scripts can inspect `exit_code`, while cancellation and timeouts reject.

```js
const result = await tools.bash({command:"go test ./..."})
text({exit:result.exit_code, output:result.output})
```

### Images

`image` accepts a base64 data URL, `{image_url: "data:..."}`, or `{type:"image", data, mimeType}`. PNG, JPEG, GIF, and WebP are recognized from their signatures. Remote URLs are rejected. Emitted images and text preserve order until text output is truncated, at which point the head/tail text summary precedes retained images.

```js
image("data:image/png;base64,iVBORw0KGgo=")
```

### Persistent state

Store keys must be strings and values must be JSON-serializable. Each JSON value is limited to 262144 UTF-16 characters, with 1048576 characters across keys and JSON values. Limit violations throw `RangeError`. Reads and writes copy JSON, so later object mutation does not change the stored value.

Writes commit only when the script succeeds, including `exit()`. Errors and cancellation discard that script's writes. Completed external side effects are not rolled back.

Snapshots are persisted in message metadata under `tool_state:codemode`, separate from model-visible content. Resume and branch replay reconstruct state from the selected messages. Compaction carries forward snapshots. Transcripts containing this metadata may contain private application state.

## Models

```js
const available = await models.getAvailableOfType("classifier")
if (available.length) {
  const result = await models.classify(available[0], {
    state:{text:"A synthetic example"},
    questions:{positive:{
      type:"bool",
      instructions:"Is the sentiment positive?",
      criteria:{true:"Positive sentiment", false:"Negative or neutral sentiment"}
    }}
  })
  return result
}
```

Catalog methods:

- `models.getModelsOfType(type, provider?)`
- `models.getAvailableOfType(type, provider?)`
- `models.getModelOfType(type, provider, id)`, returning `undefined` for an unknown model

Types are `chat`, `classifier`, and `image`. Availability means credentials or a no-auth provider are configured, not that the endpoint is reachable. Chat models can be listed but cannot be executed from scripts. Auxiliary models remain separate from chat-model pickers.

### Classification

`models.classify(model, {state, questions})` accepts an object state and a nonempty question map. Every question has string `instructions` and one of:

- `choice`: `criteria` maps labels to descriptions. Answers contain `choice`, `probabilities`, and `confidence`.
- `bool`: `criteria` contains `true` and `false` descriptions. Answers contain `probability` for true.
- `score`: `criteria` is a list of level descriptions. Answers contain `score` and `confidence`.

Supported transports are TypeSafe-compatible System One, Cloudflare Workers AI System One, and llama.cpp raw next-token probabilities. Cloudflare's bundled endpoint resolves `CLOUDFLARE_ACCOUNT_ID` in the host. Local choice classification supports 2 to 62 labels, score supports 2 to 10 levels, and labels must tokenize distinctly. Local prompts repeat the state around a shared question overview, preserving question and choice ordering.

### Image generation

```js
const available = await models.getAvailableOfType("image")
if (available.length) {
  const result = await models.generateImages(available[0], {
    input:[{type:"text",text:"Draw a simple landscape"}]
  })
  for (const block of result.output) {
    if (block.type === "image") image(block)
    else if (block.type === "text") text(block.text)
  }
}
```

`input` is a nonempty array of text or base64 image blocks. Image generation uses the OpenRouter image transport. Generated blocks are not automatically emitted. Codemode adds a note when image generation returned images but the script emitted no image.

Both inference APIs return `api`, `provider`, `model`, `timestamp`, and `stopReason`, together with `answers` or `output`. Service failures return `stopReason:"error"` and `errorMessage`, cancellation returns `"aborted"`. Invalid arguments reject. Reported usage is included and charged to the session even if the script later fails. Its usage events carry `auxiliary:true` (Go SDK `Event.Auxiliary`) and do not replace the chat context estimate. Unknown pricing is not a promise of free inference. At most four inference calls run concurrently per script.

Credentials and endpoints stay in the host. Scripts select catalog models, not arbitrary endpoints or API keys. TypeSafe credentials can use `TYPESAFE_API_KEY`. Other providers use their normal credential resolution, including user-defined providers. Configure additional classifier or image models in `models.json` with `type`, `api`, and optional `output` fields. Supported auxiliary API IDs are `typesafe-system-one`, `cloudflare-workers-ai-system-one`, `llama-cpp-classify`, and `openrouter-images`. The bundled auxiliary catalog includes classifier entries for TypeSafe, OpenRouter, OpenCode, Vercel AI Gateway and Cloudflare Workers AI, plus OpenRouter's published image-model catalog. Its snapshot was checked against public listings on 2026-10-02. Availability still depends on credentials and provider configuration.

### Discovery and extension exposure

Extensions can supply namespace descriptions and instructions, which participate in BM25 ranking and `describeNamespace`. Inline declarations use a shared UTF-16-based budget, selecting the cheapest remaining tool from each namespace in rounds. Shared MCP type declarations are charged once when the first MCP tool is selected. Tools that do not fit remain discoverable.

Tool exposure is `direct` by default. `codemode` lists a tool for scripts without an eager direct definition, `deferred` keeps it discoverable without inline declarations until explicitly activated, and `model-only` excludes it from scripts. In `only` mode, direct client definitions are hidden. Deferred definitions remain available for later activation. Go extensions can combine structured output, exposure, namespace metadata and context-aware cancellation with `ToolWithOptions`.

Standalone calls can execute scripts without an agent, with an empty tool catalog and a temporary store. Store writes are discarded after the invocation, and model capabilities require an attached agent.

## Output, permissions, and isolation

Text defaults to 10000 estimated tokens, using four UTF-16 characters per token. `max_output_tokens` accepts a non-negative safe integer. Overflow retains the head and tail and writes full text to a private temporary file. Temporary files are not automatically deleted after the result is returned. Output starts with `Script completed` or `Script failed` and elapsed wall time. Failure preserves earlier emitted output.

Nested calls use normal guards, argument rewrites, confirmations, permissions, and observable tool lifecycle events. IDs are `<parent-id>/<number>` and parallel calls can finish out of order. Their results are events, not separate model transcript messages. Inference operations use this same guarded execution path, with lifecycle names `models.classify` and `models.generateImages`. Catalog queries do not create tool lifecycle rows.

Unawaited calls are cancelled when the script ends. Cleanup waits for host calls, preventing late events or usage updates after the outer result. Host-tool cancellation remains cooperative, so a tool ignoring its context can delay cleanup beyond the script deadline. Recursion is refused, with a maximum nested tool chain of four.

JavaScript runs using **Goja inside WebAssembly**, embedded in zot and executed by Wazero. No external runtime, CGO, or subprocess is required. Each invocation has fresh linear memory with a hard **256 MiB cap**, including the worker's Go runtime and JavaScript heap. Oversized allocations terminate the worker, rather than necessarily raising a catchable JavaScript exception. Context cancellation forcibly stops VM execution, including interpreter work in native callbacks.

The VM mounts no filesystem and exposes no direct network, process, module-loading, or timer APIs. Explicit host capabilities are the only path to outside effects. This confines worker execution, not authorized tools. The cap does not cover host-side JSON decoding, output buffering, concurrent VM totals, or tool allocations. It is not an operating-system sandbox for tools. Jail remains an accident-prevention guardrail.

## Go execution package

`github.com/patriceckhart/zot/packages/codemode` provides the reusable execution engine. It has no dependency on agent, core, or provider packages. Its private worker, wire protocol, build command, and generator live under `packages/codemode/internal/vm`. The compressed worker asset is embedded in the execution package.

Use `Initialize()` to compile the shared worker before starting a script-specific deadline. `Run(ctx, Start, Caller, emit)` starts a fresh VM. `Start` supplies JavaScript source, tool descriptions, optional JSON state, and whether to expose the host-backed models API. `Caller` handles tool and global capability requests and can be called concurrently. `emit` receives text and base64 image items serially, or can be nil to discard output.

`Run` returns a `Result` with script success, error text, and a successful state snapshot. Script exceptions return `OK:false`, while VM and cancellation failures return a Go error. The caller owns state persistence and host permissions. A nil host callback rejects capability requests. Cleanup cancels outstanding requests and waits for callbacks to finish, so callbacks must honor their context.

`packages/agent/codemode` adapts this engine to the agent. It retains tool discovery and presentation, configuration, output formatting, guarded nested calls, usage accounting, and session persistence. CLI flags and script behavior are unchanged by the package split.

## Development

After changing worker code or its dependencies, regenerate the embedded asset:

```sh
go generate ./packages/codemode
go run ./packages/codemode/internal/vm/generate -check -output packages/codemode/worker.wasm.gz
```

The compressed asset is built with `GOOS=wasip1`, `GOARCH=wasm`, and `CGO_ENABLED=0`. Reproduction requires the same Go toolchain and dependency versions.
