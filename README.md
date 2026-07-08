<div align="center">
  <a href="https://www.zot.sh">
    <img src="packages/provider/auth/assets/zot-logo.png" alt="zot coding agent harness" width="130" height="130" />
  </a>
</div>
<p align="center">
  <a href="https://www.zot.sh">zot.sh</a>
</p>

## What is it?

Yet another coding agent harness, lightweight and written (vibe-slopped) in go.

- one static binary.
- built-in providers for Anthropic, OpenAI/Codex/Responses, Kimi, DeepSeek, Google Gemini/Vertex, GitHub Copilot, Bedrock, Azure OpenAI, OpenRouter, Groq, Cerebras, xAI, Together, Hugging Face, Mistral, Moonshot, Z.AI, Xiaomi, MiniMax, Fireworks, Vercel AI Gateway, OpenCode, Cloudflare AI, and Ollama/local models.
- four tools (read, write, edit, bash).
- three run modes (interactive tui, print, json).
- built-in telegram bot.
- extensions in any language via subprocess + json-rpc. None installed by default; opt in with `zot ext install` or `zot --ext`. See [docs/extensions.md](docs/extensions.md).
- user and extension themes via JSON; see [docs/themes.md](docs/themes.md).
- standing instructions via `AGENTS.md` files (global and per-project); see [Persistent instructions](#persistent-instructions-agentsmd).
- reusable instructions via `SKILL.md` files; see [docs/skills.md](docs/skills.md).

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
2. provider-specific env var (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `KIMI_API_KEY`, `MOONSHOT_API_KEY`, `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `GROQ_API_KEY`, `OPENROUTER_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `CEREBRAS_API_KEY`, `TOGETHER_API_KEY`, `HF_TOKEN`, `ZAI_API_KEY`, `XIAOMI_API_KEY`, `MINIMAX_API_KEY`, `FIREWORKS_API_KEY`, `AI_GATEWAY_API_KEY`, `COPILOT_GITHUB_TOKEN`, `GITHUB_COPILOT_TOKEN`, and others for provider-specific backends)
3. `$ZOT_HOME/auth.json` (API key or OAuth token; mode 0600)

`$ZOT_HOME` defaults to:
- macOS: `~/Library/Application Support/zot`
- Linux: `$XDG_STATE_HOME/zot` or `~/.local/state/zot`
- Windows: `%LOCALAPPDATA%\zot`

### `/login` flow

Run `zot` and type `/login`. Pick one of two methods:

- **API key**: a small local web server starts on `127.0.0.1:<free-port>`, your browser opens a form, you pick a provider from the full API-key provider list, paste the key, and zot saves it to `auth.json` if accepted. Providers with a lightweight model-list endpoint are probed before saving; provider backends that need extra project/account env vars are saved directly.
- **Subscription**: use your Claude Pro/Max, ChatGPT Plus/Pro, Kimi Code, or GitHub Copilot subscription. DeepSeek and Google Gemini do **not** have a subscription login path. For those, use the API-key flow.
  - Anthropic and OpenAI pin the browser callback to fixed provider-specific ports (`localhost:53692` for Anthropic, `localhost:1455` for OpenAI) because those are the only ports their auth servers will redirect to.
  - Anthropic uses the Claude Code OAuth flow. Messages go to `api.anthropic.com` with a bearer token and the Claude Code identity headers.
  - OpenAI uses the Codex CLI OAuth flow. Messages go to `chatgpt.com/backend-api/codex/responses` with the `chatgpt-account-id` extracted from the returned id_token.
  - Kimi uses the Kimi Code device-code OAuth flow. zot opens the verification URL, polls until you approve it in the browser, then sends messages to `api.kimi.com/coding/v1` with the Kimi Code identity headers.
  - GitHub Copilot uses GitHub's device-code login flow. zot stores the GitHub access token and exchanges it for short-lived Copilot inference tokens on demand.

> **Note on subscription login.** The OAuth client IDs used are the ones published in Anthropic's Claude Code CLI, OpenAI's Codex CLI, Kimi Code CLI, and GitHub Copilot's device-code flow. Reusing them from a third-party tool may be against their terms of service and may be revoked at any time. Use it at your own risk; the API-key flow is the safe default.

### Token refresh

OAuth access tokens are short-lived (Anthropic ~8h, OpenAI ~30d; Kimi and GitHub Copilot also use refresh/exchange flows). zot refreshes or exchanges them automatically:

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
├── AGENTS.md           # optional: global instructions appended to the prompt
├── SYSTEM.md           # optional: replaces the default system prompt
├── skills/             # optional: user SKILL.md files
├── themes/             # optional: user theme JSON files
├── extensions/         # installed extensions, one dir per extension
└── logs/               # app log files
```

Drop a `SYSTEM.md` in `$ZOT_HOME` to replace the built-in identity and guidelines for every run. `--system-prompt` still wins per-invocation. Delete the file to revert to the default.

## Persistent instructions (AGENTS.md)

Use `AGENTS.md` to give zot standing instructions that layer **on top of** the default system prompt, without replacing it. This is the friendliest way to shape behavior (for example, taming local models that jump straight to code edits) because it adds guidance rather than taking over the base identity the way `SYSTEM.md` does.

zot discovers `AGENTS.md` files automatically at startup and loads them in this order:

1. `$ZOT_HOME/AGENTS.md` (global, machine-wide instructions that apply to every project).
2. Every `AGENTS.md` from the top-most parent directory down to the current working directory. More specific (deeper) files may override earlier ones.

All discovered files are appended to the prompt in that order, so a global baseline can be refined per project.

For a general, non-project-specific instruction set, put your rules in `$ZOT_HOME/AGENTS.md`, for example:

```markdown
Treat questions and discussions as requests for explanation. Do not edit files or run tools unless explicitly asked to make a change. Ask before modifying code.
```

`AGENTS.md` vs the other mechanisms:

| Mechanism | Scope | Effect |
|---|---|---|
| `$ZOT_HOME/AGENTS.md` | global, every run | appended to the default prompt |
| `./AGENTS.md` (and parent dirs) | project | appended to the default prompt |
| `$ZOT_HOME/SYSTEM.md` | global, every run | replaces the default prompt entirely |
| `--append-system-prompt <text>` | single run | appends for one invocation (repeatable) |
| `--system-prompt <text>` | single run | replaces the prompt for one invocation |

> **Note:** zot does not read a `CLAUDE.md` instruction file. The only Claude-compatible thing it picks up is skills under `.claude/skills/`. If you are migrating from Claude Code, move that content into `AGENTS.md` (global or per-project) and zot will use it.

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
| `--provider <id>` | Pick the provider (for example `anthropic`, `openai`, `openai-codex`, `kimi`, `google`, `github-copilot`, `groq`, `openrouter`, `amazon-bedrock`, `ollama`; see Providers). |
| `--model <id>` | Pick the model (see `--list-models`). |
| `--api-key <key>` | Override the API key. |
| `--base-url <url>` | Override the provider base URL (tests, self-hosted). |
| `--insecure` | Skip TLS certificate verification for the explicit `--base-url` endpoint or a `baseUrl` defined for a user model in `models.json` (self-signed local/internal inference servers). Built-in providers, auth, and model discovery keep normal TLS verification. |
| `--system-prompt <text>` | Replace the default system prompt for this run (also overrides `$ZOT_HOME/SYSTEM.md`). |
| `--append-system-prompt <text>` | Append text to the system prompt (repeatable). |
| `--reasoning off\|minimum\|low\|medium\|high\|maximum` | Set thinking level on supported models (default: off). |
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
| `--no-skill` | Disable all skills, including built-ins. No `skill` tool is registered and the system prompt has no skill manifest. |
| `--no-yolo` | Confirm every tool call before it runs (interactive TUI only). A dialog shows the tool name and a one-line preview of its args with four choices: yes, yes-always-this-tool-this-session, yes-always-this-session, no. Ignored with a stderr warning in print / json / rpc modes, where tools still run freely so scripts and automation keep working. |

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

- **Go in-process**: import `github.com/patriceckhart/zot/packages/agent/sdk`. One `Runtime` per project; `Prompt(ctx, text, images)` returns a channel of `Event`. Small example in `examples/sdk/`.
- **Any language, out-of-process**: spawn `zot rpc` as a subprocess and exchange newline-delimited JSON over its stdin/stdout. Wire format and event schema in [docs/rpc.md](docs/rpc.md). Reference clients live under `examples/rpc/`.

Both interfaces share the same event schema, so transcripts captured by one can be replayed through the other.

## Slash commands

Type `/` in the TUI to open the autocomplete popup. Available commands:

| Command | Description |
|---|---|
| `/help` | Show key bindings and commands. |
| `/login` | Log in via API key or subscription (opens a dialog). |
| `/logout [provider]` | Clear credentials for any logged-in provider, or all when omitted. `/logout openai-codex` clears ChatGPT/Codex subscription auth while preserving a public OpenAI API key; `/logout kimi` also disables fallback to the official Kimi Code CLI token until you log in to Kimi through zot again. |
| `/model` | Pick a model from a list (or `/model <id>` to set directly). |
| `/sessions` | Resume a previous session for this directory. |
| `/session` | Four ops on the current session: `export` to a portable `.zotsession` file, `import` one back in, `fork` from a past user message into a new branch, `tree` to switch between branches. Opens a picker without an argument; direct forms: `/session export [path]`, `/session import <path>`, `/session fork`, `/session tree`. Default export destination is `~/Downloads`. |
| `/jump` | Scroll the chat to a previous turn (or `/jump <text>` to filter). |
| `/btw` | Side chat with full context that doesn't add to the main thread. |
| `/swarm` | Spawn, monitor, and chat with background subagents. Each runs in parallel with your main session and shares its working directory. |
| `/skills` | List discovered skills (SKILL.md files) and preview their bodies. |
| `/compact` | Summarize the transcript into one message to free up context. |
| `/study` | Run the canned prompt "Read and understand everything in the current directory." so the agent has full project context before you start asking targeted questions. Pass a path — typed, drag-dropped, or selected via `@` — to target a specific file or directory instead: `/study [dir:packages/]`, `/study cmd/zot/main.go`. |
| `/jail` | Confine tools to the current directory. |
| `/unjail` | Allow tools to touch paths outside again. |
| `/reload-ext` | Hot-reload all extensions (re-read manifests, respawn subprocesses, rebuild tool registry). |
| `/telegram` | Connect, disconnect, or show status of the Telegram bridge (takes `connect` / `disconnect` / `status` as an optional argument; opens a picker without one). When connected, DMs from the paired user become prompts in the running session and the assistant's replies are mirrored back to Telegram. Alias: `/tg`. |
| `/settings` | Toggle persistent settings (inline images, auto-swarm) with `enter`/`space`. Saved to `$ZOT_HOME/config.json`; takes effect immediately. |
| `/clear` | Clear the chat transcript. |
| `/exit` | Exit zot. |

Extension-registered commands appear under a divider at the bottom of the popup, sorted by name.

### Shell escape (`!command`)

Type `!` followed by a command to run it directly without going through the model. Everything after the `!` is passed to the same shell the `bash` tool uses (`/bin/sh -c` on Unix, `cmd /C` on Windows), runs in the session working directory, and honors the `/jail` sandbox. The output is appended below the transcript as a terminal-log block (command echo, output, exit code), styled by success or failure. It stays on screen until you send your next prompt (or run `/clear`), so it doesn't bleed into the model conversation. A running `!command` shares the busy state with the agent: `esc` cancels it, and you cannot start one while a turn (or another shell escape) is in flight.

### `/sessions`

Shows previous sessions for the current working directory, newest first, with timestamp, model, message count, cost, and the first user prompt. Pick one with `up`/`down`, `enter` to resume, `esc` to cancel. zot swaps the current session file for the selected one and replays the full transcript (including tool calls) into the agent. Sessions remember the model they ended on, so resuming picks up on that exact model even if your global default changed.

### `/session`

Four ops on the current session. `/session` alone opens a picker; each is also runnable directly.

- **`/session export [path]`**. Writes the running transcript to a portable `.zotsession` file. Default destination is `~/Downloads/<timestamp>-<session-id>-<prompt-slug>.zotsession`. Pass a path to override; a directory is fine (a dated name is built inside), a bare name gets `.zotsession` appended. The meta's cwd is stripped on the way out so the recipient doesn't see your filesystem layout.

  **What's included.** Only the main chat thread of the running session — messages, tool calls, tool results, compactions, and usage. **`/swarm` subagents are NOT included.** Their transcripts, unix-socket inboxes, and per-agent session files are all machine-local; a `.zotsession` is just a chat transcript and has no way to revive a unix socket on another box. If you want the conversation, copy it out of the dashboard manually.
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

### `/swarm`

Background subagents that run alongside your main session. Each one is a separate `zot` subprocess with its own model loop, its own persistent session file, and its own chat in the dashboard — but they all run in **the same working directory as the host**, so they see and edit the same files you do. Spawn one for a side task (“draft the migration”, “investigate this stack trace”, “write the test harness for module X”), keep going in the main thread, check in on it whenever you want.

> **Agents edit the same files you do.** They use the same `read` / `write` / `edit` / `bash` tools as the main agent against the host's working directory. There's no per-agent worktree or branch. If you need parallel edits on isolated checkouts, set that up yourself with `git worktree` outside zot.

```
/swarm                            # open the dashboard
/swarm new <task>                 # spawn an agent
/swarm new --model gpt-5 <task>    # pin the new agent to a specific model
/swarm logs <id>                  # jump straight into one agent's transcript
/swarm send <id> <text>           # send a follow-up without opening the dashboard
/swarm resume                     # pick a stopped agent to bring back
/swarm resume <id>                # bring a specific agent back
/swarm kill <id>                  # stop a running agent (its state stays)
/swarm remove <id>                # delete the agent's session and state
/swarm list                       # alias for opening the dashboard
```

**Dashboard (`/swarm` with no arg)** — a list of every agent for the current session, with status, age, and current activity. Keys:

| Key | Action |
|---|---|
| `↑` / `↓` | Move cursor between rows. |
| `enter` | Open the highlighted agent's transcript view. |
| `n` | Spawn a new agent (opens an inline task editor; inherits the host's current model). |
| `p` | One-off prompt editor for the selected row (without entering the transcript). |
| `R` | Resume a stopped agent in place. |
| `k` | Kill the selected running agent. Its session and state stay so you can resume it later. |
| `r` | Remove the selected agent entirely (session + meta gone). |
| `esc` | Close the dashboard. |

**Inside an agent's transcript** — a chat overlay with an always-on inline composer at the bottom. The conversation flows above it; type and `enter` to send a follow-up. The view auto-follows streaming output and shows an inline spinner with the agent's current activity (`thinking`, `tool: edit_file`, etc.) while it's busy. `esc` returns to the dashboard.

**Switching the spawn model from inside the editor** — while composing a task in the `n`-prompt, type `/model` on its own line and `enter`. The standard `/model` picker pops up; pick a model, the picker closes, and the editor reopens with your typed task intact and the new model pinned for the spawn.

**Session scoping** — each agent is stamped with the host session that spawned it and only shows up in that session's dashboard. Swap sessions with `/sessions` and the dashboard re-narrows accordingly. Agents from other sessions keep running in the background and reappear when you switch back.

**Persistence across zot restarts** — every spawn writes a `meta.json` next to its event log and session file under `$ZOT_HOME/swarm/agents/<id>/`. On the next `zot` launch they show up in the dashboard as **detached**; press `R` (or `/swarm resume <id>`) to bring one back. Resumed agents reattach to the same session and inbox socket, so the conversation continues from where it left off.

**Where state lives** — everything per-agent (session file, events log, inbox socket, meta) lives under `$ZOT_HOME/swarm/agents/<id>/`. The agent's actual code edits land directly in your repo; track them with normal `git status` / `git diff`.

**`/session export` does NOT bundle subagents.** A `.zotsession` is just the main chat transcript; per-agent state (session file, unix-socket inbox) is machine-local and doesn't round-trip through a JSONL file. To share what an agent said, copy it out of the transcript view manually.

**Auto-swarm.** With `/settings` -> auto-swarm on, the main agent gets a built-in `swarm_spawn` tool and a system-prompt nudge to use it. It can then fork sub-agents on its own when a request naturally splits into independent parallel work ("implement A and B", "investigate three files"). Each spawn returns the sub-agent id immediately and the main turn keeps going. When every sub-agent the agent spawned in that batch finishes its initial task, zot injects one `[auto-swarm update]` message back into the main chat recapping each agent's status, task, and transcript tail; the main agent then writes a short follow-up summary referencing the agents by id. Off by default; toggle from `/settings`.

### `/settings`

Opens a dialog with every persistent setting. `up`/`down` to navigate, `enter` or `space` to change the selected row, `esc` to close (rows that open a sub-view, like model shortcuts, use `esc` to go back one level first). Changes are written to `$ZOT_HOME/config.json` and take effect on the next turn (no restart needed). Current settings:

- **render images when supported** — draw screenshots / `read`-returned images inline using the terminal's image protocol, or fall back to a text placeholder. Auto-detected from `TERM_PROGRAM`; the toggle overrides the detection. The row is greyed out and forced off on terminals that don't speak any image protocol.
- **auto-swarm** — let the main agent spawn background sub-agents in parallel via a built-in `swarm_spawn` tool. Off by default. When on, the tool is registered with the running agent, the system prompt gains a short addendum telling the model to delegate independent sub-tasks proactively, and zot watches every sub-agent the main agent spawns. As soon as the last sub-agent in a batch finishes its initial task, an `[auto-swarm update]` message is injected back into the chat with each agent's status / task / transcript tail, so the main agent can summarise the collective outcome. Flipping off mid-session removes the tool from the live agent and strips the addendum on the next turn — the model stops trying to delegate. See `/swarm` for the dashboard that lets you monitor, message, kill, or remove the spawned agents.
- **compact transcript rendering**: reduce visual chrome in the chat transcript. Tool calls render as a quiet header plus indented output instead of a bordered box, and sent messages render without padded background bubbles. Off by default. Changes apply immediately and persist to `config.json` as `compact_mode`.
- **TUI settings**: opens a sub-view for input layout and status placement. **Input style** can be `plain` (default prompt line), `lines` (separator lines above and below the input), or `block` (a user-bubble-style input block). **Status position** places model, usage, and working-directory information above or below the input. **Working spinner position** places the busy spinner above or below the input. Changes apply immediately and persist to `config.json` as `tui_input_style`, `tui_status_position`, and `tui_working_position` (`above_input` or `below_input` for the position fields).
- **thinking level** — choose reasoning for supported models: off (default; no reasoning), minimum (~1k tokens), low (~2k), medium (~8k), high (~16k), maximum (~32k). The change is persisted to `config.json` and applied to the running agent's next model call.
- **color theme** — choose the built-in auto/dark/light theme or any JSON theme discovered under `$ZOT_HOME/themes` or a loaded extension. Theme files can override any subset of UI colors, syntax colors, and spinner frames/messages. Changes apply immediately; if a selected theme file is deleted, zot resets to auto. See [docs/themes.md](docs/themes.md).
- **model shortcuts** — opens a sub-view with nine slots (`model 1` ... `model 9`). `enter` on a slot opens the same `/model` selector and binds the chosen provider/model to that slot; `backspace` clears a slot. Once assigned, press `Ctrl+1` ... `Ctrl+9` from the editor to switch the active model instantly (the same cross-provider swap `/model` performs, transcript and cost carried over). Assigning a shortcut does not change the current model. Shortcuts are skipped while a turn is running.

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

## Providers

zot's built-in provider catalog includes:

- **Subscription-capable**: Anthropic Claude Pro/Max (`anthropic`), OpenAI Codex / ChatGPT Plus/Pro (`openai-codex`), Kimi Code (`kimi`), GitHub Copilot (`github-copilot`).
- **Direct API providers**: Anthropic, OpenAI Chat Completions, OpenAI Responses, DeepSeek, Google Gemini, Kimi/Moonshot, Moonshot CN, Groq, Cerebras, xAI, Together AI, Hugging Face Router, OpenRouter, Mistral, Z.AI, Xiaomi/MiMo token-plan regions, MiniMax global/CN, Fireworks, Vercel AI Gateway, OpenCode/OpenCode Go.
- **Cloud/platform providers**: Amazon Bedrock, Google Vertex AI, Azure OpenAI, Cloudflare Workers AI, Cloudflare AI Gateway.
- **Local/compatible**: Ollama and OpenAI-compatible local endpoints via `--base-url`.

Use `/login` to store API keys or subscription credentials. `/model` only shows models from providers that are currently available from env vars, `auth.json`, Kimi CLI fallback, or local Ollama.

## Models

`--list-models` or the `/model` picker shows the full catalog across all built-in providers. Three sources:

- **Catalog**: models baked into zot, covering Claude, GPT/Codex, Gemini/Gemma, Kimi/Moonshot, DeepSeek, Groq-hosted Llama/Gemma/Compound, OpenRouter-routed models, Bedrock model ids, Vertex model ids, Azure OpenAI deployments, Copilot models, and other provider-specific catalog entries.
- **Live**: IDs discovered from `GET /v1/models` using your stored API key (cached for 6h in `$ZOT_HOME/models-cache.json`, refreshed in the background on startup).
- **Speculative**: IDs that appear in the upstream generator but aren't live on the public API yet. They'll 404 today and start working the moment the provider ships them.

The context meter in the status line uses the model's advertised context window to show how much of it your last turn consumed.

### Model fallback (rescue)

When a turn fails because of a recoverable provider error — expired token (`401`), permission denied (`403`), rate limit (`429`), provider outage (`502`/`503`/`504`), or a transient network failure — zot opens a **rescue** picker over the chat instead of just painting a red banner.

The picker is the same vertical list / fuzzy filter UI as `/model`, but it only shows models from providers you're currently logged in to (env vars, `auth.json`, Kimi CLI fallback, ollama). The failed model is excluded. Press `↑`/`↓` to choose, `enter` to retry the **same prompt** on the new model, `esc` to dismiss.

Before the actual provider request fires, the OpenAI / Anthropic / Kimi / DeepSeek / Google / OpenAI-Codex clients also do up to two silent retries with short backoff (250ms, 750ms) on `502`/`503`/`504` and connection-reset / EOF-before-headers errors. Most edge-proxy blips disappear without you ever seeing the rescue picker.

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

Provider keys are normalized: `openai-codex` and `openai-responses` map to `openai`, `anthropic-messages` maps to `anthropic`, `moonshot`, `moonshot-ai`, and `kimi-code` map to `kimi`, and `deepseek-chat` and `deepseek-ai` map to `deepseek`. Built-in provider ids such as `groq`, `openrouter`, `github-copilot`, `amazon-bedrock`, `google-vertex`, `azure-openai-responses`, `fireworks`, `vercel-ai-gateway`, `mistral`, and `xai` can also be used directly.

User-defined models show `source: user` in `--list-models` and take precedence over both the baked-in catalog and live-discovered models. Adding a `models.json` does not hide the built-in catalog; entries are merged on top of it. Missing or invalid files are silently ignored.

#### Custom providers

A top-level provider key that is not a built-in id defines a custom provider. Give it a provider-level `baseUrl` and an `api` wire format (`openai` for OpenAI-compatible chat completions, the default, or `anthropic` for the Anthropic messages API). A model-level `baseUrl` overrides the provider-level one for that model; an unknown `api` value falls back to `openai` with a warning.

```json
{
  "providers": {
    "my-company": {
      "baseUrl": "https://llm.mycompany.com/v1",
      "api": "openai",
      "models": [
        { "id": "company-llm-v2", "name": "Company LLM v2" }
      ]
    }
  }
}
```

Custom providers are first-class: they appear in `--list-models`, `/model`, and `/login`. `models.json` never stores secrets. Supply the key through `/login`, `--api-key`, or a derived environment variable in upper snake case (so `my-company` reads `MY_COMPANY_API_KEY`). Because many self-hosted gateways do not expose a model-list endpoint, custom provider keys are accepted and stored without a verification probe; an invalid key surfaces on the first model call.

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

### DeepSeek

zot has built-in DeepSeek support through DeepSeek's OpenAI-compatible chat API.

```bash
zot --provider deepseek
```

By default this uses:

- model: `deepseek-v4-pro`
- base URL: `https://api.deepseek.com/v1`

Catalog ships with `deepseek-v4-pro` (reasoning) and `deepseek-v4-flash`. These are exactly the IDs returned by `GET https://api.deepseek.com/models` today. You can add additional model IDs to `models.json` under the `deepseek` provider.

Credential lookup order for DeepSeek:

1. `--api-key`
2. `DEEPSEEK_API_KEY`
3. `$ZOT_HOME/auth.json`

Use `/login` and pick **api key** to paste a DeepSeek key. zot probes `/v1/models` once and stores the key under `deepseek` in `auth.json`.

> **Auth model: API key only.** DeepSeek does not offer a subscription OAuth flow. The `/login subscription` step lists only Anthropic, OpenAI, and Kimi; DeepSeek shows up only under `/login → api key`.

> **Text only at the wire level.** DeepSeek's chat-completions endpoint currently rejects the multimodal content schema (`unknown variant image_url, expected text`). When the active provider is `deepseek`, zot silently drops `ImageBlock` parts from outgoing user/tool messages and keeps only the text. Switching back to a vision-capable model (Claude, GPT-4o/5, Gemini) re-sends the image normally because the session file still stores it.

For a custom-compatible endpoint (mirror, gateway, self-host):

```bash
zot --provider deepseek --base-url https://my-deepseek-mirror.example.com/v1 --api-key "$DEEPSEEK_API_KEY"
```

### Google Gemini

zot has built-in Google Gemini support through the [AI Studio Generative Language API](https://aistudio.google.com/).

```bash
zot --provider google
```

By default this uses:

- model: `gemini-2.5-pro`
- base URL: `https://generativelanguage.googleapis.com`

Catalog ships with `gemini-2.5-pro`, `gemini-2.5-flash`, `gemini-2.5-flash-lite`, `gemini-2.0-flash`, and `gemini-2.0-flash-lite`. Live discovery against `/v1beta/models` adds anything else your key can see.

Credential lookup order for Google:

1. `--api-key`
2. `GEMINI_API_KEY`
3. `GOOGLE_API_KEY`
4. `$ZOT_HOME/auth.json`

Use `/login` and pick **api key** to paste an AI Studio key. zot probes `/v1beta/models` once and stores the key under `google` in `auth.json`.

> **Auth model: API key only.** Google does not issue OAuth tokens for consumer Gemini Advanced / Google One AI Premium subscriptions, so there is no "log in with your Google subscription" flow. Programmatic access requires either an AI Studio API key (this provider) or a Vertex AI / GCP service-account credential (not yet wired up in zot). The `/login subscription` step quietly downgrades to the api-key form when you pick Google so you don't end up in a dead end.

> **Free-tier rate limits.** AI Studio's free tier has tight per-minute and per-day caps that vary by model: `gemini-2.5-pro` is the strictest (a few requests per minute, ~50 per day), Flash and Flash-Lite are far more generous. If a Pro turn 429s with `"You exceeded your current quota"` while Flash on the same key still works, you've hit the Pro free-tier RPD. Either switch to Flash for agent loops, or [enable billing](https://aistudio.google.com/app/apikey) on your AI Studio project to flip the same key from free to pay-as-you-go pricing (`$1.25/M` input, `$10/M` output for Pro).

Reasoning levels (`--reasoning off|minimum|low|medium|high|maximum`, also configurable in `/settings` as **thinking level**) map differently per generation. Budget-based providers use roughly 1k/2k/8k/16k/32k thinking tokens for minimum/low/medium/high/maximum, with provider/model caps applied (Gemini 2.5 Pro caps at 32k; Flash at 24k). Gemini 3.x uses the `thinkingLevel` enum (`MINIMAL`/`LOW`/`MEDIUM`/`HIGH`), with Gemini-3-Pro pinned to `LOW` minimum and `HIGH` for any "medium" or higher request. Effort-based OpenAI-compatible chat providers map minimum to `low`, low/medium directly, and high/maximum to `high`; the Codex/Responses backend maps maximum to `xhigh` where supported. `off` sends no reasoning config. 2.0-family Gemini models have no thinking config at all.

You can add additional Gemini model IDs to `models.json` under the `google` provider.

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

## Tool rendering

By default each tool call (bash, read, write, edit) renders inside a bordered panel — a `┌─ header ─┐`, `│`-prefixed body rows, and a `└─┘` footer. On a screen with many calls the borders can read as busy, so zot also offers a **flat** mode: a single quiet header line per call (`▌ bash …`) with indented, border-free output. Same information — tool name, arg summary, streamed output, the `... (N more lines, ctrl+o to expand)` truncation — just no frame.

Set the `tool_render` key in `$ZOT_HOME/config.json`:

```json
{
  "tool_render": "flat"
}
```

| Value | Effect |
|---|---|
| unset / `"box"` (default) | Each tool call is wrapped in a bordered panel. |
| `"flat"` | Boxless: a quiet header line plus indented output. |

The `ZOT_FLAT_TOOLS` env var overrides the config for a single run, which is handy for trying it without editing the file:

| Value | Effect |
|---|---|
| `1`, `true`, `yes`, `on`, `flat` | Force flat rendering. |
| `0`, `false`, `no`, `off`, `box` | Force the bordered panel. |
| unset | Fall back to the `tool_render` config key. |

```sh
ZOT_FLAT_TOOLS=1 zot   # flat, just this run
ZOT_FLAT_TOOLS=0 zot   # boxes, even if config.json says "flat"
```

Either way, theme colors still drive the rendering (the header uses your accent/foreground, output uses the tool-output color) and `ctrl+o` still expands a truncated result.

### Tool arg width

The header line for a tool call shows the tool name plus a one-line summary of its primary argument — a `path`, a `command`, or a query. That summary is truncated to 60 cells by default (`web_answer What is the best architecture to implement resilience wit...`). On a wide terminal that can clip long queries more than you'd like, so set the `ZOT_TOOL_ARG_WIDTH` env var to raise (or lower) the limit:

```sh
ZOT_TOOL_ARG_WIDTH=120 zot   # allow up to 120 cells before truncating
```

| Value | Effect |
|---|---|
| unset (default) | Truncate the arg summary at 60 cells. |
| integer in `[20, 500]` | Truncate at that many cells instead. |
| anything else | Ignored; falls back to the 60-cell default. |

## Compact input

By default a message you send renders as a padded, background-tinted bubble: a blank tinted row above and below the text, with a `▌` accent bar down the left. So even a one-line prompt occupies three rows. Set `compact_input` to collapse it to a single quiet `▌ your text` gutter line per wrapped row — no padding rows, no background tint.

```json
{
  "compact_input": true
}
```

| Value | Effect |
|---|---|
| unset / `false` (default) | Padded, background-tinted user bubble. |
| `true` | One quiet gutter line per wrapped row. |

The `ZOT_COMPACT_INPUT` env var overrides the config for a single run (`1`/`true`/`on`/`compact` force compact; `0`/`false`/`off`/`bubble` force the bubble):

```sh
ZOT_COMPACT_INPUT=1 zot   # compact, just this run
```

## Queued messages

You can keep typing while the agent is working. Pressing `enter` during a turn queues the message instead of interrupting: it shows up above the status bar as `sliding in: <text>` and is delivered as the next user turn the moment the current one finishes. Queue as many as you want; they run in order. `esc` cancels the active turn and drops the queue so a runaway turn doesn't flood you with stale follow-ups; `ctrl+c` while busy arms the exit hint instead of interrupting, a second `ctrl+c` within two seconds exits zot.

To recover the most recently queued message back into the editor (to tweak it before it runs), press `Option+↑`. In VS Code's integrated terminal that chord doesn't survive xterm.js's macOS key handling — use `Option+Shift+↑` there. zot's hint line under the sliding-in queue adapts automatically based on `$TERM_PROGRAM`.

Slash commands also work while the agent is busy. Read-only ones (`/help`, `/jump`, `/btw`, `/sessions`, `/skills`, `/settings`, `/jail`, `/unjail`, `/exit`) take effect immediately. Destructive ones (`/clear`, `/compact`, `/login`, `/logout`, `/model`, `/reload-ext`) cancel the active turn first and then run.


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
| `ctrl+v` | Paste an image from the system clipboard through zot on macOS. Images are saved as temporary PNGs and attached to the next prompt. Other platforms currently use a no-op stub. Use your terminal/OS paste shortcut for text. |
| `ctrl+o` | Expand or collapse long tool results (read, write, edit, bash outputs over ~12 lines). |
| `ctrl+1` ... `ctrl+9` | Switch to the model bound to that quick-model slot (configured in `/settings` -> model shortcuts). No-op while a turn is running. |
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
| `up`, `down` | Move within multi-line input. At the top edge, `up` recalls previous prompts and `down` moves forward through prompt history. |

### Chat scroll

| Key | Action |
|---|---|
| `pgup`, `pgdn` | Scroll one page up or down. |
| `up`, `down` (editor empty, not browsing prompt history) | Scroll three lines up or down. This is how the mouse wheel reaches the scroll logic on most terminals. |

## Extensions

zot can be extended in any language via a subprocess + JSON-RPC protocol. Extensions can register slash commands, expose tools to the model, intercept tool calls (block or rewrite args), gate whole turns before the model is called, and rewrite the assistant's visible text before it reaches the user. None are installed by default; opt in explicitly. Hot-reload any time with `/reload-ext`.

### Install and manage

```bash
zot ext install <path|git-url>   # copy / clone into $ZOT_HOME/extensions/
zot ext list                      # show installed extensions
zot ext doctor                    # diagnose load, registration, and conflict issues
zot ext logs <name> [-f]          # cat or tail the extension's stderr log
zot ext enable <name>             # re-enable a disabled extension
zot ext disable <name>            # disable without removing
zot ext remove <name>             # delete an extension directory
```

`zot ext doctor` keeps normal extension startup fail-soft, but gives
you an explicit troubleshooting view: manifest errors, disabled or
shadowed extensions, subprocess load errors, ready/auto-ready status,
registered commands/tools, registration conflicts, warnings, and the
stderr log path.

For development, point `zot --ext <path>` at a working directory and skip the install step entirely. Repeatable; takes precedence over installed extensions of the same name.

### Updating extensions

`zot update` refreshes the zot binary **and** every installed extension that lives in a git checkout. Per-extension behaviour:

- Disabled extensions are skipped.
- Extensions without a `.git/` directory (installed by `zot ext install ./local-path`) are skipped — there is no remote to pull from.
- For the rest, zot stashes any dirty worktree state (including untracked runtime files like `todos.json` or `config.json`), runs `git pull --ff-only`, and pops the stash. If the pop produces conflicts, the conflict markers are left in place and you'll see a warning.
- Diverged branches, offline pulls, or any other git failure are reported as `failed` and the next extension is processed. `zot update` itself never aborts because of an extension.
- zot does **not** run any build step (`go build`, `npm install`, `make`) after the pull. Extension authors are expected to commit the runnable artifact (binary, transpiled JS, etc.). If you need a build, rebuild manually and use `/reload-ext`.

### Theme-only extensions

An extension may ship only a theme: `extension.json` plus `theme.json` (or `themes/theme.json`) and no executable. zot loads it without spawning a subprocess and shows it in `/settings` with source information. See [docs/themes.md](docs/themes.md).

### Reference

`examples/extensions/` ships reference implementations in Go, TypeScript, Node, and shell. See [docs/extensions.md](docs/extensions.md) for the full protocol, the SDK API (`packages/agent/ext`), and the phase roadmap.

## Skills

A skill is a per-folder `SKILL.md` file with a YAML frontmatter header. zot discovers skills at startup, surfaces their names in the system prompt, and exposes a built-in `skill` tool the model uses to load the body on demand.

By default zot loads built-in skills plus user-installed skills from:

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
- Messages you type in the TUI are mirrored into the Telegram thread prefixed `you: ...` and the assistant's replies come back prefixed `zot: ...`, so the Telegram chat stays a complete record of both sides of the conversation.
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

### Architecture: protocol-agnostic bot core

The messenger functionality is split in two layers. A generic, protocol-agnostic core lives in `packages/agent/modes/bot`: it owns the turn queue, agent prompting, built-in command dispatch (`/start`, `/help`, `/status`, `/stop`), status formatting, and per-turn credential refresh. Concrete transports implement the small `BotAdapter` interface (inbound polling, sending replies, a typing indicator, and optional protocol-specific status text); the Telegram support in `packages/agent/modes/telegram` is one such adapter.

This means additional messaging backends (Discord, Slack, Signal, and similar) can be added by implementing `BotAdapter` in a new package and wiring up a subcommand. No changes to the runner, agent, or core are required. Channel IDs are opaque strings owned by the adapter, so the shared runner stays free of protocol-specific types.

## Development

```bash
make build     # build ./bin/zot
make test      # go test -race ./...
make lint      # go vet + gofmt check
make fmt       # gofmt -w .
make release   # cross-compile linux/darwin/windows on amd64 and arm64
```

Source layout (single Go module, four packages under `packages/`):

```
cmd/zot/                              main()
packages/provider/                    LLM client surface, model catalog, streaming clients
packages/provider/auth/               credential store, api-key probe, oauth, login server
packages/core/                        agent loop, sessions, cost tracking, compaction
packages/tui/                         terminal raw-mode, input parser, editor, renderer, markdown, view
packages/agent/                       cli wiring, arg parsing, system prompt, config
packages/agent/extensions/            extension subprocess manager
packages/agent/extproto/              extension wire-format types
packages/agent/modes/                 interactive tui, print, json, dialogs
packages/agent/modes/bot/             protocol-agnostic bot runner (BotAdapter interface)
packages/agent/modes/telegram/        telegram adapter, api client, daemon
packages/agent/tools/                 read, write, edit, bash, sandbox
packages/agent/skills/                skill discovery, frontmatter parser, skill tool
packages/agent/swarm/                 background subagent runtime
packages/agent/sdk/                   public Go SDK for embedding zot in-process (package sdk)
packages/agent/ext/                   public Go SDK for writing extensions (package ext)
```

Downstream consumers can depend on individual packages:
`go get github.com/patriceckhart/zot/packages/core` pulls only `core` and its transitive deps (today: `provider`), no agent or TUI code.

## License

MIT
