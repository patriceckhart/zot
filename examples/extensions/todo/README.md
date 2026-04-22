# todo — example zot extension (Go, interactive panel)

Demonstrates the interactive panel API plus a companion tool the model
can call. The slash command opens a persistent todo panel; the tool lets
zot read and update the same todo list.

## Requirements

Go 1.22+.

## Install

From this directory:

```bash
zot ext install .
```

The example is configured to run directly from source:

```json
{
  "exec": "go",
  "args": ["run", "."]
}
```

That avoids architecture-specific binaries when sharing the example, as
long as Go is installed.

## Optional local build

```bash
cd examples/extensions/todo
go build -o todo-panel .
```

If you do build it, change `extension.json` back to:

```json
{
  "exec": "./todo-panel"
}
```

## Use

In zot:

- `/todo` opens the panel

## Features

- `/todo` opens the panel
- panel keys:
  - up/down - move
  - a - add with typed text
  - e - edit selected todo
  - x - toggle done
  - d - delete
  - r - redraw
  - esc - close panel
- persistent storage in the extension data directory as `todos.json`
- LLM tool: `todo_manage`
  - list
  - add
  - complete
  - edit
  - remove

## Natural-language examples

- Create a new entry named "Call Georg" on my to-do list.
- Complete task "Call Georg".
- Edit task "Call Georg" to "Call Georg tomorrow".
- Remove task "Call Georg".

## See also

- `examples/extensions/hello` — Go SDK slash commands
- `examples/extensions/weather` — Go SDK tool example
- `examples/extensions/guard` — Go SDK intercept example
- `examples/extensions/scratchpad` — TypeScript commands + tool
- `docs/extensions.md` — full protocol reference
