# hello — example zot extension (Go)

A minimal example showing how to register slash commands with the
`zotext` SDK.

## Build

```bash
cd examples/extensions/hello
go build -o hello .
```

## Install

```bash
# from this directory
zot ext install .
```

This copies the directory (manifest + binary) into
`$ZOT_HOME/extensions/hello/`. zot picks it up the next time you
launch the TUI.

## Use

In zot, type:

- `/hello`           — agent greets you
- `/hello <name>`    — agent greets `<name>`
- `/summon`          — extension pushes a one-shot notice to the chat

## See also

- `pkg/zotext` — Go SDK
- `docs/extensions.md` — protocol reference for any-language extensions
