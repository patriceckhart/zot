# zot RPC

`zot rpc` runs the agent runtime as a subprocess that speaks newline-delimited JSON on stdin and stdout. Use it from any language that can spawn a process and read/write its pipes — Go, TypeScript, Python, Rust, shell, anything.

For a Go program embedding the runtime in-process, use the `pkg/zotcore` SDK instead. The wire format below mirrors the SDK's types one-for-one so consumers can share parsing code.

## Quick start

```bash
# spawn zot rpc; talk to it from a shell
( echo '{"id":"1","type":"prompt","message":"hello"}'; sleep 5 ) \
  | zot rpc --provider anthropic
```

You'll see one JSON object per line on stdout: a response acknowledging the prompt, a stream of events (`text_delta`, `tool_call`, `tool_result`, `usage`), then `done`.

## Process model

- One `zot rpc` process serves **one cwd, one model, one session**.
- For multiple projects, spawn multiple processes.
- Concurrency: at most one prompt or compact in flight at a time. A second one queues until the first finishes; aborting fires immediately.
- The process exits when stdin closes.

## Flags

`zot rpc` accepts the same flags as the other modes: `--provider`, `--model`, `--cwd`, `--api-key`, `--base-url`, `--system-prompt`, `--append-system-prompt`, `--reasoning`, `--max-steps`, `--no-tools`, `--tools`. Sessions are disabled by default in RPC mode — the embedding application owns persistence.

## Auth

If the environment variable `ZOTCORE_RPC_TOKEN` is set on the spawned process, the first line on stdin **must** be a `hello` command containing the matching token:

```json
{"id":"0","type":"hello","token":"shared-secret"}
```

If absent or wrong, the response carries `success:false` and the process exits. Without `ZOTCORE_RPC_TOKEN` set, no auth is required (the spawning process is implicitly trusted; if it can spawn `zot` it can also read your `auth.json` directly).

## Wire format

Every line in either direction is one JSON object terminated by `\n`. Object boundaries follow newline boundaries — no multi-line JSON.

### Frame types

| `type` | Direction | Description |
|---|---|---|
| any command (`prompt`, `abort`, …) | client → server | Request |
| `response` | server → client | Reply to one command, correlated by `id` |
| any event (`text_delta`, `tool_call`, …) | server → client | Stream notification (no `id`) |

## Commands

All commands share an optional `id` field; if present, the matching `response` echoes it. Use `id` to correlate replies with requests, especially when several requests are in flight.

### `hello`

```json
{"id":"0","type":"hello","token":"shared-secret"}
```

Response:

```json
{"type":"response","id":"0","command":"hello","success":true,
 "data":{"protocol_version":1,"version":"0.0.4","provider":"anthropic","model":"claude-opus-4-5"}}
```

Required as the first message when `ZOTCORE_RPC_TOKEN` is set; optional otherwise.

### `prompt`

```json
{"id":"1","type":"prompt","message":"fix the failing test","images":[]}
```

Optional `images` is `[{"mime_type":"image/png","data":"<base64>"}]`.

Response is immediate (the turn is starting):

```json
{"type":"response","id":"1","command":"prompt","success":true,"data":{"started":true}}
```

Then a stream of event objects (see below) until the turn ends with `{"type":"done"}`.

### `abort`

Cancel the active prompt or compact.

```json
{"id":"2","type":"abort"}
```

Response: `{"type":"response","id":"2","command":"abort","success":true}`.

If the turn was streaming, the next events you see will be a `turn_end` with `stop:"aborted"` then `done`.

### `compact`

Summarise the current transcript into one synthetic user message. Same lifecycle as `prompt` (immediate response, then events).

```json
{"id":"3","type":"compact"}
```

Final event: `{"type":"compact_done","summary":"<text>"}`.

### `get_state`

Snapshot of the runtime.

```json
{"id":"4","type":"get_state"}
```

Response data:

```json
{
  "provider": "anthropic",
  "model": "claude-opus-4-5",
  "cwd": "/Users/pat/Developer/zot",
  "message_count": 12,
  "busy": false,
  "usage": {"input": 1234, "output": 567, "cache_read": 890, "cache_write": 0, "cost_usd": 0.0123}
}
```

### `get_messages`

Full transcript.

```json
{"id":"5","type":"get_messages"}
```

Response data: `{"messages": [<message>, ...]}`. See **message shape** below.

### `clear`

Drop the entire transcript. Equivalent to the `/clear` slash command.

```json
{"id":"6","type":"clear"}
```

### `set_model`

Switch model within the same provider.

```json
{"id":"7","type":"set_model","model":"claude-sonnet-4-5"}
```

Cross-provider swaps require relaunching `zot rpc` with the new `--provider`.

### `get_models`

List models known for the current provider.

```json
{"id":"8","type":"get_models"}
```

Response data: `{"models":[{"id":"...","provider":"...","context_window":200000,"max_output":8192,"reasoning":true}, ...]}`.

### `ping`

Health check.

```json
{"id":"9","type":"ping"}
```

Response: `{"type":"response","id":"9","command":"ping","success":true,"data":{"pong":true}}`.

## Events

Stream notifications during a `prompt` or `compact`. None carry an `id`.

| `type` | Fields | Meaning |
|---|---|---|
| `turn_start` | `step` | Beginning of one model call (max-steps loop iteration) |
| `user_message` | `content`, `time` | The submitted prompt as it was added to the transcript |
| `assistant_start` | (none) | About to receive assistant streaming |
| `text_delta` | `delta` | Partial assistant text. Concatenate to build the full reply |
| `tool_call` | `id`, `name`, `args` | The model wants to call a tool |
| `tool_progress` | `id`, `text` | Optional progress line from the tool while it runs |
| `tool_result` | `id`, `is_error`, `content` | Tool finished |
| `assistant_message` | `content`, `time` | Final assistant message after the model turn ends |
| `usage` | `input`, `output`, `cache_read`, `cache_write`, `cost_usd`, `cumulative` | Per-turn + cumulative tokens / cost |
| `turn_end` | `stop`, optional `error` | One model call finished. `stop` is `end_turn`, `tool_use`, `length`, `error`, or `aborted` |
| `done` | (none) | The whole prompt/compact completed (success or error) |
| `error` | `message` | Non-fatal error message |
| `compact_done` | `summary` | Compaction finished, summary text included |

## Message shape

Used by `get_messages` and inside `user_message` / `assistant_message` events.

```json
{
  "role": "user",
  "content": [<content_block>...],
  "time": "2026-04-19T11:30:00Z"
}
```

### Content block types

```json
{"type": "text", "text": "..."}
{"type": "image", "mime_type": "image/png", "bytes": 12345}
{"type": "tool_call", "id": "toolu_xyz", "name": "read", "args": {"path": "..."}}
{"type": "tool_result", "call_id": "toolu_xyz", "is_error": false, "content": [<content_block>...]}
```

Image bytes are reported as a count rather than embedded base64 to keep transcript dumps small. Tool results may nest text and image blocks.

## Reference clients

See `examples/rpc/` for working implementations in:

- `shell` — `bash` + `jq` one-liner
- `python` — `subprocess.Popen` + `json.loads` per line
- `node` — `child_process.spawn` + `readline`
- `go` — direct subprocess wrapper (the `pkg/zotcore` SDK is the in-process Go API)

## Versioning

The `protocol_version` field in the `hello` response is the major version of this schema. Backwards-incompatible changes bump it. The set of supported events and commands within a major version only grows.
