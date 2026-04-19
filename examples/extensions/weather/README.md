# weather — example zot extension (Go, tool-providing)

Demonstrates the **tool registration** half of the extension protocol
(phase 2). The extension exposes a `weather(city)` tool that the LLM
can invoke; the result is fake-but-deterministic so repeated calls
for the same city return the same thing.

## Build

```bash
cd examples/extensions/weather
go build -o weather .
```

## Install

```bash
zot ext install .
```

Copies the directory (manifest + binary) into
`$ZOT_HOME/extensions/weather/`. zot picks it up the next time you
launch the TUI and registers the tool with the agent.

## Use

In zot, ask:

- "What's the weather in Berlin?"
- "Compare the weather in Tokyo and Reykjavik."

The model decides on its own to call the `weather` tool because the
description tells it what the tool is for. You don't need to invoke
anything by hand.

## Add the leading slash to your /help table

The tool also shows up in the system prompt's tool list because zot
folds extension tools into the agent's registry at startup.

## See also

- `examples/extensions/hello` — slash-command extension (Go SDK)
- `examples/extensions/clock` — slash-command extension (Node, no SDK)
- `docs/extensions.md` — full protocol reference
