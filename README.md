# zot

yet another coding agent harness, lightweight and written (vibe-slopped) in go.

- one static binary.
- two providers atm (anthropic, openai/codex).
- four tools (read, write, edit, bash).
- three run modes (interactive tui, print, json).
- built-in telegram bot.
- no extensions atm.
- no community atm.

## install

> the repo and prebuilt artifacts are **private at the moment**. until they go public, every download command below needs a github personal access token (PAT) with `contents:read` scope on `patriceckhart/zot`. create one at <https://github.com/settings/tokens> and export it before running any installer:
>
> ```bash
> export GITHUB_TOKEN=ghp_xxx
> ```
>
> once the repo is public, the token becomes optional.

### one-liner (macos + linux)

```bash
curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" \
  https://raw.githubusercontent.com/patriceckhart/zot/main/install.sh \
  | GITHUB_TOKEN=$GITHUB_TOKEN bash
```

detects your os/arch, downloads the latest release from github, verifies the sha256 against the release's `checksums.txt`, extracts the binary, and drops it in `/usr/local/bin`, `~/.local/bin`, or `~/bin` — whichever is writable first. pass a version or prefix to pin:

```bash
curl -fsSL https://raw.githubusercontent.com/patriceckhart/zot/main/install.sh | bash -s -- v0.0.1 ~/bin
```

### one-liner (windows, powershell)

```powershell
$env:GITHUB_TOKEN = "ghp_xxx"
$h = @{ Authorization = "Bearer $env:GITHUB_TOKEN" }
(Invoke-WebRequest -UseBasicParsing -Headers $h `
  https://raw.githubusercontent.com/patriceckhart/zot/main/install.ps1).Content | iex
```

drops `zot.exe` into `$HOME\bin` and adds it to the user PATH if missing. open a fresh terminal afterwards.

### homebrew (macos + linux)

available once the repo is public. the tap lives at `patriceckhart/homebrew-tap`:

```bash
brew install patriceckhart/tap/zot
```

### go install

while the repo is private, `go install` needs to bypass the public go module proxy:

```bash
export GOPRIVATE=github.com/patriceckhart/*
export GIT_TERMINAL_PROMPT=0
go install github.com/patriceckhart/zot/cmd/zot@latest
```

your git must already be authenticated to github (ssh key or https PAT in `~/.netrc`). once the repo is public, `go install github.com/patriceckhart/zot/cmd/zot@latest` is enough.

### from source

```bash
git clone https://github.com/patriceckhart/zot
cd zot
make build        # produces ./bin/zot
make install      # into $GOPATH/bin
```

### prebuilt binaries

every release on the [releases page](https://github.com/patriceckhart/zot/releases) ships archives for linux, macos, and windows on amd64 + arm64 (except windows/arm64), plus a `checksums.txt` file. download, verify, `chmod +x`, and drop on your `$PATH`.

## authenticate

the easiest way is to just run `zot` and type `/login`. the tui opens even without credentials and walks you through a browser-based login flow.

### credential lookup order

1. `--api-key` flag
2. `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` env var
3. `$ZOT_HOME/auth.json` (api key or oauth token; mode 0600)

`$ZOT_HOME` defaults to:
- macOS: `~/Library/Application Support/zot`
- linux: `$XDG_STATE_HOME/zot` or `~/.local/state/zot`
- windows: `%LOCALAPPDATA%\zot`

### `/login` flow

run `zot` and type `/login`. pick one of two methods:

- **api key** — a small local web server starts on `127.0.0.1:<free-port>`, your browser opens a form, you paste your `sk-ant-...` or `sk-...` key. zot probes the provider once and saves it to `auth.json` if accepted.
- **subscription** — use your claude pro/max or chatgpt plus/pro subscription. the oauth flow pins the callback to a fixed port per provider (`localhost:53692` for anthropic, `localhost:1455` for openai) because those are the only ports their auth servers will redirect to.
  - anthropic uses the claude code oauth flow; messages go to `api.anthropic.com` with a bearer token and the claude-code identity headers.
  - openai uses the codex cli oauth flow; messages go to `chatgpt.com/backend-api/codex/responses` with the `chatgpt-account-id` extracted from the returned id_token.

> **note on subscription login**: the oauth client ids used are the ones published in anthropic's claude code cli and openai's codex cli. reusing them from a third-party tool is against their terms of service and may be revoked at any time. use it at your own risk; the api-key flow is the safe default.

### token refresh

oauth access tokens are short-lived (anthropic ~8h, openai ~30d). zot refreshes them automatically:

- at every credential lookup, zot checks the stored `expiry` and — if past it (with a 60s safety margin) — hits the provider's `oauth/token` endpoint with the stored `refresh_token`, persists the new `access_token` + `refresh_token` + `expiry` back to `auth.json`, and hands the fresh token to the client.
- the telegram bridge additionally refreshes once per turn so a bot that runs for days keeps working without manual intervention.
- if the refresh itself fails (the `refresh_token` was revoked, or the account was logged out everywhere), the error bubbles up to the caller: the tui shows it in the status line, the bot replies with it in your dm. run `/login` to get a fresh token pair.

all data lives under `$ZOT_HOME`:

```
$ZOT_HOME/
├── config.json         # last-used provider/model/theme, saved automatically
├── auth.json           # api keys and oauth tokens (mode 0600)
├── sessions/           # jsonl transcripts, one dir per cwd
├── models-cache.json   # live /v1/models discovery cache (6h ttl)
└── logs/               # app log files
```

## usage

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

## flags

| flag | description |
|---|---|
| `--provider anthropic\|openai` | pick the provider |
| `--model <id>` | pick the model (see `--list-models`) |
| `--api-key <key>` | override api key |
| `--base-url <url>` | override provider base url (tests / self-hosted) |
| `--system-prompt <text>` | replace the default system prompt |
| `--append-system-prompt <text>` | append text to the system prompt (repeatable) |
| `--reasoning low\|medium\|high` | enable reasoning on supported models |
| `-c`, `--continue` | resume the latest session for this cwd |
| `-r`, `--resume` | pick a session to resume |
| `--session <path>` | resume a specific session file |
| `--no-session` | don't read or write session files |
| `--cwd <path>` | use `<path>` as the working directory |
| `--no-tools` | disable all tools |
| `--tools <csv>` | only enable the listed tools |
| `--max-steps <n>` | cap agent loop iterations (default 50) |

## tools

- `read` — read text files (or inline images: png / jpg / gif / webp)
- `write` — create or overwrite files, making parent directories as needed
- `edit` — one or more exact-match replacements in an existing file
- `bash` — run a shell command in the session cwd, with merged stdout/stderr and a timeout

when the sandbox is on (see `/lock`), all four tools refuse paths outside the session cwd.

## modes

- **interactive** (default): chat tui with streaming output, spinner, cost meter, slash commands.
- **print**: `zot -p "prompt"` runs the agent to completion and writes only the final assistant text to stdout.
- **json**: `zot --json "prompt"` emits one json object per agent event to stdout, newline-delimited. the schema is documented in `instructions.md` §8.

## slash commands

type `/` in the tui to open the autocomplete popup. available commands:

| command | description |
|---|---|
| `/help` | show key bindings and commands |
| `/login` | log in via api key or subscription (opens a dialog) |
| `/logout [provider]` | clear credentials for `anthropic`, `openai`, or all when omitted |
| `/model` | pick a model from a list (or `/model <id>` to set directly) |
| `/sessions` | resume a previous session for this directory |
| `/compact` | summarize the transcript into one message to free up context |
| `/lock` | confine tools to the current directory |
| `/unlock` | allow tools to touch paths outside again |
| `/clear` | clear the chat transcript |
| `/exit` | exit zot |

### `/sessions`

shows previous sessions for the current working directory, newest first, with timestamp, model, message count, cost, and the first user prompt. pick one with `↑/↓`, `enter` to resume, `esc` to cancel. zot swaps the current session file for the selected one and replays the full transcript (including tool calls) into the agent. sessions remember the model they ended on, so resuming picks up on that exact model even if your global default changed.

### `/compact`

sends the current transcript through the model with a structured summarization prompt. the returned summary replaces the transcript as one synthetic user message, with the last few exchanges kept verbatim for continuity. status bar's `ctx N/M (P%)` meter resets. use it when the context meter creeps past ~80%.

zot also auto-compacts in the background: after any turn that leaves context usage ≥ **85%** of the model's window, the agent kicks off a condense pass on its own. you'll see `condensing history… (esc to cancel)` above the status bar and an `(auto)` tag next to the context percentage; esc aborts it without touching the transcript.

### `/lock`

enforces a sandbox rooted at the cwd shown in the status bar. `read` / `write` / `edit` resolve their target path (including through symlinks) and refuse anything outside the sandbox. `bash` refuses obvious escape patterns: `sudo`, `rm -rf /`, leading `cd /` / `cd ..` / `cd ~`, `chmod -R`, `dd of=/`, etc. status bar shows `· locked · ~/your/cwd` while active.

this is a guardrail against accidents, not a hard security boundary. if you need real isolation, run zot under docker or a proper sandbox.

## sessions

every interactive or print/json run (unless `--no-session`) writes a jsonl transcript under `$ZOT_HOME/sessions/<cwd-hash>/`. resume any of them with `--continue`, `--resume`, `--session <path>`, or interactively via `/sessions` inside the tui.

## models

`--list-models` or the `/model` picker shows the full catalog. three sources:

- **catalog** — models baked into zot, always available
- **live** — ids discovered from `GET /v1/models` using your stored api key (cached for 6h in `$ZOT_HOME/models-cache.json`, refreshed in the background on startup)
- **speculative** — ids that appear in the upstream generator but aren't live on the public api yet; they'll 404 today and start working the moment the provider ships them

the context meter in the status line (`ctx N/M (P%)`) uses the model's advertised context window to show how much of it your last turn consumed.

## inline images

when a tool returns an image (e.g. `read` on a png), zot renders it inline on terminals that support it: **iterm2**, **wezterm**, **kitty**, **ghostty**. on other terminals you see a text placeholder with mime type, pixel dimensions, and byte size. control with the `ZOT_INLINE_IMAGES` env var:

| value | effect |
|---|---|
| unset (default) | auto-detect based on `TERM_PROGRAM` |
| `iterm` / `iterm2` | force iterm2 osc 1337 protocol |
| `kitty` | force kitty graphics protocol |
| `off` / `none` | always use the text placeholder |

frames containing images are full-repainted (no differential diff) to prevent stale image pixels from lingering through scroll. that costs one terminal flash per image-containing frame; set `ZOT_INLINE_IMAGES=off` if that bothers you.

## queued messages

you can keep typing while the agent is working. pressing enter during a turn queues the message instead of interrupting: it shows up above the status bar as `▸ sliding in: <text>` and is delivered as the next user turn the moment the current one finishes. queue as many as you want; they run in order. esc / ctrl+c cancels the active turn and drops the queue so a runaway turn doesn't flood you with stale follow-ups.

slash commands still require an idle state — typing `/something` during a turn prints `cancel the current turn (esc) before running a slash command`.

## keys (interactive mode)

### input

| key | action |
|---|---|
| `enter` | submit (queued if the agent is busy) |
| `alt+enter` | newline |
| `tab` | complete the selected slash command |
| `esc` | cancel the current turn (while busy); clear input (while idle) |
| `ctrl+c` | exit when idle; cancel the current turn while busy |
| `ctrl+d` | exit on empty input |
| `ctrl+l` | redraw the screen |
| `ctrl+o` | expand / collapse long tool results |

### editor line navigation

| key | action |
|---|---|
| `ctrl+a` / `ctrl+e` | jump to start / end of line |
| `alt+←` / `alt+→` | jump one word back / forward |
| `ctrl+u` / `ctrl+k` | delete to start / end of line |
| `ctrl+w` · `alt+backspace` | delete the previous word |
| `up` / `down` (editor non-empty) | cycle through prompt history |

### chat scroll

| key | action |
|---|---|
| `pgup` / `pgdn` | scroll one page up / down |
| `up` / `down` (editor empty) | scroll three lines up / down — this is how the mouse wheel reaches the scroll logic on most terminals |

## telegram bot (bridge)

zot can run as a telegram bot so you can dm it from your phone. it's a built-in subcommand, not a plugin:

```bash
zot telegram-bot setup     # paste a BotFather token, verify, save
zot telegram-bot run       # foreground: long-poll in this terminal (ctrl+c to stop)
zot telegram-bot start     # background: detach and return immediately
zot telegram-bot stop      # sigterm the background bot (sigkill after 5s)
zot telegram-bot logs -f   # tail $ZOT_HOME/logs/bot.log (omit -f to just cat)
zot telegram-bot status    # config (token masked) + running/stopped
zot telegram-bot reset     # forget the token + paired user
# short alias: `zot tg ...` is accepted for every subcommand
```

the background flavor writes the child's pid to `$ZOT_HOME/bot.pid` and redirects stdout+stderr to `$ZOT_HOME/logs/bot.log`. `zot telegram-bot stop` reads that pid, sends sigterm, waits up to five seconds, then escalates to sigkill if the child is still alive. running two instances at once is refused at startup.

> **use the installed binary for `start`.** `go run ./cmd/zot telegram-bot start` won't work — `go run` builds a binary in a temp directory and deletes it when it exits, which kills the detached child. run `make install` (or `go build`) first and invoke the installed binary.

setup flow:

1. talk to [@BotFather](https://t.me/BotFather) on telegram, run `/newbot`, copy the token it gives you.
2. run `zot telegram-bot setup` and paste the token when prompted.
3. run `zot telegram-bot run` in the directory you want the agent to operate in.
4. open your bot on telegram, send `/start`. the first user to do this claims the bridge (stored as `allowed_user_id`); every other user is rejected.

from then on, any dm you send is forwarded to the agent as a user prompt. attached photos or image/* documents are downloaded and passed to vision-capable models. in-bot telegram commands: `/help`, `/status`, `/stop` (cancel the current turn). config lives in `$ZOT_HOME/bot.json` (mode 0600).

bot mode respects the usual zot flags — `--provider`, `--model`, `--cwd`, `--reasoning`, `--continue`, `--no-session`, `--no-tools`, etc. run `zot tg run -c --model claude-opus-4-1` to resume the latest session on opus, for example.

## development

```bash
make build     # build ./bin/zot
make test      # go test -race ./...
make lint      # go vet + gofmt check
make fmt       # gofmt -w .
make release   # cross-compile linux/darwin/windows × amd64/arm64
```

source layout:

```
cmd/zot/                      main()
internal/agent/               cli wiring, arg parsing, system prompt, config
internal/agent/modes/         interactive tui, print, json, dialogs
internal/agent/tools/         read, write, edit, bash, sandbox
internal/auth/                credential store, api-key probe, oauth, login server
internal/core/                agent loop, sessions, cost tracking
internal/provider/            anthropic + openai streaming clients, model catalog
internal/tui/                 terminal raw-mode, input parser, editor, renderer, markdown, view
```

## license

MIT
