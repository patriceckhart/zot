# clock — example zot extension (Node, no dependencies)

A minimal TypeScript-style extension showing the wire protocol from
the Node side without any SDK. Pure stdlib (`readline`, `process`).

## Requirements

Node 18 or newer (uses ESM). No `npm install` step.

## Install

From this directory:

```bash
zot ext install .
```

This copies the manifest + script into `$ZOT_HOME/extensions/clock/`.
zot picks it up the next time you launch the TUI.

## Use

In zot:

- `/now`              — extension pushes a styled note showing local
                        and ISO time (no model call)
- `/uptime`           — extension asks the agent to comment on how
                        long the clock extension has been running
- `/uptime caching`   — same, but the agent's comment is steered by
                        the trailing args

## Why JavaScript and not TypeScript

The file uses JSDoc types (`@typedef`, `@param`) so it type-checks
under `tsc --checkJs` without a build step. Authentic TypeScript
authoring works too — rename `index.js` → `index.ts`, install
[`tsx`](https://www.npmjs.com/package/tsx), and update
`extension.json`:

```json
{
  "exec": "npx",
  "args": ["-y", "tsx", "./index.ts"]
}
```

zot doesn't care which one you use; it just spawns whatever `exec`
points at and reads/writes JSON lines on its stdio.

## See also

- `examples/extensions/hello` — Go version using the `pkg/zotext` SDK
- `docs/extensions.md` — full protocol reference
