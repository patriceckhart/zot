# zot extensions

zot can be extended with custom slash commands by running an external
program as a subprocess and exchanging newline-delimited JSON over
its stdin/stdout. Extensions can be written in **any language** that
can read and write JSON lines from stdio — Go, TypeScript, Python,
Rust, shell with `jq`, anything.

Four phases shipped so far:

- **Phase 1**: slash commands + chat notifications.
- **Phase 2**: tools the LLM can call.
- **Phase 3**: lifecycle event subscriptions + tool-call interception
  for guardrail extensions.
- **Phase 4**: interactive extension-owned panels rendered inside zot.
- **Theme-only extensions**: ship `theme.json` without launching a
  subprocess. See [themes.md](themes.md).

## Quick start

The simplest extension is a script that prints a hello frame, reads
commands, and prints responses. Here's the whole thing in **Python**,
no SDK required:

```python
#!/usr/bin/env python3
# $ZOT_HOME/extensions/hello-py/hello.py
import json, sys

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

emit({"type":"hello","name":"hello-py","version":"1.0.0","capabilities":["commands"]})

for line in sys.stdin:
    msg = json.loads(line)
    if msg["type"] == "hello_ack":
        emit({"type":"register_command","name":"hellopy","description":"say hi (python)"})
        emit({"type":"ready"})
    elif msg["type"] == "command_invoked":
        emit({"type":"command_response","id":msg["id"],"action":"prompt",
              "prompt": "Greet me very briefly in one sentence."})
    elif msg["type"] == "shutdown":
        emit({"type":"shutdown_ack"})
        break
```

Drop it in a directory with this `extension.json`:

```json
{
  "name": "hello-py",
  "version": "1.0.0",
  "exec": "./hello.py",
  "language": "python",
  "enabled": true
}
```

`exec` is required for protocol extensions. If an extension only ships
`theme.json` or `themes/theme.json`, no `exec` is required and zot does
not spawn a subprocess. A skill-only bundle may instead provide a `skills`
array and is also never spawned.

`chmod +x hello.py`, install:

```bash
zot ext install ./hello-py
```

Restart `zot`, type `/hellopy`, the agent greets you. Done.

## Built-in extensions

**zot ships with no extensions installed by default.** A fresh `zot install` (or `go install`) gives you a clean agent. Extensions are entirely opt-in: you install (or `--ext` for one run) only the ones you want.

The `examples/extensions/` directory in the repo is reference code, not a default install set. To use any of those:

```bash
# go-based examples need a build first
cd path/to/zot/examples/extensions/hello && go build -o hello .

# install (copies to $ZOT_HOME/extensions/hello/)
zot ext install path/to/zot/examples/extensions/hello

# or load straight from the repo for one zot session
zot --ext path/to/zot/examples/extensions/hello
```

Nothing is auto-installed and nothing reaches out to the network without your explicit action.

## Layout & discovery

zot scans two directories on startup, in this order:

1. **Project-local**: `./.zot/extensions/<name>/extension.json`
2. **Global**: `$ZOT_HOME/extensions/<name>/extension.json`

A project-local extension with the same name wins over a global one.
When `XDG_STATE_HOME` is set on any platform, `$ZOT_HOME` defaults to
`$XDG_STATE_HOME/zot`. Otherwise it defaults to `~/Library/Application Support/zot/`
on macOS, `~/.local/state/zot` on Linux, or `%LOCALAPPDATA%\zot` on Windows.

Because each extension owns its own directory, the recommended place
for extension state is inside that directory itself (for example
`todos.json`, `settings.json`, or an auth/cache file used only by that
extension). The host also passes this path back in `hello_ack` as
`extension_dir` / `data_dir` so runtime code does not need to guess it.

Each extension owns its own subdirectory. The `extension.json`
manifest tells zot how to launch it:

```json
{
  "name": "weather",
  "version": "1.0.0",
  "exec": "./weather",
  "args": ["--mode", "daemon"],
  "language": "go",
  "description": "current weather for any city",
  "enabled": true
}
```

| field | meaning |
|---|---|
| `name` | required. how zot identifies the extension; must match what's sent in the `hello` frame. |
| `version` | optional. shown in `zot ext list`. |
| `exec` | required. path to the executable (relative to the manifest). |
| `args` | optional. extra argv passed to `exec`. |
| `language` | optional. informational only (`go`, `python`, `typescript`, ...). |
| `description` | optional. shown in `zot ext list`. |
| `skills` | optional. directories (recursively scanned) or direct `SKILL.md` paths, relative to the manifest. |
| `enabled` | optional, defaults to `true`. set to `false` to disable without removing. |

## Lifecycle

1. **Discovery**: zot reads every `extension.json` in the search dirs.
2. **Spawn**: enabled extensions are launched as subprocesses. stderr
   redirects to `$ZOT_HOME/logs/ext-<name>.log` (one file per
   extension, append-mode).
3. **Hello handshake**: the extension's first stdout frame must be
   `hello`; zot replies with `hello_ack` containing the protocol
   version, the active provider/model/cwd, and the extension's own
   data directory so it can persist files beside its manifest.
4. **Registration**: after receiving `hello_ack`, the extension sends
   `register_command`, `register_tool`, and subscription frames, then
   sends `ready`. First-come-first-served: a name already taken by a
   built-in or by a previously-loaded extension is silently shadowed
   (logged in the extension's own log file).
5. **Runtime**: after `ready`, zot dispatches `command_invoked` frames
   when the user runs a registered command; the extension responds
   with `command_response`. Extensions can also push `notify` frames
   during runtime. Panel-capable extensions may open an interactive
   panel, receive key events, and push redraws while the panel is
   focused.
6. **Shutdown**: when zot exits, it sends `shutdown` and waits up to
   2s for the extension to send `shutdown_ack`. Holdouts are
   SIGTERM'd, then SIGKILL'd.

A crashing extension does not bring down zot. The slash command it
owned simply stops working until the extension is fixed and zot is
restarted.

## Wire format

All frames are one JSON object per line. Top-level `type` is the
discriminator. Optional `id` correlates request frames with their
responses. The canonical startup order is `hello`, `hello_ack`,
registration frames, then `ready`. Do not send `notify`, logs, or any
other stdout frame before `hello`.

### Extension → host

#### `hello` (required, first frame)

```json
{"type":"hello","name":"weather","version":"1.0.0",
 "capabilities":["commands","tools","panels"]}
```

Include `"call_tool"` to opt in to host-mediated calls to built-in and
extension tools. The host advertises support in `hello_ack.capabilities`.
Older hosts omit it, and callers must not send `call_tool` to those hosts.

Include `"tool_cancel"` in `capabilities` to opt in to host cancellation
notifications. The Go SDK advertises this automatically. The host only sends
`tool_cancel` to extensions advertising that exact capability. Missing or
unknown capabilities do not enable cancellation notifications.

#### `call_tool`

An opt-in extension may request an active tool by name with JSON object args:

```json
{"type":"call_tool","id":"request-1","name":"skill","args":{"name":"example"}}
```

The host replies with a correlated result:

```json
{"type":"tool_result","id":"request-1","name":"skill",
 "content":[{"type":"text","text":"..."}],"is_error":false}
```

`is_error` reports tool or policy failure. Content contains text or base64
image blocks. Results are bounded to 256 KiB and mark `truncated: true` when
shortened. Each request is synchronous for its caller, but multiple requests
can be outstanding concurrently if they use distinct IDs. A
`call_tool_cancel` frame with the same ID cancels the request. Calls time out
after 60 seconds or when the extension disconnects. Cancellation cannot undo
side effects that have already completed.

Calls use the active agent's tool registry, guard interception, and normal
confirmation policy. Headless modes retain their normal no-confirmation
behavior. A call does not create a model tool-use message in the session
transcript. Tool-call, tool-result, confirmation, permission-decision, and
tool-interception frames identify these calls through `origin_extension`.
As with model-initiated calls, event subscribers may receive sensitive tool
output.

Tool handlers can invoke other tools by setting `parent_id` to the ID of the
host's `tool_call` frame. The host carries the parent's cancellation context
through the nested call, refuses repeated tool names in a chain, and allows at
most four tool calls in one chain, including the original tool. Unknown or
expired parent IDs fail. To avoid untraceable recursion, requests without
`parent_id` are refused while that extension has a host-invoked tool active.
A concurrent command-initiated request from that extension is therefore also
refused until its host-invoked tool finishes. Interactive tools can outlive the
60-second request deadline only when invoked by the model, not via `call_tool`.

The Go SDK exposes `e.CallTool(ctx, name, args)` and rejects older hosts
immediately. A tool handler's context supplies `parent_id` automatically.
Tool failures return `ToolResult.IsError`, while context and transport failures
return a Go error. Extensions have no direct access to another extension's
process or built-in implementation.

#### `register_command`

```json
{"type":"register_command","name":"weather",
 "description":"current weather for a city"}
```

Command names are matched case-insensitively. zot sends `command_invoked.name` using the canonical spelling registered here. Registrations that differ only by case conflict, and the first registration remains active.

#### `register_tool`

Registers a tool the LLM can call. `schema` is a JSON Schema object
describing the tool's args (the same shape Anthropic and OpenAI accept).

```json
{"type":"register_tool","name":"weather",
 "description":"Get the current weather for a city.",
 "schema":{
   "type":"object",
   "properties":{"city":{"type":"string"}},
   "required":["city"]
 }}
```

Tool names live in the same namespace as built-in tools (`read`,
`write`, `edit`, `bash`, `skill`). Conflicts are silently shadowed by
the built-in.

Set `"deferred": true` to register a tool without advertising its definition initially. A loader tool can activate registered deferred tools by returning their names in `activate_tools`:

```json
{"type":"tool_result","id":"...",
 "content":[{"type":"text","text":"Enabled weather lookup"}],
 "activate_tools":["weather"]}
```

On Kimi K3's OpenAI-compatible routes, zot places newly activated schemas at the tool-result position using Kimi's native deferred-tool format. Other models receive the complete active tool list on the next request. Unknown names are ignored. The Go extension SDK exposes `DeferredTool` and `ToolResult.ActivateTools` for the same protocol.

Optional `output_schema` declares a tool's JSON result type for programmatic callers such as codemode. Optional `namespace` groups its discovery entries (defaulting to the extension name). Return the value in `tool_result.structured_content` while keeping ordinary model-facing blocks in `content`. Optional `namespace_description` and `namespace_instructions` provide discovery context. These additive fields do not change the protocol version or existing tools.

```json
{"type":"register_tool","name":"weather",
 "schema":{"type":"object"},
 "output_schema":{"type":"object","properties":{"temperature":{"type":"number"}}},
 "namespace":"weather"}
```

The Go SDK provides `StructuredTool(name, description, schema, outputSchema, handler)` and `ToolResult.StructuredContent`. A structured result can still carry `is_error:true`, allowing scripts to inspect failures as data. Without both an output schema and structured result, codemode uses text or rejects failed calls. This metadata describes the result, it does not validate it against the declared schema.

Optional `exposure` accepts `direct` (the default), `codemode` (callable from scripts and discoverable without an eager direct definition), `deferred` (discoverable without inline declarations until activated), or `model-only` (not callable from scripts). An explicit exposure overrides the legacy `deferred` flag. Unknown exposure values are rejected.

`ToolWithOptions(name, description, schema, ToolOptions, handler)` combines these fields with output schemas and a context-aware handler. Set `ToolOptions.Interactive` only for tools waiting on user input, not merely to receive cancellation. Existing `Tool`, `StructuredTool`, `DeferredTool` and `InteractiveTool` registrations retain their behavior.

#### `ready`

Sentinel telling zot "all initial registrations are flushed". Send it
right after your last `register_*` frame so the host can build the
agent's tool registry without racing the registration window.

```json
{"type":"ready"}
```

#### `tool_result`

Reply to a `tool_call` from the host. `content[]` is a list of
message blocks; each block is `{"type":"text","text":"..."}` or
`{"type":"image","mime_type":"image/png","data":"<base64>"}`. Set
`is_error: true` to mark the call as failed. `activate_tools` can name
registered deferred tools that become available after this result. Optional
`structured_content` is a JSON value for callers of tools declaring `output_schema`.

Text blocks may include `"format":"markdown"` to opt into Markdown rendering
inside interactive tool results. Omitted or unknown formats retain ordinary
plain-text rendering. This is a display hint only: models receive the original
text, and saved sessions preserve the hint. It does not change extension panel
rendering or add ANSI styling to print/RPC output. Older hosts ignore the hint.
The Go SDK provides `ext.MarkdownResult(text)` for a single Markdown block,
or `ToolContent{Type: "text", Text: text, Format: "markdown"}` for mixed results.

```json
{"type":"tool_result","id":"...",
 "content":[{"type":"text","text":"Berlin: 16°C, fog"}]}
```

#### `subscribe`

Declares which lifecycle events the extension wants to observe and
which it wants to intercept. Send once after `hello`, before `ready`.

```json
{"type":"subscribe",
 "events":["session_start","turn_start","tool_call","tool_confirmation_requested","turn_end","assistant_message"],
 "intercept":["tool_call","turn_start","assistant_message"]}
```

Recognised event names: `session_start`, `session_end`, `user_prompt_submit`,
`turn_start`, `turn_end`, `tool_call`, `tool_result`,
`tool_confirmation_requested`, `permission_decision`, `assistant_message`,
`pre_compact`, `post_compact`, `subagent_start`, `subagent_stop`.
The new lifecycle events are observational only; adding them to `intercept`
does not make them blocking hooks. See [Lifecycle payloads](#lifecycle-payloads).

`tool_confirmation_requested` fires only when zot is about to wait for
interactive approval. Calls running in yolo mode, calls covered by a
remembered approval, and calls blocked before confirmation do not emit it.
The event includes `tool_id`, `tool_name`, and the short `tool_preview`
shown in the confirmation dialog.

Interceptable events:

- `before_agent_start`: inspect and replace the complete system prompt before
  the first model call. See [System prompt replacement](#system-prompt-replacement).
- `tool_call`: block the call (model sees `reason` as the tool
  error) or rewrite args via `modified_args`.
- `turn_start`: block the turn before the model is called. Useful
  for rate-limiting and business-hour gates. `reason` is shown to
  the user as a status line. No rewrite supported.
- `assistant_message`: suppress the message via `block`, or rewrite
  the user-visible text via `replace_text`. The model's original
  text stays in the transcript so the model sees what it actually
  said on subsequent turns.

#### System prompt replacement

Subscribe with `{"type":"subscribe","intercept":["before_agent_start"]}`.
After prompt assembly (including context files, skills, and extension tools),
zot sends a request before the first agent model call:

```json
{"type":"event_intercept","id":"start-1","event":"before_agent_start","session_id":"session-123","agent_run_id":"run-456","cwd":"/work/repo","provider":"anthropic","model":"claude-opus-4-7","system_prompt":"The complete current system prompt"}
```

Reply with an exact replacement:

```json
{"type":"event_intercept_response","id":"start-1","system_prompt":"The complete replacement system prompt"}
```

No trimming, merging, or appending is performed by the host. `""` intentionally
removes the prompt; omission leaves it unchanged. `block` does not block startup.
Extensions run serially in successful load order (explicit extensions first,
then discovery); each receives the previous extension's result.

The result remains stable across normal user messages, retries, and tool-loop
steps. The event runs again after a model change, conversation clear, session
change, or explicit prompt rebuild/reset (including interactive extension reload).
Rebuilds start from the unmodified base prompt, not the last extension result.
If a prompt rebuild/reset occurs while a hook is waiting, zot discards that
preparation's result and runs the chain again against the current base prompt,
even when the reset keeps the same text. The new pass has a new `agent_run_id`.
Cancellation stops preparation without committing a pending replacement.
`session_start` remains a separate one-way setup notification.

`session_id` is the persisted conversation ID when available, otherwise a
runtime-local ID. `agent_run_id` identifies one preparation pass and changes
on each rebuild. It is shared by all extensions in that pass.

Each extension has a five-second deadline including the request pipe write.
Invalid/non-string values, malformed responses, crashes, and timeouts preserve
the current prompt and produce an extension log diagnostic and host warning.
An omitted value is a normal no-op. The encoded `system_prompt` response value
is limited to 1 MiB; complete transport frames are limited to 4 MiB. Exceeding
the frame limit disconnects the extension's read loop. A blocked request write
closes its input pipe to release the writer. Other subscribed extensions still
run unless the user cancels startup.

Supported in interactive, print, JSON, and RPC modes. RPC warnings use
`ext_notify`. Subscribed extensions see the entire prompt, including private
context-file instructions. This feature is not a security boundary and does
not change tool permissions or validate the safety of replacement instructions.

#### `event_intercept_response`

Reply to an `event_intercept` from the host. All fields default to
"allow, pass through unmodified".

| field | meaning |
|---|---|
| `block` | `true` refuses the action. For `tool_call`, `reason` is shown to the model; for `turn_start` / `assistant_message`, `reason` is shown to the user. |
| `reason` | refusal text (on block) or pass-through note. |
| `modified_args` | for `tool_call`: rewritten JSON args the tool will actually see. Must be a valid JSON object. Ignored when `block` is true. |
| `replace_text` | for `assistant_message`: replaces the user-visible text. The model's original output still lives in the transcript. Ignored when `block` is true. |
| `system_prompt` | for `before_agent_start`: exact string replacement, including an empty string. Omission keeps the current prompt. |

Missing the response within 5s is treated as "allow, unchanged". For
`before_agent_start`, this deadline also includes the request write; legacy
interceptors start the timeout after writing the request. When multiple
extensions subscribe to the same event, they're consulted serially;
the first `block` wins for the blocking events and rewrites (args / text)
chain. `before_agent_start` follows the non-blocking replacement rules above.

```json
{"type":"event_intercept_response","id":"...",
 "block":true,"reason":"refused: matches danger pattern \"rm -rf\""}

{"type":"event_intercept_response","id":"...",
 "modified_args":{"command":"echo GUARDED: ls"}}

{"type":"event_intercept_response","id":"...",
 "replace_text":"[redacted]"}
```

#### `command_response` (reply to `command_invoked`)

```json
{"type":"command_response","id":"...","action":"prompt",
 "prompt":"Show today's weather for Berlin in one line."}
```

`action` is one of:

- `"prompt"` — submits `prompt` as a fresh user message; the agent
  runs a turn against it.
- `"tool_prompt"`: submits `prompt`, executes `tool_name` with JSON-object
  `tool_args` through the active agent's normal tool guards and confirmation,
  then records a paired assistant tool call and tool result before the model
  turn. The full tool output is stored in the session and visible to the
  model, so extensions should only request this for content intended to be
  persistent model context. Tool errors are recorded as tool errors.
  Cancellation stops before the model turn and preserves any completed
  call/result pair. This action is supported for interactive slash commands,
  not spontaneous `call_tool` requests. The extension does not receive the
  tool result in the response.
- `"insert"` — inserts `insert` into the editor at the cursor without
  submitting.
- `"display"` — appends `display` to the chat as a one-shot styled
  note. No model call, nothing written to the transcript.
- `"open_panel"` — opens an extension-owned interactive panel inside
  zot. The panel content lives in `open_panel`.
- `"noop"` — the extension handled it itself (e.g. it pushed
  `notify` frames or kicked off background work). zot doesn't change
  the UI in response.

For a skill shortcut, return the request without copying the skill output
into the prompt:

```json
{"type":"command_response","id":"...","action":"tool_prompt",
 "prompt":"and push","tool_name":"skill",
 "tool_args":{"name":"developer:commit"}}
```

Go SDK: `return ext.ToolPrompt("skill", map[string]string{"name": "developer:commit"}, args)`.
Unlike `CallTool`, this executes after the command handler returns. Avoid
calling the same tool in the handler or the work will run twice. The recorded
assistant call includes `origin_extension` metadata. Older hosts do not
understand the new action and report an unknown action instead of running it.
For Gemini 3, zot adds Google's documented
[`skip_thought_signature_validator`](https://ai.google.dev/gemini-api/docs/generate-content/thought-signatures)
placeholder to unsigned synthetic function calls when serializing requests.
Provider-issued thought signatures are preserved. The extension-to-Gemini
request and session replay are covered with a local Gemini-compatible server,
not a live Gemini API round trip.

Example:

```json
{"type":"command_response","id":"...","action":"open_panel",
 "open_panel":{
   "id":"todos-main",
   "title":"Todos",
   "lines":["□ ship panel api","✓ persist state"],
   "footer":"↑/↓ navigate - a add - x complete - esc close"
 }}
```

Panel body lines and footer text wrap to the terminal width. Unselected body
lines preserve CSI SGR styling across wrapped rows. Selected lines use the
theme's selection styling on every wrapped row instead of extension-provided
ANSI. Panels do not currently impose a height limit or provide host-side
scrolling, so long panels can extend beyond the visible terminal area.

If `error` is non-empty, zot renders it as a red status line
regardless of `action`.

#### `submit` (one-way, any time)

Submits text as a user prompt in the interactive host. If the agent is
idle, zot starts a turn immediately. If a turn is already running, zot
queues the prompt behind it using the same queue path as typed input.
Empty or whitespace-only text is ignored.

```json
{"type":"submit","text":"Summarize the selected workspace."}
```

In print / JSON / RPC modes this frame is ignored because there is no
interactive editor or prompt queue.

#### `panel_render` (one-way, while a panel is open)

Pushes a fresh frame for an already-open panel.

```json
{"type":"panel_render","panel_id":"todos-main",
 "title":"Todos",
 "lines":["□ ship panel api","✓ persist state"],
 "footer":"↑/↓ navigate - a add - x complete - esc close"}
```

#### `panel_close`

Closes a previously-open panel.

```json
{"type":"panel_close","panel_id":"todos-main"}
```

#### `notify` (one-way, any time)

```json
{"type":"notify","level":"info",
 "message":"refreshed cache (12 entries)"}
```

`level` is one of `info`, `success`, `warn`, `error`. The note shows
up below the transcript with the extension's name in brackets. Notes
are one-shot: they clear automatically when the user sends their next
prompt (and on `esc` / `/clear`).

#### `clear_notes` (one-way, any time)

Removes every note this extension previously pushed via `notify` /
`display`. Use it for transient status lines (e.g. an approval prompt)
so they do not stack up; notes from other extensions are untouched.

```json
{"type":"clear_notes"}
```

In `--mode rpc`, this surfaces to the host as an `ext_clear_notes`
event (alongside `ext_notify` / `ext_display`).

#### `shutdown_ack`

Sent in response to `shutdown`. Extension should exit promptly after.

### Host → extension

#### `hello_ack`

```json
{"type":"hello_ack","protocol_version":1,
 "zot_version":"0.0.7","provider":"anthropic",
 "model":"claude-opus-4-7","cwd":"/Users/pat/Developer/zot",
 "extension_dir":"/Users/pat/Developer/zot/.zot/extensions/todos",
 "data_dir":"/Users/pat/Developer/zot/.zot/extensions/todos"}
```

Sent immediately after `hello`. Wait for this frame before sending
registrations if they depend on host metadata. The extension can use
these fields to decide which commands to register (e.g. only register
a Python tool on macOS, only register a model-specific shortcut for
opus, etc.). `cwd` is the user's project directory; the extension
process itself runs from the extension directory, so do not use
`os.Getwd()` or `process.cwd()` when you need the project path.
`extension_dir` / `data_dir` are where the extension should persist
its own state (for example `todos.json`, cached metadata, or auth
tokens scoped to that extension).

#### `command_invoked`

```json
{"type":"command_invoked","id":"...",
 "name":"weather","args":"berlin"}
```

`args` is everything the user typed after the command name, trimmed.

#### Interactive tools

Set `"interactive": true` on `register_tool` for a tool that intentionally
waits for user input. Omitted or false retains the normal 60-second reply
timeout. Interactive tools have no host-imposed reply deadline, but context
cancellation, context deadlines, and extension disconnects still end the call.
Print, JSON, RPC, and other hosts without interactive tool support reject
these calls with a tool error instead of opening an invisible panel.

```json
{"type":"register_tool","name":"ask_user",
 "description":"Ask the user a question","schema":{"type":"object"},
 "interactive":true}
```

The Go SDK provides a context-aware handler:

```go
e.InteractiveTool("ask_user", "Ask the user a question", schema,
    func(ctx context.Context, args json.RawMessage) ext.ToolResult {
        // Open your panel and arrange for its callbacks to deliver an answer.
        // Use invocation-local state, including a unique panel ID.
        defer closeQuestionPanel()
        select {
        case answer := <-answers:
            return ext.TextResult(answer)
        case <-ctx.Done():
            return ext.TextErrorResult("Question cancelled")
        }
    })
```

Here `schema`, `answers`, and `closeQuestionPanel` are extension-owned values.
Panel dismissal must also resolve the handler's wait. Cancellation is
cooperative, the SDK cannot stop a handler that ignores its context. It
cancels active handler contexts on `tool_cancel`, shutdown, or host EOF.
Existing `Tool` and `DeferredTool` handlers remain source-compatible.

The registration field is additive and does not change the protocol version.
Older hosts ignore it and still apply their normal timeout. Legacy extensions
that do not advertise `"tool_cancel"` receive no cancellation frames. The host
still stops waiting and discards late results, but cannot request cleanup of
abandoned work in those extensions. Both host and extension must support this
feature for cancellable, unlimited waits.

#### `tool_call`

Sent when the LLM invokes a tool the extension registered. `args` is
the parsed JSON object the model produced; the extension is
responsible for validating/coercing it.

```json
{"type":"tool_call","id":"...","name":"weather",
 "args":{"city":"Berlin"}}
```

Reply with `tool_result` within the host's tool timeout (default 60s).
Interactive tools have no reply timeout. Missing a bounded timeout surfaces
an error to the model and marks the call as `timed_out`. Request writes
retain a separate bounded transport timeout, even for interactive tools.
Cancellation during a blocked write disconnects the extension transport
because a partially written frame cannot safely be withdrawn. Other pending
calls on that transport also fail.

#### `tool_cancel`

Sent best-effort when a delivered tool invocation is abandoned by cancellation
or its reply timeout, and only if the extension advertised `"tool_cancel"` in
`hello.capabilities`. Normal completion does not send a cancellation frame.
`id` is the opaque correlation ID from `tool_call`, not an agent transcript
tool ID.

```json
{"type":"tool_cancel","id":"..."}
```

Stop the associated work, close its panel, and discard its pending state.
Unknown or already-completed IDs should be ignored. Cancellation can arrive
before the handler goroutine starts, so register cancellation state before
dispatching work. Delivery is not guaranteed after a transport disconnect.
Extensions must also clean up on shutdown and EOF. The host safely ignores
late results for abandoned calls, including races with cancellation.

#### `event`

Lifecycle notification for events the extension subscribed to via
`subscribe`. One-way, no response expected. Fields are additive under protocol
version 1. Clients must tolerate unknown fields and event names.

```json
{"type":"event","event":"turn_start","step":1}
{"type":"event","event":"tool_call",
 "tool_id":"...","tool_name":"read","tool_args":{"path":"foo.go"}}
{"type":"event","event":"tool_confirmation_requested",
 "tool_id":"...","tool_name":"read","tool_preview":"foo.go"}
{"type":"event","event":"turn_end","stop":"end_turn"}
```

### Lifecycle payloads

Notifications include `session_id`, `cwd`, and a monotonically increasing
`sequence` scoped to the extension manager's lifetime. Sequence numbers span
all notifications, not just an extension's subscriptions, so gaps do not by
themselves indicate lost delivery. They reset when zot restarts. Correlate tool
outcomes with `(session_id, tool_id)`, compaction with `compaction_id`, and swarm
runs with `agent_run_id`. These are payload identifiers, not request/reply IDs.

| Event | Additional payload | Meaning |
|---|---|---|
| `session_start` | No additional fields | An active conversation opens |
| `session_end` | `reason` | The active conversation closes |
| `user_prompt_submit` | `text`, optional `queued`, `image_count` | Model input accepted, before appending or queueing |
| `turn_start` | `step` | A model step begins |
| `turn_end` | `stop`, optional `error` | A model response attempt finishes, before client tool execution |
| `tool_call` | `tool_id`, `tool_name`, `tool_args` | The model's original proposed call |
| `tool_confirmation_requested` | `tool_id`, `tool_name`, `tool_preview` | Interactive approval is about to be requested |
| `permission_decision` | `tool_id`, `tool_name`, `decision`, `source`, `stage`, optional `reason` | An approval or policy decision was resolved |
| `tool_result` | `tool_id`, `tool_name`, `tool_args`, `status`, `executed`, `result` | A client tool call reached a terminal outcome |
| `assistant_message` | `text` | Visible assistant text |
| `pre_compact` | `compaction_id`, `message_count`, `token_estimate` | A compaction attempt begins |
| `post_compact` | Same counts and ID, `status`, optional `error` | A compaction attempt finishes, including failure |
| `subagent_start` | `agent_id`, `agent_run_id`, `name` | A local swarm runner starts or resumes |
| `subagent_stop` | Same identity, `status`, optional `error` | That local swarm runner returns |

#### Prompt submission

`text` is the input handed to the agent after host preprocessing, not raw editor
keystrokes. This includes extension-generated prompts and queued follow-ups.
Queued input emits once at acceptance with `queued:true`, not again at
consumption; it can subsequently be withdrawn without reaching the model.
Image bytes are not included; `image_count` reports attached images. An
image-only prompt may omit `text`. Slash commands that do not submit model
input, transcript replay, compaction summaries, and internal image mirrors do
not emit this event.

#### Tool outcomes and permission decisions

```json
{"type":"event","event":"tool_result","session_id":"session-1","cwd":"/work","sequence":12,"tool_id":"call-1","tool_name":"bash","tool_args":{"command":"go test ./..."},"status":"completed","executed":true,"result":{"content":[{"type":"text","text":"ok"}],"is_error":false}}
```

`status` is `completed`, `failed`, `blocked`, `cancelled`, or `timed_out`.
Failures include unknown tools, panics, and tool error results. Guards and
built-in jail/manifest policy refusals are `blocked`. Cancellation and timeout
classification uses typed execution errors or explicit tool outcome metadata,
not error-message matching. A custom tool that reports only `is_error` without
such metadata is classified as `failed`.

`tool_args` contains effective arguments, including accepted interceptor
rewrites and the empty-argument default `{}`. For a call refused before
execution, it contains the last proposed arguments instead. If the arguments
are malformed JSON, `tool_args_raw` contains their raw text and `tool_args` is
omitted, so the outcome remains a valid protocol frame. `executed` means
`Tool.Execute` was entered, not that a side effect occurred or was rolled back.
A tool-local policy refusal can therefore be `blocked` with `executed:true`.
Pending calls cancelled before execution still receive terminal results.
Provider-executed server tools do not receive client `tool_result` events.

`result.content` uses text and base64 image blocks. Observation payloads have a
256 KiB combined text/base64/MIME-data budget and a 128-block limit. Text may be
shortened at a UTF-8 boundary; oversized images and unsupported block types are
omitted. `result.truncated:true` marks any omission. Tool execution, the model's
result, and the persisted transcript are not truncated by this observation
limit. Private `Details` and tool activation metadata are not exported.

```json
{"type":"event","event":"permission_decision","session_id":"session-1","tool_id":"call-1","tool_name":"bash","decision":"approved","source":"user","stage":"pre_execution"}
```

`decision` is `approved`, `denied`, or `cancelled`. `source` is `user`,
`remembered`, `policy`, or `yolo`. Automatic approval is represented by an
approved decision with its source, not a separate decision value. Guard
refusals, preview failures, and no available confirmer use `policy`.

A `pre_execution` approval does not bypass tool-local checks. If a tool later
refuses access, an additional denied decision with `source:"policy"` and
`stage:"tool_execution"` precedes its blocked result. These events cannot grant
permission or override a refusal. Calls cancelled before checking permission
or rejected as unknown tools can have a result without a permission decision.
Existing mode-specific confirmation behavior is unchanged, including modes
that do not support interactive approval.

#### Session, compaction, and swarm boundaries

Sessions use the persisted conversation ID when available, otherwise a
runtime-generated ID. Interactive and RPC sessions start when initialized;
single-shot/headless sessions start when the agent first emits an observation.
Switching conversations emits the old session's end before the new start.
Current end reasons are `shutdown`, `session_switch`, and `cwd_change`.
`session_end` is emitted at most once per activation on these graceful paths.
Switching models or clearing messages does not close the active conversation.
Reloading extensions does not close the session or replay earlier events.

Every compaction attempt emits a matching `post_compact`, including empty
transcripts, failures, and cancellation. Status is `completed`, `failed`,
`cancelled`, or `timed_out`. Post counts describe the resulting in-memory
transcript; they do not assert that persistence succeeded. Token estimates
count serialized text bytes divided by four, not exact provider usage or image
tokens. The events do not let extensions modify the summary.

Swarm events are emitted by the parent supervisor, including resumed tasks.
`session_id` identifies the task's parent conversation, even if the active
conversation later changes. `agent_id` survives resume; `agent_run_id` is fresh
for each local runner invocation and pairs start with stop. `name` currently
uses the agent ID. Stop statuses are `completed`, `failed`, `cancelled`, and
`timed_out`. Merely loading detached agents from disk does not replay lifecycle
events. Cancellation requests do not emit stop until the runner returns. On
interactive shutdown zot waits up to two seconds for local runners before
closing extensions; unresponsive runners may not produce a final notification.

#### Ordering, delivery, and privacy

For a normal client tool step, the observable sequence is:

```text
user_prompt_submit
turn_start
assistant_message / tool_call
turn_end
synchronous tool interception
tool_confirmation_requested (when needed)
permission_decision
tool execution
tool_result
next turn_start (if continuing)
```

This preserves the existing meaning of `turn_end`: it ends the model response,
not the tool batch. Multiple calls are announced before the batch executes;
each client call then gets its own permission and result events. Retries,
blocked turns, queued prompts, and concurrent swarm tasks can add or interleave
events; do not assume a globally linear workflow from the example alone.

Frames are serialized per extension, including notifications and interception
requests. Notification enqueue does not wait for the subprocess. Each
extension has a bounded outbound queue (256 frames or 16 MiB of queued bytes,
plus one in-flight frame); overflow disconnects it rather than silently dropping
an event and continuing. Synchronous writes are bounded to five seconds.
Graceful shutdown attempts to drain queued frames, including `session_end`,
before sending `shutdown`, within the shutdown grace period.

Delivery is best-effort, not a durable or exactly-once audit log. There is no
acknowledgement, replay, or crash recovery for notifications. A crash, forced
termination, slow consumer, or broken pipe can prevent delivery. The protocol's
4 MiB frame limit still applies. Slow event handlers can delay later
interception requests, so extension handlers should hand lengthy work to their
own bounded queue. Existing interception response and failure policies remain
unchanged.

Prompts, effective tool arguments, outputs, paths, and error text may contain
secrets or private data. Only subscribe to content-bearing events you need.
There is no automatic secret redaction; subscriptions are not an extension
security sandbox. This surface is available where zot loads an extension
manager (interactive, print/stream/JSON, RPC, and swarm-child modes). It does
not add extension loading to other hosts or change the RPC client event schema.

#### `event_intercept`

Sent when zot wants to give the extension a chance to block, modify,
or annotate a lifecycle event before it happens. Reply with
`event_intercept_response` within 5s; missing the deadline is
treated as "allow".

Payload fields depend on the event:

```json
// tool_call: includes the tool id, name, and parsed args
{"type":"event_intercept","id":"...","event":"tool_call",
 "tool_id":"...","tool_name":"bash",
 "tool_args":{"command":"rm -rf /tmp/foo"}}

// turn_start: includes the step number
{"type":"event_intercept","id":"...","event":"turn_start",
 "step":3}

// assistant_message: includes the assembled text
{"type":"event_intercept","id":"...","event":"assistant_message",
 "text":"here is your api key: sk-ant-..."}
```

#### `panel_key`

Sent while an extension-owned panel is focused. `key` is a normalized
name (`up`, `down`, `left`, `right`, `enter`, `esc`, `tab`, `pageup`,
`pagedown`, `home`, `end`, `backspace`, `delete`, `rune`). For
`key:"rune"`, `text` carries the typed character.

```json
{"type":"panel_key","panel_id":"todos-main","key":"down"}
{"type":"panel_key","panel_id":"todos-main","key":"rune","text":"x"}
```

#### `panel_close`

Sent when the user closes the focused panel from zot (for example with
Esc or Ctrl+C). The extension should treat this as the panel lifetime
ending and stop sending `panel_render` updates for that `panel_id`.

```json
{"type":"panel_close","panel_id":"todos-main"}
```

#### `shutdown`

Sent during graceful zot exit (or `/reload-ext` once that lands).
Reply with `shutdown_ack` and then exit.

## Managing extensions from the CLI

```
zot ext list                    list installed extensions and their state
zot ext doctor                  diagnose load, registration, and conflict issues
zot ext install <path|git-url>  copy / clone into $ZOT_HOME/extensions/
zot ext remove <name>           delete an extension directory
zot ext enable <name>           re-enable a disabled extension
zot ext disable <name>          disable without removing
zot ext logs <name> [-f]        cat / tail the extension's stderr
```

`remove`, `enable`, and `disable` accept the manifest name shown by
`zot ext list` or the installed directory name. For example, an extension
named `todo` installed in `zot-todo/` can be removed with either name.
Both global and project-local installations are searched. If a name matches
multiple installations (including a manifest name matching another directory
name), the command reports the matching paths and makes no changes. Use a
unique directory name to disambiguate where possible. `logs` uses the
manifest name.

`remove` deletes the installation directory, including state stored inside
it. It does not stop extensions in other running zot sessions. Close those
sessions before uninstalling, otherwise an extension may recreate its state
directory when saving or shutting down. Normal startup ignores directories
without `extension.json`, including state-only leftovers. It does not delete
that leftover data. Explicit `--ext` paths still report missing manifests,
and malformed or unreadable manifests still produce errors. After closing
active sessions, inspect and manually remove any leftover state directory
if needed. `ext remove` requires an installation with a manifest.

`zot ext doctor` runs the same discovery path as zot startup, but reports
what happened instead of changing the fail-soft runtime behavior. It shows
manifest errors, disabled or shadowed extensions, subprocess load errors,
ready/auto-ready status, registered commands/tools, registration conflicts,
warnings, and each extension's stderr log path.

`zot ext install <path>` does a recursive copy; `<git-url>` does a
shallow clone. Both validate that the destination contains an
`extension.json` and roll back if not.

## Loading an extension for one run

For iteration on a working copy, skip the install + reload cycle
and load straight from disk for one zot session:

```
zot --ext ./my-extension        # short form: -e ./my-extension
zot --ext ./a -e ./b            # repeatable
```

`--ext` paths take precedence over installed extensions of the same
name, so you can shadow an installed copy with a work-in-progress
version without uninstalling first. Nothing is copied or persisted;
the extension dies with zot like any other subprocess.

## SDKs

Writing the wire protocol by hand is fine for one-off scripts, but
for anything bigger the SDKs handle the boilerplate.

### Go — `packages/agent/ext`

```go
package main

import (
    "encoding/json"
    "github.com/patriceckhart/zot/packages/agent/ext"
)

func main() {
    e := ext.New("hello", "1.0.0")

    // Slash command
    e.Command("hello", "say hi", func(args string) ext.Response {
        return ext.Prompt("Greet me in one short sentence.")
    })

    // LLM-callable tool
    e.Tool("weather", "Current weather for a city.",
        json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
        func(args json.RawMessage) ext.ToolResult {
            var in struct{ City string `json:"city"` }
            json.Unmarshal(args, &in)
            return ext.TextResult(in.City + ": sunny")
        })

    // Optional: register project-specific commands after hello_ack.
    e.OnHello(func(host ext.HostInfo) {
        if host.CWD != "" {
            e.Command("cwd", "show the current project directory", func(args string) ext.Response {
                return ext.Display(host.CWD)
            })
        }
    })

    e.Run()
}
```

Build with `go build -o hello .`, drop the binary + an `extension.json`
into `$ZOT_HOME/extensions/hello/`.

`OnHello` is optional. Use it when configuration or registrations need
host metadata such as `HostInfo.CWD`, `Provider`, `Model`, `ZotVersion`,
`ExtensionDir`, or `DataDir`. The SDK sends `hello`, waits for
`hello_ack`, runs `OnHello`, announces registrations, then sends `ready`.

The SDK has five interceptor hooks, all optional:

```go
// e is the *ext.Extension returned by ext.New(...).

// Replace the complete prompt once per session or explicit rebuild.
// Return nil to keep it, or a pointer to "" to remove it entirely.
e.InterceptBeforeAgentStart(func(event ext.BeforeAgentStartEvent) *string {
    prompt := event.SystemPrompt + "\nUse the project's preferred coding style."
    return &prompt
})

// Refuse calls or rewrite args before they run.
e.InterceptToolCall(func(tool string, args json.RawMessage) (bool, string) {
    if tool == "bash" { /* inspect args, return false, reason */ }
    return true, ""
})

// Richer variant: returns ToolCallDecision so you can also rewrite
// args via ModifiedArgs.
e.InterceptToolCallX(func(tool string, args json.RawMessage) ext.ToolCallDecision {
    return ext.ToolCallDecision{
        ModifiedArgs: json.RawMessage(`{"command":"echo GUARDED"}`),
    }
})

// Block the next turn before the model is called.
e.InterceptTurnStart(func(step int) ext.TurnStartDecision {
    if time.Now().Hour() < 9 { return ext.TurnStartDecision{Block: true, Reason: "outside business hours"} }
    return ext.TurnStartDecision{}
})

// Scrub or rewrite the assistant's final text before the user sees it.
e.InterceptAssistantMessage(func(text string) ext.AssistantMessageDecision {
    return ext.AssistantMessageDecision{
        ReplaceText: strings.ReplaceAll(text, "SECRET", "[redacted]"),
    }
})
```

See:
- `examples/extensions/hello/` — slash commands
- `examples/extensions/clock/` — slash commands in plain Node, no SDK
- `examples/extensions/weather/` — LLM-callable tool
- `examples/extensions/guard/` — event subscriptions + tool-call
  interception (refuses dangerous bash patterns)
- `examples/extensions/todo/` — interactive persistent panel + tool
- `examples/extensions/scratchpad/` — source-run TypeScript commands + tool

### Hot reload

Type `/reload-ext` in the TUI to tear down every running extension
subprocess, re-read the manifests from disk, and respawn the set.
The agent's tool registry is rebuilt automatically, so freshly-
registered extension tools become callable without restarting zot.
Useful while developing an extension: edit, save, `/reload-ext`,
done. Explicit `--ext` paths are remembered and reloaded alongside
discovered extensions. The temporary reload status reports each load
error in red, including its message, and dismisses itself after five
seconds. Extension failures from startup and `/reload-ext` remain in the
scrolling chat until a successful reload or `/clear`, without being sent to
the model or saved in the session transcript. Subprocess startup and
handshake errors identify the extension and its directory, configured executable,
and declared language (if present), along with the original error and stderr log
path when the log was opened. Manifest errors also identify the extension
directory. Multiline diagnostics preserve their line breaks in both the temporary
status and scrolling chat.
If a subprocess exits before sending `hello`, the error also reports its exit
status or terminating signal.

### TypeScript / Python

These SDKs aren't in the main repo yet; the wire format is small
enough that a `~30 line` raw script gets you started in either
language. See the [Quick start](#quick-start) Python example for the
shape. SDK packages will land in follow-up commits.

## Security

Extensions run with **the user's full filesystem and network
permissions**. Treat installing an extension the same as installing
any other binary on your machine.

`zot ext install <git-url>` clones from any URL you give it. There's
no sandbox in v1; if you need isolation, install only extensions you
trust or run zot under your platform's sandboxing tool (`bwrap` /
`sandbox-exec` / AppContainer).

## Roadmap

Phase 1 (shipped):
- [x] subprocess lifecycle + hello handshake
- [x] `register_command` + `command_invoked`
- [x] `notify` + `clear_notes`
- [x] `zot ext` CLI

Phase 2 (shipped):
- [x] `register_tool` + `tool_call` + `tool_result`
- [x] `ready` sentinel for safe agent-registry build timing
- [x] tool result attribution surfaces extension name in details

Phase 3 (shipped):
- [x] event subscriptions (`session_start`, `turn_start`, `turn_end`,
      `tool_call`, `assistant_message`)
- [x] tool-call interception (block before execution)

Phase 4 (shipped):
- [x] interception for `turn_start` and `assistant_message` (in
      addition to `tool_call`)
- [x] modify tool args mid-flight via `modified_args`
- [x] rewrite user-visible assistant text via `replace_text`
- [x] `/reload-ext` slash command (hot-reload without restarting zot)

Future (no firm timeline):
- [ ] TypeScript and Python SDK packages (currently the wire format
      is stable enough to hand-roll, see the Python quick-start)
- [ ] HTTP / WebSocket transport variants (today: subprocess stdio)
- [ ] per-extension permission scopes (today: full user privileges)
