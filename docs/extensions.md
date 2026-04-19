# zot extensions

zot can be extended with custom slash commands by running an external
program as a subprocess and exchanging newline-delimited JSON over
its stdin/stdout. Extensions can be written in **any language** that
can read and write JSON lines from stdio — Go, TypeScript, Python,
Rust, shell with `jq`, anything.

This is **phase 1**: slash commands and chat notifications. Future
phases will add tools the model can call, lifecycle event subscriptions,
and tool-call interception.

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
discriminator. Optional `id` correlates command invocations with
their responses.

### Extension → host

#### `hello` (required, first frame)

```json
{"type":"hello","name":"weather","version":"1.0.0",
 "capabilities":["commands"]}
```

#### `register_command`

```json
{"type":"register_command","name":"weather",
 "description":"current weather for a city"}
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

import "github.com/patriceckhart/zot/pkg/zotext"

func main() {
    ext := zotext.New("hello", "1.0.0")
    ext.Command("hello", "say hi", func(args string) zotext.Response {
        return zotext.Prompt("Greet me in one short sentence.")
    })
    ext.Run()
}
```

Build with `go build -o hello .`, drop the binary + an `extension.json`
into `$ZOT_HOME/extensions/hello/`.

See `examples/extensions/hello/` for the full working example.

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

Phase 1 (this document):
- [x] subprocess lifecycle + hello handshake
- [x] `register_command` + `command_invoked`
- [x] `notify`
- [x] `zot ext` CLI

Phase 2:
- [ ] `register_tool` + `tool_call` + `tool_result` (extension-defined
      tools that the LLM can call)
- [ ] tool result rendering with extension attribution

Phase 3:
- [ ] event subscriptions (`turn_start`, `turn_end`, `tool_call_*`,
      `session_start`, etc.)
- [ ] tool-call interception (block / modify before execution)
- [ ] `/reload-ext` slash command (hot-reload without restarting zot)
