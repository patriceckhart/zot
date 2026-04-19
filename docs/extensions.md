# zot extensions

zot can be extended with custom slash commands by running an external
program as a subprocess and exchanging newline-delimited JSON over
its stdin/stdout. Extensions can be written in **any language** that
can read and write JSON lines from stdio — Go, TypeScript, Python,
Rust, shell with `jq`, anything.

Three phases shipped so far:

- **Phase 1**: slash commands + chat notifications.
- **Phase 2**: tools the LLM can call.
- **Phase 3**: lifecycle event subscriptions + tool-call interception
  for guardrail extensions.

## Quick start

The simplest extension is a script that prints a hello frame, reads
commands, and prints responses. Here's the whole thing in **Python**,
no SDK required:

```python
#!/usr/bin/env python3
# $ZOT_HOME/extensions/hello-py/hello.py
import json, sys, threading

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

emit({"type":"hello","name":"hello-py","version":"1.0.0","capabilities":["commands"]})
emit({"type":"register_command","name":"hellopy","description":"say hi (python)"})

for line in sys.stdin:
    msg = json.loads(line)
    if msg["type"] == "command_invoked":
        emit({"type":"command_response","id":msg["id"],"action":"prompt",
              "prompt": "Greet me very briefly. Add one emoji."})
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

`chmod +x hello.py`, install:

```bash
zot ext install ./hello-py
```

Restart `zot`, type `/hellopy`, the agent greets you. Done.

## Layout & discovery

zot scans two directories on startup, in this order:

1. **Project-local**: `./.zot/extensions/<name>/extension.json`
2. **Global**: `$ZOT_HOME/extensions/<name>/extension.json`

A project-local extension with the same name wins over a global one.
On macOS `$ZOT_HOME` defaults to `~/Library/Application Support/zot/`;
on Linux it's `$XDG_STATE_HOME/zot` or `~/.local/state/zot`.

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
| `enabled` | optional, defaults to `true`. set to `false` to disable without removing. |

## Lifecycle

1. **Discovery**: zot reads every `extension.json` in the search dirs.
2. **Spawn**: enabled extensions are launched as subprocesses. stderr
   redirects to `$ZOT_HOME/logs/ext-<name>.log` (one file per
   extension, append-mode).
3. **Hello handshake**: the extension sends a `hello` frame; zot
   replies with `hello_ack` containing the protocol version and the
   active provider/model/cwd.
4. **Registration**: the extension sends `register_command` frames.
   First-come-first-served: a name already taken by a built-in or by
   a previously-loaded extension is silently shadowed (logged in the
   extension's own log file).
5. **Runtime**: zot dispatches `command_invoked` frames when the
   user runs a registered command; the extension responds with
   `command_response`. Extensions can also push `notify` frames at
   any time.
6. **Shutdown**: when zot exits, it sends `shutdown` and waits up to
   2s for the extension to send `shutdown_ack`. Holdouts are
   SIGTERM'd, then SIGKILL'd.

A crashing extension does not bring down zot. The slash command it
owned simply stops working until the extension is fixed and zot is
restarted.

## Wire format

All frames are one JSON object per line. Top-level `type` is the
discriminator. Optional `id` correlates request frames with their
responses.

### Extension → host

#### `hello` (required, first frame)

```json
{"type":"hello","name":"weather","version":"1.0.0",
 "capabilities":["commands","tools"]}
```

#### `register_command`

```json
{"type":"register_command","name":"weather",
 "description":"current weather for a city"}
```

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
`is_error: true` to mark the call as failed.

```json
{"type":"tool_result","id":"...",
 "content":[{"type":"text","text":"Berlin: 16°C, fog"}]}
```

#### `subscribe`

Declares which lifecycle events the extension wants to observe and
which it wants to intercept. Send once after `hello`, before `ready`.

```json
{"type":"subscribe",
 "events":["session_start","turn_start","tool_call","turn_end","assistant_message"],
 "intercept":["tool_call"]}
```

Recognised event names: `session_start`, `turn_start`, `turn_end`,
`tool_call`, `assistant_message`. Only `tool_call` is interceptable
in this version; other names listed under `intercept` are ignored.

#### `event_intercept_response`

Reply to an `event_intercept` from the host. `block: true` refuses
the action; `reason` is shown to the model as the tool error text.
Missing the response within 5s is treated as "allow" (i.e. an
unresponsive extension never stalls the agent).

```json
{"type":"event_intercept_response","id":"...",
 "block":true,"reason":"refused: matches danger pattern \"rm -rf\""}
```

#### `command_response` (reply to `command_invoked`)

```json
{"type":"command_response","id":"...","action":"prompt",
 "prompt":"Show today's weather for Berlin in one line."}
```

`action` is one of:

- `"prompt"` — submits `prompt` as a fresh user message; the agent
  runs a turn against it.
- `"insert"` — inserts `insert` into the editor at the cursor without
  submitting.
- `"display"` — appends `display` to the chat as a one-shot styled
  note. No model call, nothing written to the transcript.
- `"noop"` — the extension handled it itself (e.g. it pushed
  `notify` frames or kicked off background work). zot doesn't change
  the UI in response.

If `error` is non-empty, zot renders it as a red status line
regardless of `action`.

#### `notify` (one-way, any time)

```json
{"type":"notify","level":"info",
 "message":"refreshed cache (12 entries)"}
```

`level` is one of `info`, `success`, `warn`, `error`. The note shows
up below the transcript with the extension's name in brackets.

#### `shutdown_ack`

Sent in response to `shutdown`. Extension should exit promptly after.

### Host → extension

#### `hello_ack`

```json
{"type":"hello_ack","protocol_version":1,
 "zot_version":"0.0.7","provider":"anthropic",
 "model":"claude-opus-4-7","cwd":"/Users/pat/Developer/zot"}
```

Sent immediately after `hello`. The extension can use these fields to
decide which commands to register (e.g. only register a Python tool
on macOS, only register a model-specific shortcut for opus, etc.).

#### `command_invoked`

```json
{"type":"command_invoked","id":"...",
 "name":"weather","args":"berlin"}
```

`args` is everything the user typed after the command name, trimmed.

#### `tool_call`

Sent when the LLM invokes a tool the extension registered. `args` is
the parsed JSON object the model produced; the extension is
responsible for validating/coercing it.

```json
{"type":"tool_call","id":"...","name":"weather",
 "args":{"city":"Berlin"}}
```

Reply with `tool_result` within the host's tool timeout (default 60s).
Missing the timeout surfaces an error to the model and the call is
marked as failed.

#### `event`

Lifecycle notification for events the extension subscribed to via
`subscribe`. One-way — no response expected.

```json
{"type":"event","event":"turn_start","step":1}
{"type":"event","event":"tool_call",
 "tool_id":"...","tool_name":"read","tool_args":{"path":"foo.go"}}
{"type":"event","event":"turn_end","stop":"end_turn"}
```

#### `event_intercept`

Sent when zot wants to give the extension a chance to block a
lifecycle event before it happens. Same payload shape as `event`.
Reply with `event_intercept_response` within 5s; missing the deadline
is treated as "allow".

Only `event: "tool_call"` is sent in this version.

```json
{"type":"event_intercept","id":"...","event":"tool_call",
 "tool_id":"...","tool_name":"bash",
 "tool_args":{"command":"rm -rf /tmp/foo"}}
```

#### `shutdown`

Sent during graceful zot exit (or `/reload-ext` once that lands).
Reply with `shutdown_ack` and then exit.

## Managing extensions from the CLI

```
zot ext list                    list installed extensions and their state
zot ext install <path|git-url>  copy / clone into $ZOT_HOME/extensions/
zot ext remove <name>           delete an extension directory
zot ext enable <name>           re-enable a disabled extension
zot ext disable <name>          disable without removing
zot ext logs <name> [-f]        cat / tail the extension's stderr
```

`zot ext install <path>` does a recursive copy; `<git-url>` does a
shallow clone. Both validate that the destination contains an
`extension.json` and roll back if not.

## SDKs

Writing the wire protocol by hand is fine for one-off scripts, but
for anything bigger the SDKs handle the boilerplate.

### Go — `pkg/zotext`

```go
package main

import (
    "encoding/json"
    "github.com/patriceckhart/zot/pkg/zotext"
)

func main() {
    ext := zotext.New("hello", "1.0.0")

    // Slash command
    ext.Command("hello", "say hi", func(args string) zotext.Response {
        return zotext.Prompt("Greet me in one short sentence.")
    })

    // LLM-callable tool
    ext.Tool("weather", "Current weather for a city.",
        json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
        func(args json.RawMessage) zotext.ToolResult {
            var in struct{ City string `json:"city"` }
            json.Unmarshal(args, &in)
            return zotext.TextResult(in.City + ": sunny")
        })

    ext.Run()
}
```

Build with `go build -o hello .`, drop the binary + an `extension.json`
into `$ZOT_HOME/extensions/hello/`.

See:
- `examples/extensions/hello/` — slash commands
- `examples/extensions/clock/` — slash commands in plain Node, no SDK
- `examples/extensions/weather/` — LLM-callable tool
- `examples/extensions/guard/` — event subscriptions + tool-call
  interception (refuses dangerous bash patterns)

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
- [x] `notify`
- [x] `zot ext` CLI

Phase 2 (shipped):
- [x] `register_tool` + `tool_call` + `tool_result`
- [x] `ready` sentinel for safe agent-registry build timing
- [x] tool result attribution surfaces extension name in details

Phase 3 (shipped):
- [x] event subscriptions (`session_start`, `turn_start`, `turn_end`,
      `tool_call`, `assistant_message`)
- [x] tool-call interception (block before execution)

Future (no firm timeline):
- [ ] interception for additional events beyond `tool_call`
- [ ] modify (not just block) tool args mid-flight
- [ ] `/reload-ext` slash command (hot-reload without restarting zot)
