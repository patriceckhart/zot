# scratchpad — example zot extension (TypeScript, source-run)

Real `.ts` (not `.js`), no build step, no SDK. Runs via `npx -y tsx
./index.ts`, which downloads `tsx` into npm's cache on first
invocation and then reuses the cached copy on subsequent runs.

Demonstrates:

- registering slash commands (`/note`, `/notes`, `/clear-notes`)
- registering an LLM-callable tool (`read_notes`)
- the wire protocol from a typed TypeScript perspective
- running TypeScript via `npx tsx` from `extension.json`

## Requirements

Node 18+ and `npx` (bundled with npm).

## Install

From this directory:

```bash
zot ext install .
```

This copies the manifest + source into `$ZOT_HOME/extensions/scratchpad/`.

## Use

In zot:

- `/note remind me to update the changelog`  — appends to the scratchpad
- `/notes`                                    — shows everything stored
- `/clear-notes`                              — wipes the scratchpad

The model also has a `read_notes` tool. Ask it:

> "What did I tell you to remember?"

…and it will call the tool and tell you.

## Storage

Notes persist as JSONL at `<cwd>/.zot/scratchpad-notes.jsonl`. The
file is created on first `/note` and survives zot restarts. Each line
is one note: `{"at":"2026-04-19T13:00:00.000Z","text":"..."}`.

Scope is per-project: switching to a different cwd gives you a
different scratchpad. Cross-project sharing isn't supported in this
example (would just be a matter of changing the path constant).

## Why TypeScript here

The extension protocol is small enough that you can hand-write it in
any language. JS works fine; TS adds type safety on the frame shapes
without any infrastructure beyond `tsx`. If you want richer ergonomics
(decorators, schema-from-types), publish your own SDK on top.

## See also

- `examples/extensions/clock` — JavaScript sibling (no build step)
- `examples/extensions/hello` — Go SDK slash commands
- `examples/extensions/weather` — Go SDK tool example
- `examples/extensions/guard` — Go SDK intercept example
- `examples/extensions/todo` — interactive panel example
- `docs/extensions.md` — full protocol reference
