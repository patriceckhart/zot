<img src="internal/assets/zot-logo.png" alt="zot" width="130" height="130" />

# zot

Yet another coding agent harness, lightweight and written (vibe-slopped) in go.

- one static binary.
- built-in providers for anthropic, openai/codex, kimi, and ollama/openai-compatible local models.
- four tools (read, write, edit, bash).
- three run modes (interactive tui, print, json).
- built-in telegram bot.
- extensions in any language via subprocess + json-rpc. None installed by default; opt in with `zot ext install` or `zot --ext`. See [docs/extensions.md](docs/extensions.md).
- reusable instructions via `SKILL.md` files; see [docs/skills.md](docs/skills.md).
- no community atm.

## Install

### One-liner (macOS, Linux)

```bash
curl -fsSL https://www.zot.sh/install.sh | bash
```

Detects your OS and architecture, downloads the latest release from GitHub, verifies the SHA-256 against the release's `checksums.txt`, extracts the binary, and drops it in `/usr/local/bin`, `~/.local/bin`, or `~/bin`, whichever is writable first. Pass a version or prefix to pin:

```bash
curl -fsSL https://www.zot.sh/install.sh | bash -s -- v0.0.1 ~/bin
```

### One-liner (Windows, PowerShell)

```powershell
iwr -useb https://www.zot.sh/install.ps1 | iex
```

Drops `zot.exe` into `$HOME\bin` and adds it to the user PATH if missing. Open a fresh terminal afterwards.

### go install

```bash
go install github.com/patriceckhart/zot/cmd/zot@latest
```

### From source

```bash
git clone https://github.com/patriceckhart/zot
cd zot
make build        # produces ./bin/zot
make install      # into $GOPATH/bin
```

### Prebuilt binaries

Every release on the [releases page](https://github.com/patriceckhart/zot/releases) ships archives for Linux, macOS, and Windows on amd64 and arm64 (except windows/arm64), plus a `checksums.txt` file. Download, verify, `chmod +x`, and drop on your `$PATH`.

## Authenticate

The easiest way is to just run `zot` and type `/login`. The TUI opens even without credentials and walks you through a browser-based login flow.

### Credential lookup order

1. `--api-key` flag
2. `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `KIMI_API_KEY`, or `MOONSHOT_API_KEY` env var
3. `$ZOT_HOME/auth.json` (API key or OAuth token; mode 0600)

`$ZOT_HOME` defaults to:
- macOS: `~/Library/Application Support/zot`
- Linux: `$XDG_STATE_HOME/zot` or `~/.local/state/zot`
- Windows: `%LOCALAPPDATA%\zot`

### `/login` flow

Run `zot` and type `/login`. Pick one of two methods:

- **API key**: a small local web server starts on `127.0.0.1:<free-port>`, your browser opens a form, you paste your `sk-ant-...`, `sk-...`, or Kimi/Moonshot key. zot probes the provider once and saves it to `auth.json` if accepted.
- **Subscription**: use your Claude Pro/Max, ChatGPT Plus/Pro, or Kimi Code subscription.
  - Anthropic and OpenAI pin the browser callback to fixed provider-specific ports (`localhost:53692` for Anthropic, `localhost:1455` for OpenAI) because those are the only ports their auth servers will redirect to.
  - Anthropic uses the Claude Code OAuth flow. Messages go to `api.anthropic.com` with a bearer token and the Claude Code identity headers.
  - OpenAI uses the Codex CLI OAuth flow. Messages go to `chatgpt.com/backend-api/codex/responses` with the `chatgpt-account-id` extracted from the returned id_token.
  - Kimi uses the Kimi Code device-code OAuth flow. zot opens the verification URL, polls until you approve it in the browser, then sends messages to `api.kimi.com/coding/v1` with the Kimi Code identity headers.

> **Note on subscription login.** The OAuth client IDs used are the ones published in Anthropic's Claude Code CLI, OpenAI's Codex CLI, and Kimi Code CLI. Reusing them from a third-party tool may be against their terms of service and may be revoked at any time. Use it at your own risk; the API-key flow is the safe default.

### Token refresh

OAuth access tokens are short-lived (Anthropic ~8h, OpenAI ~30d). zot refreshes them automatically:

- At every credential lookup, zot checks the stored `expiry` and, if past it (with a 60s safety margin), hits the provider's `oauth/token` endpoint with the stored `refresh_token`, persists the new `access_token`, `refresh_token`, and `expiry` back to `auth.json`, and hands the fresh token to the client.
- The telegram bridge additionally refreshes once per turn so a bot that runs for days keeps working without manual intervention.
- If the refresh itself fails (the `refresh_token` was revoked, or the account was logged out everywhere), the error bubbles up to the caller: the TUI shows it in the status line, the bot replies with it in your DM. Run `/login` to get a fresh token pair.

All data lives under `$ZOT_HOME`:

```
$ZOT_HOME/
├── config.json         # last-used provider/model/theme, saved automatically
├── auth.json           # api keys and oauth tokens (mode 0600)
├── sessions/           # jsonl transcripts, one dir per cwd
├── models-cache.json   # live /v1/models discovery cache (6h ttl)
├── SYSTEM.md           # optional: replaces the default system prompt
├── skills/             # optional: user SKILL.md files (opt in with --with-skills)
├── extensions/         # installed extensions, one dir per extension
└── logs/               # app log files
```

Drop a `SYSTEM.md` in `$ZOT_HOME` to replace the built-in identity and guidelines for every run. `--system-prompt` still wins per-invocation. Delete the file to revert to the default.

## Changelog on update

The first time you launch a newer zot binary, the TUI shows the GitHub release notes once in a dismissible overlay. Press any key to close. The version is recorded in `config.json`'s `last_changelog_shown` so the same release notes never reappear. Fresh installs don't see a changelog (no upgrade has happened yet). The fetch is best-effort: a network failure or a missing release page silently skips, with another attempt on the next launch.

## Usage

```bash
zot                              # interactive tui
zot "fix the failing test"       # tui, pre-filled prompt
zot -p "list all go files"       # print final text, exit
zot --json "refactor main.go"    # newline-delimited json events, exit
zot --continue                   # resume the most recent session for this cwd
zot --resume                     # pick a session to resume
zot --list-models                # show supported models
zot --help
```

## Flags

| Flag | Description |
|---|---|
| `--provider anthropic\|openai\|kimi\|ollama` | Pick the provider. |
| `--model <id>` | Pick the model (see `--list-models`). |
| `--api-key <key>` | Override the API key. |
| `--base-url <url>` | Override the provider base URL (tests, self-hosted). |
| `--system-prompt <text>` | Replace the default system prompt for this run (also overrides `$ZOT_HOME/SYSTEM.md`). |
| `--append-system-prompt <text>` | Append text to the system prompt (repeatable). |
| `--reasoning low\|medium\|high` | Enable reasoning on supported models. |
| `-c`, `--continue` | Resume the latest session for this cwd. |
| `-r`, `--resume` | Pick a session to resume. |
| `--session <path>` | Resume a specific session file. |
| `--no-session` | Don't read or write session files. |
| `--cwd <path>` | Use `<path>` as the working directory. |
| `--no-tools` | Disable all tools. |
| `--tools <csv>` | Only enable the listed tools. |
| `--max-steps <n>` | Cap agent loop iterations (default 50). |
| `-e`, `--ext <path>` | Load an extension from `<path>` for this run (repeatable; wins against installed extensions of the same name). |
| `--no-ext` | Skip extension discovery for this run. `--ext` still works on top, so `--no-ext --ext ./x` runs only `x`. |
| `--with-skills` | Also load user-installed skills. Without this, only the built-in skills shipped in the binary are loaded. |
| `--no-skill` | Disable all skills, including built-ins. No `skill` tool is registered and the system prompt has no skill manifest. |
| `--no-yolo` | Confirm every tool call before it runs (interactive TUI only). A dialog shows the tool name and a one-line preview of its args with four choices: yes, yes-always-this-tool-this-session, yes-always-this-session, no. Ignored with a stderr warning in print / json / rpc modes, where tools still run freely so scripts and automation keep working. Type `/yolo` in the TUI to disable the gate for the rest of the session. |

## Tools

- `read`: read text files, or inline images (PNG, JPEG, GIF, WebP).
- `write`: create or overwrite files, making parent directories as needed.
- `edit`: one or more exact-match replacements in an existing file.
- `bash`: run a shell command in the session cwd, with merged stdout/stderr and a timeout.

When the sandbox is on (see `/jail`), all four tools refuse paths outside the session cwd.

## Modes

- **Interactive** (default): chat TUI with streaming output, spinner, cost meter, slash commands.
- **Print**: `zot -p "prompt"` runs the agent to completion and writes only the final assistant text to stdout.
- **JSON**: `zot --json "prompt"` emits one JSON object per agent event to stdout, newline-delimited. The schema is documented in [docs/rpc.md](docs/rpc.md).
- **RPC**: `zot rpc` runs as a long-lived child process; commands in on stdin, events and responses out on stdout, both as NDJSON. Designed for embedding zot in third-party apps written in any language. See [docs/rpc.md](docs/rpc.md) for the wire schema and `examples/rpc/{python,node,shell,go}` for working clients.

## Embedding

Two ways to drive zot from another program:

- **Go in-process**: import `github.com/patriceckhart/zot/pkg/zotcore`. One `Runtime` per project; `Prompt(ctx, text, images)` returns a channel of `Event`. Small example in `examples/sdk/`.
- **Any language, out-of-process**: spawn `zot rpc` as a subprocess and exchange newline-delimited JSON over its stdin/stdout. Wire format and event schema in [docs/rpc.md](docs/rpc.md). Reference clients live under `examples/rpc/`.

Both interfaces share the same event schema, so transcripts captured by one can be replayed through the other.

## Slash commands

Type `/` in the TUI to open the autocomplete popup. Available commands:

| Command | Description |
|---|---|
| `/help` | Show key bindings and commands. |
| `/login` | Log in via API key or subscription (opens a dialog). |
| `/logout [provider]` | Clear credentials for `anthropic`, `openai`, `kimi`, or all when omitted. `/logout kimi` also disables fallback to the official Kimi Code CLI token until you log in to Kimi through zot again. |
| `/model` | Pick a model from a list (or `/model <id>` to set directly). |
| `/sessions` | Resume a previous session for this directory. |
| `/session` | Four ops on the current session: `export` to a portable `.zotsession` file, `import` one back in, `fork` from a past user message into a new branch, `tree` to switch between branches. Opens a picker without an argument; direct forms: `/session export [path]`, `/session import <path>`, `/session fork`, `/session tree`. Default export destination is `~/Downloads`. |
| `/jump` | Scroll the chat to a previous turn (or `/jump <text>` to filter). |
| `/btw` | Side chat with full context that doesn't add to the main thread. |
| `/skills` | List discovered skills (SKILL.md files) and preview their bodies. |
| `/compact` | Summarize the transcript into one message to free up context. |
| `/study` | Run the canned prompt "Read and understand everything in the current directory." so the agent has full project context before you start asking targeted questions. |
| `/jail` | Confine tools to the current directory. |
| `/unjail` | Allow tools to touch paths outside again. |
| `/reload-ext` | Hot-reload all extensions (re-read manifests, respawn subprocesses, rebuild tool registry). |
| `/yolo` | Turn off `--no-yolo` confirmation for the rest of this session. |
| `/telegram` | Connect, disconnect, or show status of the Telegram bridge (takes `connect` / `disconnect` / `status` as an optional argument; opens a picker without one). When connected, DMs from the paired user become prompts in the running session and the assistant's replies are mirrored back to Telegram. Alias: `/tg`. |
| `/clear` | Clear the chat transcript. |
| `/exit` | Exit zot. |

Extension-registered commands appear under a divider at the bottom of the popup, sorted by name.

### `/sessions`

Shows previous sessions for the current working directory, newest first, with timestamp, model, message count, cost, and the first user prompt. Pick one with `up`/`down`, `enter` to resume, `esc` to cancel. zot swaps the current session file for the selected one and replays the full transcript (including tool calls) into the agent. Sessions remember the model they ended on, so resuming picks up on that exact model even if your global default changed.

### `/session`

Four ops on the current session. `/session` alone opens a picker; each is also runnable directly.

- **`/session export [path]`**. Writes the running transcript to a portable `.zotsession` file. Default destination is `~/Downloads/<timestamp>-<session-id>-<prompt-slug>.zotsession`. Pass a path to override; a directory is fine (a dated name is built inside), a bare name gets `.zotsession` appended. The meta's cwd is stripped on the way out so the recipient doesn't see your filesystem layout.
- **`/session import <path>`**. Copies a `.zotsession` file into `$ZOT_HOME/sessions/<cwd-hash>/` with a fresh id and the current cwd, then switches the running agent onto it. Imported sessions are first-class: they show up in `/sessions`, `/jump`, and the tree. Drag-drop paths in the editor are accepted (zot strips the surrounding quotes automatically).
- **`/session fork`**. Opens a turn picker (same shape as `/jump`). Pick any past user message; zot copies every message up to and including that turn into a new session, records `parent` + `fork_point` in the new meta, and switches onto the branch. The parent session stays on disk. Use it to try a different question without polluting the original transcript, or to rewind after the agent went down the wrong path.
- **`/session tree`**. Shows every session in the current cwd arranged by parent/child relationships, depth-first with indent per level. The current session is tagged `[current]`. Pick any entry to switch into it. Parentless sessions are roots; branches created via `/session fork` nest under whichever session they were forked from. Orphaned children (whose parent file was deleted) still show as roots so they stay discoverable.

### `/jump`

Opens a turn picker for the current session, one row per user prompt, each showing the turn number, how many tools that turn invoked, and the first line of the prompt. `up`/`down` to pick, `enter` to jump, `esc` to cancel. Any printable rune while the picker is open extends a filter; backspace narrows it back. `/jump <text>` pre-applies the filter; if exactly one turn matches, zot jumps straight there without showing the picker.

Jumping is non-destructive. The transcript is untouched, the viewport just scrolls so the chosen turn is at the top. A muted line at the top of the chat reads `viewing turn N of M, pgdn to catch up`. Scroll back to the bottom with `pgdn` (or keep scrolling with the arrow keys) and the indicator goes away.

### `/btw`

Opens a side-chat overlay with the full main session as frozen context, so you can ask quick clarifying questions ("does asyncio.gather() catch exceptions?", "btw the bundle budget is 10MB", "what's the default fetch timeout?") without bloating the main thread.

Each question fires a one-off model call against `system + main transcript + side-chat history so far`. Responses render in the overlay and stay there. When you press `esc` to close, **nothing** has been added to the main session and subsequent main-thread turns don't re-read any of the side-chat exchanges, keeping the running context window lean.

```
/btw                              # open the overlay, type questions interactively
/btw does PUT replace the whole resource?
```

Inside the overlay: `enter` sends, `esc` cancels an in-flight call (or closes the overlay if idle), `ctrl+c` closes immediately. Side-chat exchanges never touch the transcript and aren't persisted to the session file.

### `/skills`

Opens a picker listing every discovered SKILL.md file, built-ins hidden. Each row shows the skill name, source, and description. `enter` opens the body inline (scrollable with `up`/`down`/`pgup`/`pgdn`); `esc` goes back. Re-runs discovery each time it opens, so edits to a SKILL.md during a session are reflected immediately.

### `/compact`

Sends the current transcript through the model with a structured summarization prompt. The returned summary replaces the transcript as one synthetic user message, with the last few exchanges kept verbatim for continuity. The status bar's context meter resets. Use it when the context meter creeps past ~80%.

zot also auto-compacts in the background: after any turn that leaves context usage at or above **85%** of the model's window, the agent kicks off a condense pass on its own. You'll see `condensing history, esc to cancel` above the status bar and an `(auto)` tag next to the context percentage; `esc` aborts it without touching the transcript.

### `/jail`

Enforces a sandbox rooted at the cwd shown in the status bar. `read`, `write`, and `edit` resolve their target path (including through symlinks) and refuse anything outside the sandbox. `bash` refuses obvious escape patterns: `sudo`, `rm -rf /`, leading `cd /`, `cd ..`, `cd ~`, `chmod -R`, `dd of=/`, and similar. The status bar shows `jailed, ~/your/cwd` while active.

This is a guardrail against accidents, not a hard security boundary. If you need real isolation, run zot under docker or a proper sandbox.

## Sessions

Every interactive or print/json run (unless `--no-session`) writes a JSONL transcript under `$ZOT_HOME/sessions/<cwd-hash>/`. Resume any of them with `--continue`, `--resume`, `--session <path>`, or interactively via `/sessions` inside the TUI. Empty sessions (the user exited without prompting) are deleted on close so the list stays tidy.

## Models

`--list-models` or the `/model` picker shows the full catalog. Three sources:

- **Catalog**: models baked into zot, always available.
- **Live**: IDs discovered from `GET /v1/models` using your stored API key (cached for 6h in `$ZOT_HOME/models-cache.json`, refreshed in the background on startup).
- **Speculative**: IDs that appear in the upstream generator but aren't live on the public API yet. They'll 404 today and start working the moment the provider ships them.

The context meter in the status line uses the model's advertised context window to show how much of it your last turn consumed.

### Model fallback (rescue)

When a turn fails because of a recoverable provider error — expired token (`401`), permission denied (`403`), rate limit (`429`), provider outage (`502`/`503`/`504`), or a transient network failure — zot opens a **rescue** picker over the chat instead of just painting a red banner.

The picker is the same vertical list / fuzzy filter UI as `/model`, but it only shows models from providers you're currently logged in to (env vars, `auth.json`, Kimi CLI fallback, ollama). The failed model is excluded. Press `↑`/`↓` to choose, `enter` to retry the **same prompt** on the new model, `esc` to dismiss.

Before the actual provider request fires, the OpenAI / Anthropic / Kimi / OpenAI-Codex clients also do up to two silent retries with short backoff (250ms, 750ms) on `502`/`503`/`504` and connection-reset / EOF-before-headers errors. Most edge-proxy blips disappear without you ever seeing the rescue picker.

A rescue retry always **drops launch-time `--api-key` and `--base-url`** before rebuilding the agent. Those overrides are usually the reason the rescue triggered (bad key, typo'd base URL, corporate gateway only valid for the originally-picked provider), so the retry re-resolves credentials from env vars / `auth.json` / provider defaults instead. Use `/model` if you want overrides to stick.

No configuration is required — the candidate list is built dynamically from your active credentials. Bad-request / context-length / serialization errors are NOT routed to the rescue picker, because switching models won't fix them; those still surface as a normal error.

### Custom models

Place a `models.json` in `$ZOT_HOME` (macOS: `~/Library/Application Support/zot/`, Linux: `~/.local/state/zot/`) to add models that aren't in the baked-in catalog or to override existing entries:

```json
{
  "providers": {
    "openai": {
      "models": [
        {
          "id": "gpt-5.5",
          "name": "GPT-5.5",
          "reasoning": true,
          "contextWindow": 400000,
          "maxTokens": 128000
        }
      ]
    }
  }
}
```

Supported fields per model: `id` (required), `name`, `reasoning`, `contextWindow`, `maxTokens`, `baseUrl`, `priceInput`, `priceOutput`, `priceCacheRead`, `priceCacheWrite`.

Provider keys are normalized: `openai-codex` and `openai-responses` map to `openai`, `anthropic-messages` maps to `anthropic`, and `moonshot`, `moonshot-ai`, and `kimi-code` map to `kimi`.

User-defined models show `source: user` in `--list-models` and take precedence over both the baked-in catalog and live-discovered models. Missing or invalid files are silently ignored.

### Kimi Code

zot has built-in Kimi support through Kimi's OpenAI-compatible chat API.

```bash
zot --provider kimi
```

By default this uses:

- model: `kimi-for-coding`
- base URL: `https://api.kimi.com/coding/v1`

Credential lookup order for Kimi:

1. `--api-key`
2. `KIMI_API_KEY`
3. `MOONSHOT_API_KEY`
4. `$ZOT_HOME/auth.json`
5. the official Kimi Code CLI token at `~/.kimi/credentials/kimi-code.json`, unless disabled by `/logout kimi`

Use `/login` for either API-key login or Kimi Code subscription login. The subscription flow uses Kimi Code's device-code OAuth flow: zot opens the verification URL, waits for browser approval, stores the token in `auth.json`, and refreshes it automatically.

For direct Moonshot API keys or a custom compatible endpoint:

```bash
zot --provider kimi --model kimi-k2-0905-preview --base-url https://api.moonshot.ai/v1 --api-key "$KIMI_API_KEY"
```

You can add additional Kimi/Moonshot model IDs to `models.json` under the `kimi` provider.

### Local models with ollama

zot works with [ollama](https://ollama.com) out of the box. Ollama serves an OpenAI-compatible API locally, so any model you have pulled works with zot.

Quick start:

```bash
ollama pull qwen3.5:4b
zot --provider ollama --model qwen3.5:4b
```

That's it. No API key needed for local models. zot defaults to `http://localhost:11434`.

For a remote ollama instance or one behind auth:

```bash
zot --provider ollama --model llama3 --base-url https://my-server.com/v1 --api-key my-token
```

You can also add models to your `models.json` so you don't need flags every time:

```json
{
  "providers": {
    "ollama": {
      "models": [
        {
          "id": "qwen3.5:4b",
          "name": "Qwen 3.5 4B",
          "contextWindow": 32768,
          "maxTokens": 8192
        }
      ]
    }
  }
}
```

The `ollama` provider uses the OpenAI chat completions protocol internally, so it also works with any OpenAI-compatible server (vLLM, LM Studio, LocalAI, etc.).

## Inline images

When a tool returns an image (for example `read` on a PNG), zot renders it inline on terminals that support it: **Ghostty**, **Kitty**, **iTerm2**, **WezTerm**. On other terminals you see a text placeholder with MIME type, pixel dimensions, and byte size. Control with the `ZOT_INLINE_IMAGES` env var:

| Value | Effect |
|---|---|
| unset (default) | Auto-detect based on `TERM_PROGRAM`. |
| `iterm`, `iterm2` | Force the iTerm2 OSC 1337 protocol. |
| `kitty` | Force the Kitty graphics protocol. |
| `off`, `none` | Always use the text placeholder. |

Frames containing images are full-repainted (no differential diff) to prevent stale image pixels from lingering through scroll. That costs one terminal flash per image-containing frame; set `ZOT_INLINE_IMAGES=off` if that bothers you.

## Queued messages

You can keep typing while the agent is working. Pressing `enter` during a turn queues the message instead of interrupting: it shows up above the status bar as `sliding in: <text>` and is delivered as the next user turn the moment the current one finishes. Queue as many as you want; they run in order. `esc` cancels the active turn and drops the queue so a runaway turn doesn't flood you with stale follow-ups; `ctrl+c` while busy arms the exit hint instead of interrupting, a second `ctrl+c` within two seconds exits zot.

Slash commands also work while the agent is busy. Read-only ones (`/help`, `/jump`, `/btw`, `/sessions`, `/skills`, `/jail`, `/unjail`, `/exit`) take effect immediately. Destructive ones (`/clear`, `/compact`, `/login`, `/logout`, `/model`, `/reload-ext`) cancel the active turn first and then run.


## Keys (interactive mode)

### Input

| Key | Action |
|---|---|
| `enter` | Submit (queued if the agent is busy). |
| `alt+enter` | Newline. |
| `tab` | Complete the selected slash command. |
| `esc` | Cancel the current turn (while busy); clear input (while idle). |
| `ctrl+c` | Clear the input and queue (while idle) or arm the exit hint (while busy). Press again within 2s to exit. Use `esc` to cancel a running turn. |
| `ctrl+d` | Exit on empty input. |
| `ctrl+l` | Redraw the screen. |
| `ctrl+o` | Expand or collapse long tool results (read, write, edit, bash outputs over ~12 lines). |
| `@` | Open the file picker. Browse files and directories in the working directory. |

### File picker (`@`)

| Key | Action |
|---|---|
| `@` | Open the file picker (type after a space or at the start of input). |
| `up`, `down` | Navigate the file list. |
| `right` | Open the selected directory. |
| `left` | Go back to the parent directory. |
| `enter` | Select the file or directory and insert it as a chip (`[file:name]` or `[dir:name/]`). |
| `esc` | Close the file picker. |

Type `@` followed by a filter string to narrow the list (e.g. `@read` shows only entries containing "read"). Selected files are inserted as compact chips that expand to the full path on submit. Dragged-and-dropped files and directories also collapse to chips automatically.

### Editor line navigation

| Key | Action |
|---|---|
| `ctrl+a`, `ctrl+e` | Jump to start or end of line. |
| `alt+left`, `alt+right` | Jump one word back or forward. |
| `ctrl+u`, `ctrl+k` | Delete to start or end of line. |
| `ctrl+w`, `alt+backspace` | Delete the previous word. |
| `up`, `down` (editor non-empty) | Cycle through prompt history. |

### Chat scroll

| Key | Action |
|---|---|
| `pgup`, `pgdn` | Scroll one page up or down. |
| `up`, `down` (editor empty) | Scroll three lines up or down. This is how the mouse wheel reaches the scroll logic on most terminals. |

## Extensions

zot can be extended in any language via a subprocess + JSON-RPC protocol. Extensions can register slash commands, expose tools to the model, intercept tool calls (block or rewrite args), gate whole turns before the model is called, and rewrite the assistant's visible text before it reaches the user. None are installed by default; opt in explicitly. Hot-reload any time with `/reload-ext`.

### Install and manage

```bash
zot ext install <path|git-url>   # copy / clone into $ZOT_HOME/extensions/
zot ext list                      # show installed extensions
zot ext logs <name> [-f]          # cat or tail the extension's stderr log
zot ext enable <name>             # re-enable a disabled extension
zot ext disable <name>            # disable without removing
zot ext remove <name>             # delete an extension directory
```

For development, point `zot --ext <path>` at a working directory and skip the install step entirely. Repeatable; takes precedence over installed extensions of the same name.

### Reference

`examples/extensions/` ships reference implementations in Go, TypeScript, Node, and shell. See [docs/extensions.md](docs/extensions.md) for the full protocol, the SDK API (`pkg/zotext`), and the phase roadmap.

## Skills

A skill is a per-folder `SKILL.md` file with a YAML frontmatter header. zot discovers skills at startup, surfaces their names in the system prompt, and exposes a built-in `skill` tool the model uses to load the body on demand.

By default only the built-in skills shipped with the zot binary are loaded. Pass `--with-skills` to also load user-installed skills from:

- `./.zot/skills/<name>/SKILL.md` (project)
- `$ZOT_HOME/skills/<name>/SKILL.md` (global)
- `./.claude/skills/<name>/SKILL.md`, `~/.claude/skills/<name>/SKILL.md` (Claude-compatible layout)
- `./.agents/skills/<name>/SKILL.md`, `~/.agents/skills/<name>/SKILL.md` (agent-compatible layout)

See [docs/skills.md](docs/skills.md) for the frontmatter fields, authoring tips, and example skills under `examples/skills/`.

## Telegram bot (bridge)

zot can run as a telegram bot so you can DM it from your phone. Two ways to run it: **from inside the TUI** (the running session mirrors into Telegram) or **as a standalone background daemon** (a headless bot with its own independent agent).

### From inside the TUI

Type `/telegram` in the running TUI to open a picker with **connect**, **disconnect**, and **status**. When connected:

- DMs from the paired user become prompts in the **same** session you're typing in, so you can continue a conversation from the terminal on your phone and back again.
- Messages you type in the TUI are mirrored into the Telegram thread prefixed `you: …` and the assistant's replies come back prefixed `zot: …`, so the Telegram chat stays a complete record of both sides of the conversation.
- Messages sent from Telegram show up as your own bubble in Telegram (no mirror) and the assistant's reply to them comes back bare (no prefix).
- The status bar shows a `- tg -` tag while the bridge is active.
- `/telegram connect` / `/telegram disconnect` / `/telegram status` (or `/tg`) also work as direct commands without the picker.

The in-TUI bridge refuses to start while the standalone daemon (below) is running, since two concurrent long-poll consumers of the same bot race on every update and silently drop messages.

### Standalone daemon

For headless servers or long-running bots unattached to a TUI:

```bash
zot telegram-bot setup     # paste a BotFather token, verify, save
zot telegram-bot run       # foreground: long-poll in this terminal (ctrl+c to stop)
zot telegram-bot start     # background: detach and return immediately
zot telegram-bot stop      # SIGTERM the background bot (SIGKILL after 5s)
zot telegram-bot logs -f   # tail $ZOT_HOME/logs/bot.log (omit -f to just cat)
zot telegram-bot status    # config (token masked) + running/stopped
zot telegram-bot reset     # forget the token and paired user
# short alias: `zot tg ...` is accepted for every subcommand
```

The background flavor writes the child's PID to `$ZOT_HOME/bot.pid` and redirects stdout and stderr to `$ZOT_HOME/logs/bot.log`. `zot telegram-bot stop` reads that PID, sends SIGTERM, waits up to five seconds, then escalates to SIGKILL if the child is still alive. Running two instances at once is refused at startup.

> **Use the installed binary for `start`.** `go run ./cmd/zot telegram-bot start` won't work. `go run` builds a binary in a temp directory and deletes it when it exits, which kills the detached child. Run `make install` (or `go build`) first and invoke the installed binary.

Setup flow:

1. Talk to [@BotFather](https://t.me/BotFather) on telegram, run `/newbot`, copy the token it gives you.
2. Run `zot telegram-bot setup` and paste the token when prompted.
3. Run `zot telegram-bot run` in the directory you want the agent to operate in.
4. Open your bot on telegram, send `/start`. The first user to do this claims the bridge (stored as `allowed_user_id`); every other user is rejected.

From then on, any DM you send is forwarded to the agent as a user prompt. Attached photos or `image/*` documents are downloaded and passed to vision-capable models. In-bot telegram commands: `/help`, `/status`, `/stop` (cancel the current turn). Config lives in `$ZOT_HOME/bot.json` (mode 0600).

Bot mode respects the usual zot flags: `--provider`, `--model`, `--cwd`, `--reasoning`, `--continue`, `--no-session`, `--no-tools`, and so on. Run `zot tg run -c --model claude-opus-4-1` to resume the latest session on Opus, for example.

## Development

```bash
make build     # build ./bin/zot
make test      # go test -race ./...
make lint      # go vet + gofmt check
make fmt       # gofmt -w .
make release   # cross-compile linux/darwin/windows on amd64 and arm64
```

Source layout:

```
cmd/zot/                      main()
internal/agent/               cli wiring, arg parsing, system prompt, config
internal/agent/extensions/    extension subprocess manager
internal/agent/modes/         interactive tui, print, json, dialogs
internal/agent/tools/         read, write, edit, bash, sandbox
internal/auth/                credential store, api-key probe, oauth, login server
internal/core/                agent loop, sessions, cost tracking
internal/extproto/            extension wire-format types
internal/provider/            anthropic + openai-compatible streaming clients, model catalog
internal/skills/              skill discovery, frontmatter parser, skill tool
internal/tui/                 terminal raw-mode, input parser, editor, renderer, markdown, view
pkg/zotcore/                  public Go SDK for embedding zot in-process
pkg/zotext/                   public Go SDK for writing extensions
```

## License

MIT
