# scratchpad — TypeScript extension example

Real `.ts` (not `.js`), no build step, no SDK. Runs via `npx tsx`,
which downloads itself into the npm cache on first invocation and
runs from cache on every subsequent call.

Demonstrates:

- registering slash commands (`/note`, `/notes`, `/clear-notes`)
- registering an LLM-callable tool (`read_notes`)
- the wire protocol from a typed TypeScript perspective
- running TypeScript via `npx tsx` from `extension.json`

## Requirements

Node 18+ and `tsx` on `$PATH`:

```bash
npm install -g tsx
```

This is what `extension.json` invokes (`exec: "tsx"`). Without the
global install, swap to `"exec": "npx"` + `"args": ["--yes", "tsx",
"index.ts"]` — functional but adds ~1 second to each zot startup
because npm checks the registry on every invocation.

## Install

From this directory:

```bash
zot ext install .
```

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

- `examples/extensions/clock` — JS sibling (no tsx required)
- `examples/extensions/hello` — Go SDK
- `examples/extensions/weather` — Go SDK, exposes one tool
- `examples/extensions/guard` — Go SDK, demonstrates intercepts
- `docs/extensions.md` — full protocol reference
