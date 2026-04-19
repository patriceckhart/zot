---
name: write-zot-extension
description: Help the user create a new zot extension (slash command, LLM tool, or guard) in any language.
---

# Writing a zot extension

Use this skill when the user asks for help building a zot extension —
a new slash command, a new tool the LLM can call, an audit hook, or
a permission gate. Skim this whole skill first, then collaborate
with the user on the specific extension they want.

## What an extension is

A zot extension is **an external executable** that zot launches as a
subprocess and talks to over its stdin/stdout in newline-delimited
JSON. It can be written in any language that can read/write JSON
lines from stdio: Go, TypeScript (via tsx), Python, Rust, shell with
jq, anything. Crash isolation is automatic; one bad extension never
takes down zot.

Three things an extension can do (any combination):

1. **Slash commands** — register `/foo` so the user can run it from
   the input. The handler returns a "prompt" (submitted to the
   agent), an "insert" (text dropped into the editor), a "display"
   (one-shot styled note in the chat), or a "noop".

2. **Tools** — register tools the LLM itself calls. Schema is
   JSON Schema; zot routes the model's `tool_call` to the
   extension's `tool_result`. Same lifecycle as built-in tools
   (read/write/edit/bash/skill).

3. **Lifecycle hooks** — subscribe to events
   (session_start, turn_start, tool_call, turn_end,
   assistant_message) for telemetry / audit / custom UI, or
   intercept tool calls before execution to refuse dangerous
   patterns.

## On-disk layout

Each extension lives in its own directory:

```
~/Library/Application Support/zot/extensions/<name>/
├── extension.json    # manifest (required)
└── <executable>      # whatever exec points at
```

Or project-local: `<project>/.zot/extensions/<name>/`. Project-local
wins on name conflict.

For ad-hoc use during development, skip the install step entirely
and run `zot --ext PATH` (repeatable: `-e PATH -e PATH`).

### Manifest

```json
{
  "name": "weather",
  "version": "1.0.0",
  "exec": "./weather",
  "args": [],
  "language": "go",
  "description": "current weather lookups for any city",
  "enabled": true
}
```

Field rules:
- `name` (required, unique) — id zot uses internally; matches the
  hello frame. Slash commands & tools live in the same name space
  as built-ins; conflicts are silently shadowed by built-ins.
- `exec` (required) — the executable path. Resolution:
  - absolute: as-is
  - starts with `./` or `../`: relative to the manifest's directory
  - bare name (no separator): looked up via `$PATH` (e.g. `node`,
    `python3`, `npx`, `tsx`)
- `args` — extra argv passed to `exec` (e.g. `["index.js"]`)
- `language` — informational only (`"go"`, `"typescript"`,
  `"python"`, etc.)
- `enabled` — defaults to true; set false to keep installed but skip

## Wire format

Newline-delimited JSON in both directions. Top-level `type` is the
discriminator. Optional `id` correlates command/tool requests with
their responses.

### Required handshake

The very first frame the extension sends is `hello`:

```json
{"type":"hello","name":"weather","version":"1.0.0",
 "capabilities":["commands","tools"]}
```

Capabilities are advisory; current values are `commands`, `tools`,
`events`. Send all that apply.

zot replies with `hello_ack`:

```json
{"type":"hello_ack","protocol_version":1,"zot_version":"0.0.x",
 "provider":"anthropic","model":"claude-opus-4-7","cwd":"/path/to/project"}
```

### Registration (immediately after hello)

Send registration frames in any order, then a single `ready`
sentinel so zot can finalize the agent's tool registry:

```json
{"type":"register_command","name":"weather","description":"current weather"}
{"type":"register_tool","name":"weather","description":"Get current weather for a city.",
 "schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}
{"type":"subscribe","events":["tool_call"],"intercept":["tool_call"]}
{"type":"ready"}
```

If you don't send `ready`, zot's idle watchdog auto-treats you as
ready after 250ms of no frames, but always send it explicitly when
you can — newer extensions on faster hosts shave that 250ms off.

### Runtime frames

**zot → extension:**

```json
{"type":"command_invoked","id":"abc","name":"weather","args":"berlin"}
{"type":"tool_call","id":"def","name":"weather","args":{"city":"Berlin"}}
{"type":"event","event":"turn_start","step":1}
{"type":"event_intercept","id":"ghi","event":"tool_call",
 "tool_name":"bash","tool_args":{"command":"rm -rf /tmp/foo"}}
{"type":"shutdown"}
```

**extension → zot (replies + spontaneous notifications):**

```json
{"type":"command_response","id":"abc","action":"prompt",
 "prompt":"Show today's weather for Berlin in one line."}
{"type":"tool_result","id":"def","content":[{"type":"text","text":"Berlin: 16°C, fog"}]}
{"type":"event_intercept_response","id":"ghi","block":true,
 "reason":"refused: command matches the danger pattern \"rm -rf\""}
{"type":"notify","level":"info","message":"refreshed cache"}
{"type":"shutdown_ack"}
```

`command_response.action` values:
- `"prompt"` — submit `prompt` as a fresh user message
- `"insert"` — drop `insert` into the editor at the cursor
- `"display"` — append `display` to chat as a one-shot note (no
  model call, not in transcript)
- `"noop"` — handled internally; zot doesn't change the UI

`tool_result.content[]` blocks: `{"type":"text","text":"..."}` or
`{"type":"image","mime_type":"image/png","data":"<base64>"}`.

Per-tool timeout: 60s. Per-intercept timeout: 5s. Missing the
intercept timeout is treated as "allow" so an unresponsive guard
never stalls the agent.

## Important rules

- **stdout is reserved for the protocol.** Anything you print to
  stdout that isn't a JSON frame breaks the wire. Use stderr for
  logs / debug output (zot captures stderr to
  `$ZOT_HOME/logs/ext-<name>.log`).
- **One JSON object per line.** No multi-line JSON. Always end
  every frame with `\n`.
- **Flush after writing.** Most stdout writes are line-buffered when
  piped, which is fine, but explicitly flushing avoids surprise
  buffering on slow handlers.
- Extension processes inherit the user's permissions. A bad
  extension can do anything the user can.

## Recommended layout per language

### Go (use the built-in SDK at pkg/zotext)

```go
package main

import (
    "encoding/json"
    "github.com/patriceckhart/zot/pkg/zotext"
)

func main() {
    ext := zotext.New("weather", "1.0.0")

    ext.Command("weather", "current weather for a city",
        func(args string) zotext.Response {
            return zotext.Prompt("Tell me the weather for " + args)
        })

    ext.Tool("weather", "Get current weather for a city.",
        json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
        func(args json.RawMessage) zotext.ToolResult {
            var in struct{ City string `json:"city"` }
            if err := json.Unmarshal(args, &in); err != nil {
                return zotext.TextErrorResult("invalid args")
            }
            return zotext.TextResult(in.City + ": sunny, 21°C (fake)")
        })

    if err := ext.Run(); err != nil {
        ext.Logf("fatal: %v", err)
    }
}
```

Build: `go build -o weather .`

`extension.json`:
```json
{"name":"weather","version":"1.0.0","exec":"./weather","language":"go","enabled":true}
```

### TypeScript (no SDK; handles the protocol directly)

Run via `tsx`, which executes `.ts` files without a build step.

```json
{"name":"scratchpad","version":"1.0.0","exec":"tsx","args":["index.ts"],"language":"typescript","enabled":true}
```

```typescript
// index.ts (excerpt; see examples/extensions/scratchpad/index.ts for the full version)
import { createInterface } from "node:readline";
import { stderr, stdin, stdout } from "node:process";

function send(o: object) { stdout.write(JSON.stringify(o) + "\n"); }
function log(s: string) { stderr.write(`[scratchpad] ${s}\n`); }

send({ type: "hello", name: "scratchpad", version: "1.0.0",
       capabilities: ["commands", "tools"] });
send({ type: "register_command", name: "note", description: "append a note" });
send({ type: "register_tool", name: "read_notes",
       description: "Read the user's scratchpad notes.",
       schema: { type: "object", properties: {} } });
send({ type: "ready" });

const rl = createInterface({ input: stdin, crlfDelay: Infinity });
rl.on("line", (line) => {
  const f = JSON.parse(line);
  if (f.type === "command_invoked" && f.name === "note") {
    send({ type: "command_response", id: f.id, action: "display",
           display: `noted: ${f.args}` });
  } else if (f.type === "tool_call" && f.name === "read_notes") {
    send({ type: "tool_result", id: f.id,
           content: [{ type: "text", text: "(notes go here)" }] });
  } else if (f.type === "shutdown") {
    send({ type: "shutdown_ack" });
    rl.close();
  }
});
```

`tsx` install: `npm install -g tsx`. Without global tsx, fall back
to `"exec":"npx","args":["--yes","tsx","index.ts"]` (slower
startup; npx checks the registry every launch).

### Python

```json
{"name":"hello-py","version":"1.0.0","exec":"./hello.py","language":"python","enabled":true}
```

```python
#!/usr/bin/env python3
import json, sys

def emit(o): sys.stdout.write(json.dumps(o) + "\n"); sys.stdout.flush()

emit({"type": "hello", "name": "hello-py", "version": "1.0.0", "capabilities": ["commands"]})
emit({"type": "register_command", "name": "hellopy", "description": "say hi (python)"})
emit({"type": "ready"})

for line in sys.stdin:
    msg = json.loads(line)
    if msg["type"] == "command_invoked":
        emit({"type": "command_response", "id": msg["id"],
              "action": "prompt", "prompt": "Say hi briefly."})
    elif msg["type"] == "shutdown":
        emit({"type": "shutdown_ack"})
        break
```

`chmod +x hello.py`.

## Install / dev workflow

```bash
zot ext install ./weather       # copy into $ZOT_HOME/extensions/
zot --ext ./weather             # run from disk for one zot session (no install)
zot --ext .                     # cwd is the extension dir
zot ext list                    # show installed extensions
zot ext logs weather            # cat the extension's stderr
zot ext logs weather -f         # tail it
zot ext disable weather         # keep installed but skip on launch
zot ext enable weather
zot ext remove weather
```

For TS / Python extensions, no build step is needed — edit the source
in place and relaunch zot.

For Go, run `go build -o <name> .` in the extension directory after
edits, then `zot ext install` (which copies the manifest + binary)
or `zot --ext .` to test from the working tree.

## Manual debug

The extension is just a process. Drive it directly with shell pipes
to see exactly what's happening on the wire:

```bash
{
  printf '%s\n' '{"type":"hello_ack","protocol_version":1,"zot_version":"x","provider":"a","model":"o","cwd":"/tmp"}'
  sleep 0.2
  printf '%s\n' '{"type":"command_invoked","id":"1","name":"weather","args":"Berlin"}'
  sleep 0.5
  printf '%s\n' '{"type":"shutdown"}'
} | ./weather
```

Compare what comes out of stdout to the expected wire format. If a
frame doesn't match what zot expects, it's discarded silently and
logged to `ext-<name>.log`.

## Process to follow with the user

1. Ask what the extension should DO. One sentence.
2. Pick the right capability:
   - "I want a slash command that triggers a prompt" → `command` only
   - "I want the model to be able to do X" → `tool`
   - "I want to gate / log every bash command" → `event` + `intercept`
3. Pick a language. Default to **Go via pkg/zotext** for new
   extensions if the user has Go installed; **TypeScript via tsx**
   if they prefer JS-flavored ergonomics; **Python** for one-off
   scripts.
4. Write the extension dir (manifest + source).
5. For Go, build it. For TS / Python, mark the script executable.
6. Suggest `zot --ext <path>` for testing without committing to an
   install.
7. When happy, `zot ext install <path>`.

Don't try to write a full SDK or framework on top of the protocol
unless the user asked for one — the wire format is small enough
that a 30-line raw script is the right answer for most extensions.
