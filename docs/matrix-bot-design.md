# Matrix bot bridge — design

Status: **implemented** (see README "Matrix bot"); §8 streaming edits and §7 generic bot.Mirror remain open follow-ups. This document grounds the
Matrix bridge in the protocol-agnostic bot core that the Telegram
refactor (commit `741c643`) extracted, and shows exactly where Matrix
diverges from Telegram so a concrete adapter can be written with
confidence.

---

## 1. What the Telegram refactor left us

The messenger functionality is split in two layers (documented in
`README.md` → “Architecture: protocol-agnostic bot core”):

```
packages/agent/modes/bot/         protocol-agnostic core
  adapter.go    BotAdapter interface + InboundMessage / Command / SendOptions
  runner.go     Runner: turn queue, agent prompting, command dispatch,
                per-turn credential refresh, /status formatting glue
  commands.go   IsStopCommand (plain "stop" detection)
  status.go     FormatStatus / StatusSnapshot (shared /status renderer)

packages/agent/modes/telegram/    one concrete BotAdapter
  adapter.go    Adapter — implements BotAdapter (long-poll, pairing,
                image download, chunking, typing indicator, status text)
  api.go        hand-rolled minimal Telegram Bot API HTTP client
  config.go     bot.json state (token, bot id, allowed user id, last update id)
  daemon.go     PID file + process-alive / graceful-stop helpers
  bridge.go     in-TUI mirror Bridge + Host interface (the OTHER flavor)
  io.go fs.go process_*.go commands.go status.go   small helpers/shims
```

Two **flavours** of running the bot coexist, and Matrix needs both:

| flavour | entry point | engine | agent | where it lives |
|---|---|---|---|---|
| **standalone daemon** | `zot telegram-bot run` / `start` | `bot.Runner` + `telegram.Adapter` (BotAdapter) | owns its own `*core.Agent` + session | `botcmd.go` |
| **in-TUI mirror** | `/telegram connect` | `telegram.Bridge` + `telegram.Host` | reuses the running TUI’s agent | `interactive.go` |

The two cannot run at once against the same account — they’d race on
the long-poll / sync cursor. `telegramConnect` already enforces this
via `telegram.IsRunning` (PID check) before starting the in-TUI bridge.

### The `BotAdapter` contract (unchanged for Matrix)

```go
type InboundMessage struct {
    ChannelID string          // opaque; adapter owns encoding
    MessageID string          // optional reply anchor
    Text      string
    Images    []provider.ImageBlock
}

type Command int  // CmdStart | CmdHelp | CmdStatus | CmdStop

type SendOptions struct{ ReplyToMessageID string }

type BotAdapter interface {
    Run(ctx context.Context,
        handler func(InboundMessage),
        commandHandler func(Command, InboundMessage),
    ) error
    Send(ctx context.Context, channelID, text string, opts SendOptions) error
    IndicateWorking(ctx context.Context, channelID string) (stop func())
    StatusText() string
}
```

Channel IDs are opaque strings owned by the adapter, so the runner
stays free of protocol types. Telegram encodes `chatID` as
`fmt.Sprintf("%d", chatID)`. Matrix encodes `roomID` as
`roomID.String()` (`!opaque:server`). **The interface needs no change
for Matrix.** Every Matrix-specific concern (E2EE, markdown rendering,
sync cursor, invites) lives inside the adapter.

### The in-TUI `Host` contract (also already protocol-agnostic)

```go
// in telegram/bridge.go today, but contains zero Telegram types:
type Host interface {
    SubmitOrQueue(prompt string, images []provider.ImageBlock)
    CancelTurn()
    Status() string
    Notify(level, message string)
}
```

`telegramHost` in `interactive.go` adapts `*Interactive` to it.

---

## 2. Matrix library: `maunium.net/go/mautrix` (mautrix-go)

**Choice: `maunium.net/go/mautrix`** (a.k.a. mautrix-go). It is the
single dominant Go Matrix library — it powers every mautrix bridge
(whatsapp, telegram, signal, facebook, discord), gomuks, and others.
Confirmed SOTA patterns from real production code (`mautrix/whatsapp`,
`gomuks/gomuks`, `chenhg5/cc-connect`, `sipeed/picoclaw`):

```go
import (
    "maunium.net/go/mautrix"
    "maunium.net/go/mautrix/crypto/cryptohelper"
    "maunium.net/go/mautrix/event"
    "maunium.net/go/mautrix/format"
    "maunium.net/go/mautrix/id"
    _ "maunium.net/go/mautrix/crypto/goolm"  // pure-Go olm — NO CGO
)
```

Why it fits the `BotAdapter` shape:

- `mautrix.NewClient(homeserver, userID, accessToken)` → one struct,
  like `telegram.NewClient(token)`.
- `client.SyncWithContext(ctx)` **blocks until ctx cancels** — this is
  the direct analogue of Telegram’s `GetUpdates` long-poll loop, and it
  fits `BotAdapter.Run`’s “blocks until ctx done” contract exactly.
- `syncer := client.Syncer.(*mautrix.DefaultSyncer)` then
  `syncer.OnEventType(event.EventMessage, handler)` and
  `syncer.OnEventType(event.StateMember, …)` for invites — replaces
  Telegram’s per-update switch in `handleUpdate`.
- `client.SendText(ctx, roomID, text)` /
  `client.SendMessageEvent(ctx, roomID, event.EventMessage, content)`
  replaces `Client.SendMessage`.
- `format.RenderMarkdown(text, true, false)` → `MessageEventContent`
  with an HTML `FormattedBody`, so the agent’s markdown renders nicely
  in Element/etc. (Telegram has no such concept; this is a free win.)
- `client.UserTyping(ctx, roomID, true, 30*time.Second)` refreshed on
  a ticker, stopped with `false, 0` — direct analogue of
  `SendChatAction("typing")` every 4s.
- `client.UploadMedia(ctx, mautrix.ReqUploadMedia{…})` → `ContentURI`;
  `client.Download(ctx, parsed)` → media bytes — replace Telegram’s
  `getFile` + `DownloadFile`.

### E2EE — the one place Matrix is materially harder

Telegram Bot API **cannot** do E2EE; bots live in unencrypted chats.
Matrix **DMs are encrypted by default** in most clients, so a personal
zot Matrix bot that lives in DMs almost certainly needs E2EE to receive
anything at all.

mautrix-go ships a `cryptohelper.CryptoHelper` that:

1. owns an olm/megolm key store in sqlite
   (`$ZOT_HOME/matrix-crypto/store.db`),
2. auto-decrypts inbound `m.room.encrypted` events and **re-dispatches
   them as `event.EventMessage`** — so our `OnEventType(EventMessage,
   …)` handler sees plaintext regardless,
3. auto-encrypts outbound events when `client.Crypto` is set and the
   room is encrypted.

The pure-Go olm backend (`crypto/goolm`, blank-imported) means **no
CGO, no libolm** — it builds with plain `go build`, matching zot’s
current CGO-free build. This is the deciding factor over any other
Matrix library.

E2EE should be **opt-in via config** (`crypto_passphrase`):
unencrypted bot rooms work without it; encrypted DMs require it. The
adapter degrades gracefully: if no passphrase is configured, the
crypto helper is skipped and the bot only works in unencrypted rooms
(a clear warning is logged + shown in `/status`).

---

## 3. Mapping `BotAdapter` → mautrix-go

| `BotAdapter` method | Telegram today | Matrix implementation |
|---|---|---|
| `Run(ctx, handler, cmdHandler)` | `GetUpdates` long-poll loop, `handleUpdate` parses commands + downloads images, calls `handler`/`commandHandler` | `Whoami` → (optional `CryptoHelper.Init`) → `syncer.OnEventType(EventMessage, …)` + `OnEventType(StateMember, …)` for auto-join → `client.SyncWithContext(ctx)`. Command parsing + image download happen in the `EventMessage` handler, then call `handler`/`commandHandler`. |
| `Send(ctx, channelID, text, opts)` | `SendMessage`, chunk to 4000 runes | `format.RenderMarkdown(text, true, false)`, set `RelatesTo.SetReplyTo(opts.ReplyToMessageID)`, chunk to ~4000 runes (Matrix soft limit is large; chunk for readability + edit-safety), `SendMessageEvent(EventMessage, …)`. Encryption is transparent via `client.Crypto`. |
| `IndicateWorking(ctx, channelID) (stop)` | `SendChatAction("typing")` every 4s until cancelled | goroutine: `UserTyping(ctx, roomID, true, 30s)` on a 25s ticker; `stop` cancels the goroutine and sends `UserTyping(…, false, 0)`. |
| `StatusText()` | `"@botusername"` | `"@mxid"` + display name (e.g. `@zot:matrix.org (zot)`), plus an `(e2ee)`/`(no e2ee)` tag so the user sees encryption state at a glance. |

`ChannelID` ↔ `id.RoomID` round-trips via `id.RoomID(channelID)` /
`roomID.String()`. `MessageID` ↔ `id.EventID` the same way. Identical
opacity to Telegram’s `int64` chat id.

### Pairing & access control (mirrors Telegram)

Telegram pairs the **first `/start` sender** and stores
`allowed_user_id`. Matrix has no bot-platform `/start`, so the model is
slightly different:

- **Pairing trigger:** the first user who DMs the bot **and** whose
  message is accepted (see below) claims it — store their
  `@user:server` as `allowed_user_id`. Until then, every DM gets a
  “not paired yet” reply.
- **Invite handling:** `OnEventType(event.StateMember, …)` — if the
  membership is `invite` and the state key is the bot’s own MXID,
  `client.JoinRoomByID(ctx, roomID)`. Auto-join is on by default
  (configurable) so the user just invites the bot from their client.
- **DM vs room:** only accept prompts from **1:1 DMs** (room with
  exactly 2 members, one of them the bot) by default. In group rooms
  the bot stays quiet unless directly mentioned (`@bot:server …`),
  mirroring `groupReplyAll=false` in `cc-connect`. This matches
  Telegram’s `msg.Chat.Type == "private"` gate.
- **Allowed user:** after pairing, `evt.Sender != allowed_user_id` →
  reject with “paired with a different user”, exactly like Telegram.
- **Self/echo skip:** `evt.Sender == selfUserID` → ignore (otherwise
  the bot replies to its own mirrored messages).
- **Dedup:** `evt.ID` is unique per event; a tiny LRU/set guards against
  sync replay sending the same event twice (Matrix sync can redeliver
  around disconnects). Telegram relies on `last_update_id` offset; the
  Matrix analogue is the `since_token` cursor, but a dedup set is still
  cheap insurance.
- **Old-message skip:** ignore events older than e.g. 60s at startup so
  catching up on backlog doesn’t fire stale prompts (`cc-connect` does
  this with `core.IsOldMessage`).

### Command parsing

Matrix has no built-in command routing. Like Telegram’s `handleUpdate`
text switch, the `EventMessage` handler inspects `content.Body`:

- `/start`, `/help` → `commandHandler(CmdStart|CmdHelp, …)`
- `/status` → `commandHandler(CmdStatus, …)`
- `/stop` and plain `stop` → `commandHandler(CmdStop, …)` (reuse
  `bot.IsStopCommand`)
- everything else → `handler(InboundMessage{…})`

In group rooms, strip a leading `@bot:server` mention before parsing
(use `evt.Sender` display-name matching or the
`format`-rendered `<a href="…@_userid_">` mention, as picoclaw does).

### Inbound images

`content.MsgType == event.MsgImage` → `client.Download(ctx, url.Parse())`
→ `provider.ImageBlock{MimeType: content.Info.MimeType, Data: bytes}`,
same shape Telegram produces. `MsgFile`/`MsgAudio` are ignored for now
(the agent only consumes `ImageBlock`); a follow-up could pipe audio to
a transcription tool. Captions live in `content.Body` for image
messages, combine with any text as Telegram does (`msg.Text + caption`).

---

## 4. Config & state (`$ZOT_HOME/matrix.json`)

Mirrors `bot.json`, adds the Matrix-specific fields. Mode `0600`.

```go
type Config struct {
    Homeserver       string `json:"homeserver"`         // https://matrix.org
    UserID           string `json:"user_id"`            // @zot:matrix.org
    AccessToken      string `json:"access_token,omitempty"`
    DeviceID         string `json:"device_id,omitempty"` // from Whoami; needed for E2EE
    DisplayName      string `json:"display_name,omitempty"`
    AllowedUserID    string `json:"allowed_user_id,omitempty"` // @you:server; "" = unpaired
    SinceToken       string `json:"since_token,omitempty"`     // sync cursor (≈ last_update_id)
    AutoJoin         bool   `json:"auto_join,omitempty"`       // default true
    GroupReplyAll    bool   `json:"group_reply_all,omitempty"` // default false (mention-only)
    CryptoPassphrase string `json:"crypto_passphrase,omitempty"` // "" = no E2EE
    // paired room remembered so the in-TUI bridge can send without a fresh DM:
    PairedRoomID     string `json:"paired_room_id,omitempty"`
}
```

Why separate files (`matrix.json` / `matrix.pid` / `logs/matrix.log`)
instead of sharing `bot.*`: **so Telegram and Matrix can run
concurrently** against different accounts. The Telegram daemon
plumbing already assumes a single account per protocol; parallel files
extend that naturally.

---

## 5. Login / `setup` flow

Telegram setup is “paste a BotFather token”. Matrix has no BotFather,
so setup is slightly richer. `zot matrix-bot setup` prompts for:

1. **homeserver URL** (`https://matrix…`),
2. then one of two paths:
   - **access token** — paste an existing token (e.g. from Element →
     Settings → Help → Access Token, or `curl` to
     `/_matrix/client/v3/login`). Verified via `client.Whoami(ctx)`,
     which also returns the `DeviceID` we must persist for E2EE.
   - **username + password** — `client.Login(&mautrix.ReqPassword{
     Type: mautrix.AuthTypePassword, Identifier: …, Password: …})`,
     store the returned `AccessToken` + `DeviceID`.

Either way the result is a persisted `access_token` + `device_id`, so
subsequent `run`/`start` never need the password. `reset` wipes
`matrix.json` (and ideally `matrix-crypto/`) — analogue of
`telegram-bot reset`.

> **Security note:** access tokens are bearer tokens. `matrix.json` is
> `0600`; `status` masks the token (reuse `maskToken` style). Document
> that `reset` + a fresh login is the recovery path for a leaked token,
> and that the user should `logout` the device from their client if
> they stop using the bot.

---

## 6. CLI: make `botcmd.go` protocol-pluggable

Today `runBotCommand` hardcodes `"telegram-bot"/"tg"` and every
subcommand (`setup`, `status`, `run`, `start`, `stop`, `logs`,
`reset`) is Telegram-specific only in two spots: `botSetup` (token
verify) and `botRun` (adapter construction + `RefreshCreds`). The
rest — `botStatus` config display, `botStart` detach, `botStop`
SIGTERM, `botLogs` tail, `botReset` rm — are **already generic** if
parameterised by paths and a “configured?” check.

**Proposed refactor:** a small `bot.Spec` drives a generic daemon
dispatcher, so Matrix (and any future protocol) plugs in without
copying the daemon plumbing.

```go
// packages/agent/modes/bot/spec.go (new)
package bot

type Spec struct {
    Name       string   // "telegram" | "matrix"
    Subcommand string   // "telegram-bot" | "matrix-bot"
    Aliases    []string // ["tg"] / ["mx"]
    ConfigPath func(zotHome string) string
    PIDPath    func(zotHome string) string
    LogPath    func(zotHome string) string

    // Configured reports whether the protocol is set up (e.g. token present).
    Configured func(zotHome string) (bool, error)

    // Status renders protocol-specific config lines (masked token, paired user, …).
    Status func(zotHome string) string

    // Setup is the interactive first-run flow (verify token / whoami + login).
    Setup func(tail []string) error

    // NewAdapter builds the BotAdapter for the standalone daemon, given
    // the resolved CLI args + agent wiring. It also returns a
    // RefreshCreds func for per-turn credential refresh.
    NewAdapter func(args Args, r Resolved) (adapter BotAdapter, refresh func() error, err error)

    // Reset wipes protocol state (config + any side files).
    Reset func(zotHome string) error
}
```

`runBotCommand` then becomes: match `Spec.Subcommand` or any alias →
dispatch `setup`/`status`/`run`/`start`/`stop`/`logs`/`reset` to
generic helpers that call `Spec` methods for the protocol-specific
bits. Telegram is rewritten as `telegram.Spec{…}`; Matrix is
`matrix.Spec{…}`. This is a **moderate, mechanical refactor of
`botcmd.go`** that pays off immediately (Matrix daemon = ~20 lines of
spec + the adapter) and for every future protocol.

> The `Args` / `Resolved` types live in `packages/agent` today. To
> avoid an import cycle (`bot` → `agent`), either move `Spec` into
> `packages/agent` (it’s CLI wiring, not core), or define a minimal
> `bot.RunConfig` interface that `agent.Args`/`Resolved` satisfy. The
> former is cleaner: `Spec` is CLI-glue, so it belongs beside
> `botcmd.go` in `packages/agent`. The adapter interface itself stays
> in `bot`.

Resulting CLI (mirrors Telegram exactly):

```
zot matrix-bot setup        # homeserver + token-or-password, Whoami, save
zot matrix-bot status       # config (token masked) + pid + e2ee state
zot matrix-bot run [flags]  # foreground: SyncWithContext (ctrl+c to stop)
zot matrix-bot start [flags]# background: detach + pid + logs/matrix.log
zot matrix-bot stop         # SIGTERM the background bot
zot matrix-bot logs -f      # tail logs/matrix.log
zot matrix-bot reset        # forget config + crypto store
```

---

## 7. In-TUI mirror: `matrix.Bridge` + `bot.Host`

The standalone daemon owns its agent; the in-TUI mirror reuses the
running TUI’s agent. These are fundamentally different (see §1 table),
so Matrix needs its own `matrix.Bridge` mirroring `telegram.Bridge`:

- polls via `SyncWithContext`, gates on `allowed_user_id`, downloads
  images, calls `Host.SubmitOrQueue(prompt, images)`,
- mirrors TUI-typed prompts into the paired room as `you: …`,
  forwards the assistant’s final text as bare messages
  (`OnAssistantText`), prefix logic identical to Telegram’s
  `nextReplyFromTelegram` flag,
- `Host.Notify` surfaces “connected as @zot:…”, “paired with @you”,
  “e2ee enabled” in the local transcript,
- `/matrix connect|disconnect|status` + a `matrixDialog` picker that
  clones `telegramDialog` (the only diff is the row labels and the
  “daemon already running” check switches from `telegram.IsRunning` to
  `matrix.IsRunning`).

### Opportunity: extract a generic `bot.Host` + `bot.Mirror`

The `telegram.Host` interface contains **zero Telegram types** — it’s
already `SubmitOrQueue` / `CancelTurn` / `Status` / `Notify`. The
`telegram.Bridge`’s structure (poll → gate → download → forward; plus
`OnAssistantText` / `OnUserTyped` mirroring with the
`nextReplyFromTelegram` prefix flag) is also largely protocol-agnostic
once you push pairing, image download, and command parsing into the
adapter.

**Recommendation:** for the **first cut**, mirror the Telegram
structure 1:1 (`matrix.Bridge` + `matrix.Host`) — it’s proven and keeps
Matrix isolated. **When a third protocol lands**, extract
`bot.Host` + `bot.Mirror` (generic over a `MirrorAdapter` that extends
`BotAdapter` with `PairAndFilter`/`DownloadImages`/`MirrorPrefixFor`),
and retrofit Telegram onto it. Premature generalisation now risks
getting the abstraction wrong from a sample of two; two copies is cheap
and the diff is obvious.

The one thing worth generalising **now** because it’s trivially
correct: rename `tools.TelegramSender` → `tools.MediaSender` (it’s
already `SendImage` / `SendDocument` / `Active` with no Telegram
types), keep `TelegramSender = MediaSender` as an alias for back-compat,
and have the Matrix bridge implement the same interface.

---

## 8. Outbound media tools

Telegram exposes `telegram_send_image` / `telegram_send_file` to the
model, registered only while the bridge is connected
(`applyTelegramTools`). Matrix mirrors this:

- `tools.MediaSender` (generalised from `TelegramSender`):
  ```go
  type MediaSender interface {
      SendImage(ctx context.Context, path, caption string) error
      SendDocument(ctx context.Context, path, caption string) error
      Active() bool
  }
  ```
- `tools.MatrixSendImageTool` / `tools.MatrixSendFileTool` — same
  schemas/args as the Telegram pair, but call
  `matrix.Bridge.SendImage` / `SendDocument`, which upload via
  `client.UploadMedia` and post `event.MsgImage` / `event.MsgFile`.
- `applyMatrixTools(active bool)` in `interactive.go`, identical shape
  to `applyTelegramTools`, registered on `/matrix connect`, removed on
  disconnect. Snapshots the live tool registry so extension tools added
  mid-connection survive a later disconnect.

Tool names stay protocol-specific (`matrix_send_image`, not a generic
`send_image`) so the model knows **which chat** the image is going to —
a turn can only have originated from one protocol, and the right tool
must be the one available.

### A Matrix-only bonus: message edits instead of “typing”

Matrix supports **editing** a sent message (`m.room.message` with
`m.new_content` + `m.replace` relation). A SOTA Matrix bot streams the
reply into a single message it keeps editing, then finalises — far
better UX than a 25-second typing indicator. picoclaw does exactly this
via `EditMessage` + a `ToolFeedbackAnimator`.

This is an **optional extension** to the core; Telegram can’t do it, so
it must not break the base contract. Proposed:

```go
// Optional interface a BotAdapter MAY implement. If absent, the
// Runner falls back to a single Send at turn end (current behaviour).
type StreamingAdapter interface {
    BotAdapter
    // OpenStream starts a streaming reply. The runner calls Update
    // with the full accumulated text on each EvTextDelta, and Close
    // with the final text (or an error string) when the turn ends.
    // replyTo is SendOptions.ReplyToMessageID for the first chunk.
    OpenStream(ctx context.Context, channelID, replyTo string) (Stream, error)
}
type Stream interface {
    Update(ctx context.Context, text string) error
    Close(ctx context.Context, finalText string) error
}
```

In `bot.Runner.runTurn`: if `adapter` implements `StreamingAdapter`,
open a stream and feed deltas; otherwise keep today’s
collect-then-`Send`. The Telegram adapter simply doesn’t implement it
→ zero behaviour change. The Matrix adapter implements it via
`SendMessageEvent` for the first chunk then
`client.SendMessageEvent` with `m.new_content`/`m.replace` for edits
(throttled to e.g. one edit per 500ms to avoid rate limits).

> **Decision point:** streaming edits are a real UX win for Matrix but
> add ~80 lines to the runner and a new interface. Recommend landing
> the non-streaming Matrix bridge first (single `Send`, parity with
> Telegram), then adding `StreamingAdapter` as a follow-up that
> benefits Matrix immediately and is ready for Discord/Slack later.

---

## 9. Proposed file layout

```
packages/agent/modes/matrix/        (new package — mirrors telegram/)
  adapter.go      Adapter — implements bot.BotAdapter (+ optional StreamingAdapter)
  client.go       thin wrapper over *mautrix.Client (login, whoami, send, upload, download)
  config.go       Config + LoadConfig/SaveConfig (matrix.json, 0600)
  crypto.go       E2EE: CryptoHelper init + sqlite store + goolm blank import
  daemon.go       PID/log paths + process helpers (clone of telegram/daemon.go,
                  or move both to bot/daemon.go generic)
  bridge.go       in-TUI mirror Bridge + Host interface
  io.go           stderr hook (clone of telegram/io.go)
  markdown.go     markdown→HTML + chunking (chunkMessage can be shared
                  from bot/ or duplicated; it’s rune-based and generic)
  process_unix.go / process_windows.go  (or share telegram’s via bot/)

packages/agent/modes/bot/
  spec.go         NEW: Spec type (§6) — the protocol-pluggable daemon spec
  streaming.go    NEW (optional, follow-up): StreamingAdapter interface (§8)

packages/agent/
  botcmd.go       refactored: Spec-driven dispatcher; telegram.Spec + matrix.Spec
  matrixcmd.go    NEW (optional): matrix.Spec value + matrix-specific Setup/Status
  (or fold both Specs into botcmd.go)

packages/agent/tools/
  media.go        NEW: MediaSender interface (generalised TelegramSender) + alias
  telegram_send.go unchanged except TelegramSender = MediaSender alias
  matrix_send.go  NEW: MatrixSendImageTool / MatrixSendFileTool

packages/agent/modes/
  matrix_dialog.go  NEW: picker for /matrix (clone of telegram_dialog.go)
  interactive.go    +matrixBridge, +/matrix command, +applyMatrixTools,
                    +matrixHost adapter (all 1:1 with the telegram equivalents)
```

**Shared-generic candidates** (dedup between telegram/ and matrix/):
`chunkMessage` (rune chunking), `isImageMIME`/`guessImageMIME`,
`processAlive`/`stopProcess`, PID file helpers, `maskToken`-style
masking. Move these to `bot/` (they’re already protocol-agnostic) and
have both adapters import them. Low risk, immediate dedup, and it makes
the “add a third protocol” story concrete.

---

## 10. Phasing / implementation plan

1. **Foundation (no behaviour change):** move `chunkMessage`,
   `isImageMIME`, `guessImageMIME`, process/PID helpers, `maskToken`
   from `telegram/` into `bot/` (or a `bot/util.go`). Telegram imports
   them from the new location. `go test ./...` stays green.
2. **Generalise the daemon CLI:** introduce `bot.Spec`; refactor
   `botcmd.go` to be Spec-driven; rewrite the Telegram path as
   `telegram.Spec`. Telegram CLI behaviour unchanged. (This is the
   biggest single chunk; do it behind the existing tests.)
3. **Matrix adapter (standalone daemon first):** `matrix/{config,
   client, adapter, crypto, daemon}.go` + `matrix.Spec` + `zot
   matrix-bot setup/run/start/stop/status/logs/reset`. Land E2EE
   (crypto.go) in the same step since most DMs need it. Test the
   adapter with a fake `*mautrix.Client`-shaped interface where
   practical, and a real homeserver smoke test by hand.
4. **Outbound media tools:** `tools.MediaSender` + `matrix_send.go`,
   wire into the daemon path first (the daemon owns its agent, so the
   tools attach there).
5. **In-TUI mirror:** `matrix/bridge.go` + `matrix_dialog.go` +
   `interactive.go` wiring (`/matrix`, `matrixHost`, `applyMatrixTools`).
6. **Docs:** README “Matrix bot” section + this doc linked; note the
   E2EE/`crypto_passphrase` trade-off and the access-token security
   caveat.
7. **Optional follow-up:** `bot.StreamingAdapter` + Matrix streaming
   edits; extract `bot.Host`/`bot.Mirror` when a third protocol appears.

Each phase ships independently and leaves `go test ./...` green.

---

## 11. Open decisions (need your call)

1. **Login method in `setup`:** support **both** access-token-paste and
   username+password, or token-only to start? (Recommend both —
   password login is one extra `client.Login` call and much friendlier
   for first-run.)
2. **E2EE default:** opt-in via `crypto_passphrase` (recommend) vs.
   on-by-default with an auto-generated passphrase. Opt-in keeps the
   no-CGO build working when E2EE isn’t needed and avoids surprising
   sqlite-file creation.
3. **`Spec` location:** `packages/agent` (beside `botcmd.go`, cleaner
   re: import cycles) vs `packages/agent/modes/bot` (keeps bot stuff
   together but needs a small interface to avoid `bot→agent`). Recommend
   `packages/agent`.
4. **Streaming edits (§8):** defer to follow-up (recommend) vs. land in
   the first cut.
5. **Group rooms:** ship mention-only (recommend, matches Telegram
   private-chat gate) vs. `group_reply_all` from day one.
6. **Shared helpers (§9):** move to `bot/` now (recommend, enables
   clean Matrix impl) vs. duplicate first, dedup later.
7. **Package name:** `matrix` (matches `telegram`, recommend) — note
   it’ll shadow the popular `golang.org/x/crypto/...`? No, there’s no
   stdlib `matrix` package; safe.
