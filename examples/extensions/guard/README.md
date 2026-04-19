# guard — example zot extension (Go, phase 3)

Demonstrates the **event subscription** and **tool-call interception**
half of the extension protocol (phase 3).

What it does:

- Subscribes to `session_start`, `turn_start`, `tool_call`, `turn_end`
  and appends a line to `/tmp/zot-guard-audit.log` for each.
- Intercepts every `bash` tool call. If the command matches a danger
  regex (`rm -rf`, `sudo`, `dd of=/`, `mkfs`, the fork bomb, `chmod -R
  777`), the call is **refused**. The model sees the refusal as the
  tool error and (typically) proposes something safer or asks for
  confirmation.

## Build

```bash
cd examples/extensions/guard
go build -o guard .
```

## Install

```bash
zot ext install .
```

## Try it

In zot, ask:

> Run `rm -rf /tmp/foo`

The model's `bash` call is intercepted and refused; the model
explains the refusal in its reply. No file is touched.

> Run `ls /tmp`

Allowed; the audit log records the call.

Tail the audit log:

```bash
tail -f /tmp/zot-guard-audit.log
```

## Extending the danger list

Edit `dangerPatterns` in `main.go`. Each entry is a Go regexp; the
match is case-insensitive. Rebuild and reinstall.

## See also

- `examples/extensions/hello` — slash command (phase 1)
- `examples/extensions/clock` — slash command in plain Node (phase 1)
- `examples/extensions/weather` — LLM-callable tool (phase 2)
- `docs/extensions.md` — full protocol reference
