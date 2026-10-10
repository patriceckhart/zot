# zot continuous (experimental)

zot continuous is the persistent, recoverable execution layer for zot. Every
model request and tool call is committed to a local store before and after it
runs, so a crash or interrupt leaves a run that the next step continues from its
last committed boundary. The existing zot engine does the actual model and tool
work. zot continuous decides what runs, records what happened, and refuses to
repeat an effect it cannot prove did not happen.

It is **experimental**. The journal format, Go APIs, and host protocol are
provisional. Ordinary zot workflows are unchanged. Everything is pure Go with no
new dependencies.

## Quick start

One-shot, no daemon:

```sh
zot continuous run "summarize the README" --store ./continuous-store --tools read,glob
zot continuous run "now list every TODO" --store ./continuous-store --tools read,glob
zot continuous run --resume --store ./continuous-store --tools read,glob
```

`run` admits the prompt to the workspace root conversation, then steps the
run to completion and prints the final answer. The second call continues the
same conversation with its full history. `--resume` performs any interrupted
work without admitting new input. `--json` streams events as JSON lines.
`--request-id <id>` makes a repeated call return the original submission
instead of asking again. Ordinary zot flags (`--provider`, `--model`,
`--tools`, `--cwd`, `--ext`, `--reasoning`, `--no-ext`, `--no-skills`) choose
how the engine is built. Session flags (`--continue`, `--session`, `--print`)
are rejected: the store is the authority, not a session file.

Hosted, with clients that come and go:

```sh
zot continuous serve --store ./continuous-store --tools read,glob
zot continuous attach "summarize the README" --socket ./continuous-store/host.sock --workspace main
zot continuous attach --follow --socket ./continuous-store/host.sock --workspace main
```

`serve` owns the store, recovers interrupted runs, executes queued work for
every conversation, and listens on a mode `0600` Unix socket. `attach`
submits, waits for the answer, and exits. Closing a client never cancels host
work. `--follow` streams committed entries until interrupted.

The interactive TUI can attach to the same host:

```sh
zot --continuous ./continuous-store/host.sock --continuous-workspace main
```

Prompts are submitted to the host, committed entries are rendered as they
land, the status bar shows `attached: host.sock`, and closing the TUI detaches
without cancelling the run. Session files are disabled: the store is the
authority. Each attached TUI keeps one conversation subscription alive for
its entire lifetime, including while its editor is idle. Multiple views of
the same workspace automatically receive each other's admitted prompts,
streamed text, tool progress, and committed answers without typing or sending
another prompt. Unsent editor text and image attachments are left untouched.
Cancelling a prompt only stops waiting for it, the view keeps receiving host
updates. Closing the view ends its subscription without cancelling host work.

A dropped watch reloads committed history and resumes automatically. New
prompt admission waits for the view's snapshot and replacement watch to be
restored, so a fast answer is not swallowed as historical context. Prompt-scoped
attached drivers replay missed committed results before reporting completion,
even if the host settled the submission while its watch was disconnected. If the
host connection itself closes, the TUI reports that live updates stopped.
Reconnect the TUI to resume, connections are not automatically redialed.
A newly attached view loads the newest history page and any live partial text.
Ctrl+C also detaches an idle CLI `attach --follow` or an attach waiting for a
submission, without cancelling the host's work.

New workspace roots use the host's provider, model, and reasoning defaults.
The attached TUI forwards only explicitly supplied `--provider`, `--model`,
and `--reasoning` selections, not its local configuration defaults. Opening
an existing root does not replace its configuration. The CLI host supports
one provider at a time and refuses a conversation configured for a different
provider before making a request. Same-provider model overrides remain
supported. SDK hosts can implement their own provider routing in `Engine.Build`.

The attached TUI leaves automatic compaction and context recovery to the host.
Local `/compact` is unavailable while attached, use the host protocol's
`conversation.compact` method for manual compaction. Extension slash commands
returning `action: "prompt"` still submit ordinary prompts to the host.
`action: "tool_prompt"` is unavailable while attached and is refused without
executing a tool or submitting work. Neither action falls back to local
provider credentials or local model execution.

### File and image attachments

The attached TUI sends clipboard images and explicitly selected local files
with the prompt. Select files through the `@` picker or drag them into the
editor as file chips. UTF-8 files are included as labelled `<file>` context,
PNG, JPEG, GIF, and WebP files are sent as inline image blocks. The client
reads the files, the host does not need access to the client's filesystem.
Directory chips and ordinary paths in prose remain references, they do not
upload directory trees or implicitly read local files.

Attachments are prompt context, not workspace uploads. The host never writes
attachment names into its filesystem. File reads honor the client's jail
setting, and file-read or validation failures leave the editor input intact.
Inputs queued behind an attached turn retain their image bytes. Alt+Up
restores queued text and images for editing. Closing the view still detaches
without cancelling admitted work.

The CLI accepts repeated `--file` flags, including an attachment-only prompt:

```sh
zot continuous attach "review these" --socket ./continuous-store/host.sock --workspace main --file ./notes.txt --file ./screenshot.png
```

One submission accepts at most 16 attachments and 1 MiB of combined decoded
file and image bytes. Unsupported binary files and mismatched image MIME types
are rejected before admission. The selected model must support vision to
interpret images. Attachment-aware clients check the host's advertised
capability and refuse image or file-field transfers to older hosts instead of
silently losing them. Legacy text-only TUI prompt drivers still reject images.

Images persist with user entries and submissions through restart, steering,
forking, and session export. The first image admission raises the store's
runtime record format to 3, so older binaries refuse to open it rather than
drop images. Existing text-only stores remain readable without migration.

While a prompt waits, the status bar says why, from committed state only:
`queued on host`, `awaiting approval: <tool>` (decide with
`zot continuous decide` or `approval.decide`), or `recovery blocked
(<action>)`. Committed tool progress (for example bash output) is shown in
the running tool's panel until its result arrives. When the conversation
has more entries than the 1000 the view loads, a note says that only the
newest entries are shown; older history remains on the host and is
reachable through `conversation.search`.

## Implemented

Execution and recovery:

- One persisted run per conversation with `request`, `tools`, and `done`
  phases, committed before each external step. Tool intents, result entries,
  failed attempts, compaction, and recovery notices are all committed records.
- Recovery of an interrupted run on the next step by the tool's replay
  contract: `never` (default, including `bash`, `write`, `edit`) reports an
  `interrupted` error result to the model; `safe` (`read`, `glob`) re-executes
  after a fresh authorization check; `idempotent` re-executes with the same
  stable operation key, which the receiver must enforce; `reconcile` asks the
  tool whether the operation completed and only executes when it reports not
  started. Unknown outcomes are reported, never retried. A policy that differs
  between the stored intent and the live tool is never widened.
  `Runtime.RecoveryPreview` lists the planned action per interrupted run.
- Persisted approvals: an `Approver` can require a human decision before a
  tool call's intent commits. The pending record is committed before anyone
  is notified, survives restarts and reconnects, binds to the normalized
  arguments, expires with a TTL or when the run is aborted, and is never
  implicitly approved. Decisions record actor, scope (`call` or `tool`), and
  reason. A tool-scoped allow is valid until the conversation's configuration
  changes.
- Steering: `SubmitWith(..., Policy: steer)` joins the active run at its next
  request boundary after the current tool calls settle; `RejectBusy` returns
  a conflict instead; `MaxQueue` bounds unclaimed submissions. Order is by
  commit revision. A queued input is not model context until it is placed:
  it joins at a boundary or starts the next chain, with a `steer` entry at
  the tail when other entries were written after its admission.
- Placement modes: `AgentConfig.SteerMode` and `FollowUpMode` are `all`
  (default: every waiting steered input joins at the next boundary, every
  queued prompt is answered by the next chain) or `one` (one steered input
  per boundary, one queued prompt per chain).
- Write submissions: `SubmitWith(..., Kind: SubmissionWrite)` appends a user
  entry without a model request. When nothing is active or queued it is
  written at admission (state `written`); otherwise it queues and is written
  in order at the next boundary or chain start.
- Queue after failure and abort: a failed chain holds inputs queued behind
  it (`Conversation.QueueHeld`); nothing starts until the next prompt
  submission, which places the held inputs first, oldest first. An abort
  withdraws queued prompts (state `withdrawn`, history kept) and writes
  queued writes; it never starts the next chain.
- Partial stream persistence: streamed text is committed as a `partial`
  record so attached clients render live output. The flusher sleeps until
  text arrives, waits `PartialFlushInterval` (default 100 ms) to batch
  deltas, and after each commit waits at least that interval and at least
  the record's size at 512 KiB/s. Each commit rewrites the whole record,
  so this bounds write volume for long answers (the largest record waits
  0.5 s) while answers under about 51 KiB keep the 100 ms cadence. The
  final assistant commit removes the record, and an attempt that fails or
  is interrupted retains its text as a final partial for inspection.
  Partial output is never model context.
- Remote execution: `WorkerServer` runs tools in another process behind a
  token with an operation ledger; `RemoteTool` calls it with the stable
  operation key, run identity, writer epoch, and environment. A lost
  connection is an unknown outcome (`core.ErrToolOutcomeUnknown`), never a
  failure that invites a retry: the run is held and recovery reconciles
  through `tool.lookup`. Stale epochs are refused by the worker.
  `zot continuous worker --root name=directory` serves this protocol on
  stdin/stdout. Repeat `--root` to share multiple repositories. Tool arguments
  carry `{repo, args}`. Optional flags are `--ledger`, `--environment`, and
  `--token`. EOF or interruption closes the worker's streams and stops serving.
  File tools enforce the shared root. Bash jail checks are best-effort accident
  prevention, not a security boundary. Only connect trusted hosts.
- Hook memos (`Memoize`, first write wins, scoped to a run or task).
- Context entries: the system prompt and tool definitions a request used
  are part of the transcript. A `context` entry is written directly before
  a response when either changed since the last context entry of the
  current context (a reset or compaction starts a new one), and carries only
  the changed part (`ContextChange`). Context entries are not model context,
  not exported as session rows, and not searched unless asked for by type.
  `PromptRecords` and `PromptSection` derive from them; stored `prompt/`
  records of older stores are still listed.
- Provider session identity: `Conversation.ProviderSession` is created with
  the conversation and sent as the provider session ID. It survives reopen,
  retries, reset, compaction, and model changes; forks and owned
  conversations get their own. Conversations written before it existed use
  their ID.
- Engine generations: `Service.Reload` validates a replacement engine with
  a probe build and publishes it atomically. In-flight requests keep their
  generation; a failed reload leaves the old one active. `zot continuous
  serve` reloads extensions on SIGHUP or `runtime.reload`, loading and
  validating a separate extension manager before publication. A failed load
  does not stop the active extension processes. Old extension generations
  are retained until host shutdown so existing agents can still use them.
  Repeated reloads therefore retain additional subprocesses until the host
  is restarted.
- Retention (`Retain`, `zot continuous retain`): age-based collection of
  deduplication records, decided approvals, retained partials, settled
  memos, and old document versions (keeping what fork points reference).
  History, runs, usage, and tasks are never collected.
- Host metrics (`Host.Metrics`, `runtime.status`): steps, failures, active
  and blocked runs, queued inputs, pending approvals, watchers, generation.
- Per-conversation tool permissions: `AgentConfig.Tools` (allowlist) and
  `DenyTools` narrow the host registry for requests and refuse calls at
  authorization. They can only remove tools; unknown names grant nothing.
- Queue operations: `Withdraw` settles a queued submission as `withdrawn`
  (history kept, never sent, request-ID retry returns the withdrawn record)
  and `Reorder` moves a queued submission to the front.
- Notification outbox: pending approvals, blocked recovery, and blocked task
  cleanup commit an `outbox` row together with the state they announce. A
  host `Notifier` delivers at least once with backoff and acknowledges on
  success; integrations deduplicate on the notification ID or read and
  acknowledge the outbox through the protocol.
- Remote workers persist their operation ledger (`LedgerPath`) so a worker
  restart still answers lookups: completed operations return their result,
  operations that were running at the crash are unknown and refused.
- Extensions may declare `replay: "reconcile"`; the host sends a `tool_call`
  with `reconcile: true` and the SDK's `Reconciler` answers.
- Transient provider failures are retried with persisted attempt counts and
  each failed attempt kept as an `attempt` entry outside model context.
- Abort of a run (`Runtime.Abort`, `zot continuous abort`), cascading to
  conversations owned by the aborted run's subagents. Unfinished intents get
  aborted error results so tool-call pairing stays valid.
- Automatic compaction before a request when the committed context exceeds the
  model's catalog window minus a reserve, and once more after a provider
  context overflow. Summaries are entries; older entries stay in history and
  are only excluded from model context. The cut never splits a tool round.
  Models without a catalog context window get no automatic compaction.
- Background compaction: above `CompactionPolicy.BackgroundTokens` (the CLI
  uses half of the usable window) the summary request runs concurrently with
  the run, at most one per conversation. The result is published at the next
  request boundary of that conversation only if no reset or compaction moved
  the context boundary since; a stale summary is discarded and counted in
  `HostMetrics.Compaction`. Host shutdown cancels in-flight summaries.
  `Service.WaitCompactions` waits for them in tests.
- A usage ledger with one immutable row per attempt and compaction, with known
  versus unknown cost (`Runtime.Usage`, `zot continuous usage`).
- Budgets at conversation and runtime scope (`SetBudget`, `CheckBudgets`,
  `zot continuous budget`): before each request, known spend plus a
  conservative reservation must fit the limit. Unknown-cost attempts count as
  reservations, never as zero. An exceeded budget fails the run before any
  request is sent. Overshoot is bounded by one reservation per active
  conversation plus whatever the provider bills late.
- The `handoff` tool: the model closes its context with a note and the run
  ends with a reset entry and exactly one admitted continuation, in one
  commit, deduplicated by the tool call.
- Historical search (`Runtime.Search`, `zot continuous search`) over the
  full history including entries before resets and compactions, with type,
  text, and time filters, fork ancestry, and cursor pagination. It is a
  sequential scan by default. `continuous.SearchIndex` is an optional,
  rebuildable in-memory trigram index over entry content; `serve` builds one
  at start and `Runtime.SearchIndexed` uses it to select candidates, which
  are then read from the snapshot and filtered exactly as the scan does, so
  results are identical. The index follows commits incrementally, rebuilds
  when its cursor falls outside retained history, and falls back to the scan
  for words shorter than three characters. It is not persisted.

Collaboration structures:

- Forks that share history with the parent up to a committed entry
  (`Runtime.Fork`), refused inside a tool round or while the parent runs.
- Reset with a handoff entry (`Runtime.Reset`): the model context restarts, the
  full history remains inspectable.
- Owned conversations and a `subagent` tool that steps a child conversation
  through the same service, bounded by `MaxDepth`.
- Versioned tasks with phases, checkpoints, timers, dependencies, ownership,
  `fail_fast` sibling aborts, background tasks, and a scheduler with concurrent
  workers. Phase handlers write only through the returned `Next`. Retry
  policies persist the next attempt deadline with exponential backoff, cap,
  jitter, elapsed deadline, and error classification (`ErrTaskPermanent` is
  never retried). `Cleanup` runs as compensation after a failed or aborted
  outcome; when it keeps failing the task stays `aborting` with `Blocked`
  set until `RetryCleanup`, never deceptively terminal. Overdue timers fire
  once after downtime; periodic timers under `CatchUpSkip` record how many
  occurrences were coalesced.
- Typed, versioned documents at conversation or runtime scope with revision
  checks, in-memory migrations, optional history, and fork semantics
  `fresh`, `current`, or `as_of`.

Host and protocol:

- `Host` runs every conversation's work, task ticks, and timers in one process
  with a recovery policy of `safe`, `all`, or `none`.
- `HostServer` speaks a newline-delimited JSON protocol over any
  `net.Listener`: token roles `read`, `submit`, `approve`, `admin`, gap-free
  commit watches with cursor expiry, bounded connections, queues, and watch
  buffers, and methods for status, conversations, submissions, steering,
  approvals, tasks, documents, budgets, search, usage, recovery preview and
  unblock.
- `continuous.Client` and `AttachedDriver` implement the client side: calls,
  watches that drop slow consumers instead of growing memory, and a prompt
  driver that renders committed entries in the interactive TUI. When a
  watch drops while the submission is still queued or running, the driver
  reopens it after the last applied revision (or reloads from a fresh
  snapshot when that history is no longer retained) and keeps following;
  the prompt is never resubmitted. Cancelled calls and watches release
  their client-side registrations immediately.
- `zot continuous serve`, `zot continuous attach`, and `zot --continuous`
  wrap them for local use.
- `/swarm` on an attached TUI runs each agent as an owned conversation on the
  host (`swarm.HostRunner`); see "Swarm on a host" below.
- Extension tools declare `replay` on `register_tool` and receive the
  `operation_key` on durable calls; see docs/extensions.md.

Storage:

- A backend-neutral Go storage contract under `packages/continuous/storage`.
- Atomic JSON-record commits with mandatory revision and writer-epoch checks.
- Memory storage for ephemeral use and semantic tests.
- A local journal with framed, checksummed, schema-versioned commits in
  rotated segments, a rebuildable offset index and record-index checkpoints
  for bounded recovery and bounded memory, exclusive writer locks, free
  detached snapshots, paginated reads, and a gap-free pull watch.
- A SQLite backend (`packages/continuous/storage/sqlite`, pure Go through
  `github.com/ncruces/go-sqlite3` on the wazero runtime zot already ships)
  with versioned records, indexed history, and bounded memory; selected with
  `--backend sqlite` for new stores.
- Durable queued admission with request-ID deduplication and per-conversation
  configuration with optimistic revision checks.
- Atomic legacy session import and snapshot-consistent export.
- Offline `import`, `export`, `status`, `conversations`, `verify`,
  `check-state`, `recover --dry-run`, `abort`, `usage`, `backup`,
  `verify-backup`, and `restore` commands.
- Integrity checks covering admission, runs, usage, resets, compactions, forks,
  owners, tasks, documents, and owned-conversation keys.
- Crash matrices at every persistence and execution commit boundary, a
  subprocess crash inside a tool, concurrent steppers, and two clients steering
  one conversation.

The Go API is trusted-local. Actor strings record provenance, not
authorization. Authorization exists only at the host protocol layer, by token
role. Do not expose the Go API or an unauthenticated socket to untrusted
clients.

## Store management CLI

```sh
zot continuous import ./old-session.jsonl --store ./continuous-store
zot continuous status --store ./continuous-store
zot continuous verify --store ./continuous-store
zot continuous check-state --store ./continuous-store
zot continuous backup ./history.zotbackup --store ./continuous-store
zot continuous verify-backup ./history.zotbackup
zot continuous restore ./history.zotbackup --store ./restored-store
zot continuous verify --store ./restored-store
zot continuous format --store ./continuous-store
zot continuous migrate ./migrated-store --store ./continuous-store
zot continuous conversations --store ./continuous-store --limit 100
zot continuous export <conversation-id> --store ./continuous-store --format session --output ./projection.zotsession
zot continuous tasks <conversation-id> --store ./continuous-store
zot continuous inspect <task-id|approval-id|submission-id|conversation-id> --store ./continuous-store
zot continuous approvals <conversation-id> --store ./continuous-store
zot continuous decide <approval-id> --allow --scope call --reason "reviewed" --store ./continuous-store
zot continuous budget <conversation-id> --limit-usd 2 --store ./continuous-store
zot continuous search <conversation-id> --text needle --type assistant --store ./continuous-store
zot continuous abort <conversation-id|task-id> [--include-background] --store ./continuous-store
```

Commands work without provider credentials and require an explicit store path,
except `verify-backup`, which takes an archive path and rejects `--store`.
Import may create the store if its parent exists. Restore and `migrate` require
a new directory with an existing parent. Other store commands require an
existing store.

`format` reports the journal's commit schema (`journal.InspectFormat`) and
whether this build can open it (`current`) or rewrite it (`migratable`). It
is read-only and rejects `--durability`. `migrate` (`journal.Migrate`) rewrites
the committed prefix into a new directory at `journal.CurrentSchema`: every
commit is converted by the registered migration for its schema, validated with
the current rules, and written with fresh framing. The source is locked and
unchanged; the destination keeps the archived revision and epoch, and the first
writer open advances them. Migrating a current store is a verified rewrite. A
schema this build does not know fails with `journal.ErrSchemaUnsupported` and
leaves no destination. Take a backup first; an interrupted migration leaves a
partial destination that fails verification. Journal verification and `Open`
accept exactly the current schema.
Source journal commands acquire the exclusive writer lock. Import, export,
status, listings, and `check-state` commit a newer writer epoch. Backup and read-only verification
do not advance it. None can run alongside an active source writer. Archive
verification does not acquire a journal lock. A future host/client surface must
route inspection and backup to the host instead.

Status and conversation listings emit JSON. Listing returns a snapshot revision and
an `after` cursor (the last returned ID). Pass `--after <id>` for the next page.
Export defaults to JSONL on stdout. `--output` exclusively creates a mode `0600`
file, never overwrites an existing file, and removes its own incomplete output on
reported write failure. Abrupt termination can still leave partial output. Exports
are derived files, not acknowledgement of a new durable runtime operation.

Strict durability is the default for writer commands and backup/restore
publication. Windows currently requires their explicit `--durability process`
option. There is no automatic downgrade. Journal and archive verification do not
write or synchronize data and reject `--durability`, including on Windows.
`run` and `serve` are the only commands that dispatch model requests or tools.
`zot --continuous <address>` attaches the interactive TUI to a running `serve`.

## Legacy session compatibility

`ImportSession(ctx, reader)` reads JSONL without modifying the source file. Import
closed sessions, not files being concurrently appended. A successful admission
commits all source entries and import provenance atomically. Retry with identical
source bytes to obtain the same conversation, including after restart. Different
source bytes using an existing session ID are a conflict. IDs are retained when
present, generated when absent, and checked for path separators and invalid size.
Whitespace-only changes also change the source digest.

Imports preserve JSON field values, source row order, message content, usage rows,
ancestry, metadata revisions, unknown typed application rows, and compaction
history. Source row whitespace is normalized. Compaction is not flattened or used
to delete older entries. Historical execution checkpoints are explicitly recorded
as unavailable, not inferred from a transcript.

Missing local tool results produce error-only interruption records in the export
projection and an import repair notice with source row and call IDs. Original
source rows remain immutable. Existing result messages get missing errors merged
in the projection. Provider-executed server tools do not receive synthetic local
results. Malformed rows, orphan results, duplicate results and mismatched roles
are rejected atomically rather than silently discarded. Tool IDs may be reused
across separate paired turns. Empty legacy compaction checkpoints are supported.

Import is currently limited to 4 MiB of source data, 10000 nonempty rows, and the
backend's 8 MiB encoded commit limit. Repair projections and record overhead count
against that commit limit. Large-session staging and bounded-memory import remain
unimplemented. A failed import never publishes partial conversations or entries.
Opening the CLI store can still commit writer acquisition before validation fails.

`ExportSession(ctx, id, writer)` uses one committed snapshot. Imported meta rows are
retained in order. If the current provider/model differs, a final meta revision is
appended. Queued inputs are represented as user messages. Export is a legacy
projection, not a lossless export of tasks, documents, approvals, or the store.
It contains private transcript data, so share it deliberately. Cancelling the
context stops subsequent work, but cannot interrupt a caller's blocking reader or
writer. API callers must manage cancellation of their own I/O.

## Execution model

Every model generation, tool call, compaction, and subagent is a durable task
run by one scheduler. There is no second loop driving runs. A conversation
has at most one active generation chain, recorded under `chain/<id>`, which is
also the run identity clients see.

```text
admission  submission + user entry + chain/<conversation> + zot.generation task, one commit
request    background summary published if ready; threshold compaction as an
           owned zot.compaction task; StartEffect; one model attempt
           (Agent.Turn, no in-memory retries); then assistant entry + usage +
           prompt record + owned zot.tool tasks, one commit
tool       authorize (configuration, host guard); approval; StartEffect with
           effective args, replay policy, approval ID, engine generation;
           execute; then tool_result entry + task outcome, one commit
collect    after every tool task is terminal: handoff, steering, next turn,
           or final answer + submission settlement + next queued chain
```

Every external step (one provider request, one tool execution) sits between
two commits. The commit before records that the effect may start
(`Task.Effect = "started"`), the commit after records what happened. A task
picked up by the scheduler without that mark definitely did not start its
effect. Recovery resumes each task from its last commit:

- A generation with its effect started may have sent its request. It is sent
  again and the earlier attempt's usage is recorded as unknown, never zero.
- A tool task that never started runs after fresh authorization.
- A tool task with its effect started went through the replay contract. If
  the stored policy and the current tool both allow replay and the current
  authorization accepts the committed effective arguments unchanged, it runs
  again (`safe`, `idempotent` with the same operation key) or is reconciled
  (`reconcile`). Otherwise the model receives an `interrupted` error result
  and a recovery notice is recorded on the chain. Nothing is retried
  silently.
- A `reconcile` tool whose outcome is unknown (for example a lost remote
  worker) keeps its effect marked and waits on a backoff timer. Each wake
  reconciles through the operation key; after 5 unanswered passes the call is
  reported to the model as possibly applied. It is never retried as if it had
  failed.

Identities: the chain's run ID is used for operation keys, approvals, usage
rows, and subagent ownership. It survives retries, turns, migration, and
restarts; task IDs do not. The provider-facing session ID is the
conversation's provider session. Inputs admitted while a chain is active
queue; an answered chain starts the next one in the commit that settles it,
a failed one holds them, and an aborted one withdraws them.

Cancelling the context (`Ctrl+C` for the CLI) or shutting a host down is
treated like a crash: committed intent stays and the next start resumes it.
Observer disconnects cancel nothing. An explicit abort is different: its
committed mark survives an in-flight model response, a started effect is
joined rather than cancelled, the remaining calls get aborted results, and
the inputs settle aborted. The model never sees a tool call without a result.
Failed attempts are stored as `attempt` entries; they stay inspectable and
are excluded from model context and legacy export.

Waiting never holds a worker: a generation waiting on its tool tasks, a tool
waiting on an approval, and a subagent call waiting on its child are parked
records the scheduler revisits when a commit or persisted deadline wakes it.
Tools of a round run sequentially, each tool task waiting on its predecessor.

`Service.Step(ctx, conversationID)` drives one conversation and the
conversations it owns in the caller's goroutine, on the same task
definitions, until no chain is active and nothing is queued. It returns the
run-shaped projection of the last chain, `ErrAwaitingApproval` when a call
waits for a human, and `core.ErrToolOutcomeUnknown` while a reconciliation is
pending. `Host.Run` drives every conversation.

The `Engine` interface builds a `core.Agent` per request from the host's
configuration. The runtime only replaces its transcript with the committed
context, applies the conversation's model, reasoning, and instructions, and
calls `Agent.Turn` for exactly one request and `Agent.CallTool` for exactly one
authorized call. Provider clients, credentials, tool registries, sandboxes,
extensions, and confirmation remain host concerns.

`core.ToolReplayer` is the opt-in contract for tool authors:

```go
func (t *LookupTool) ReplayPolicy() core.ToolReplayPolicy { return core.ReplaySafe }
```

Declare it only for tools whose repeated execution has no external effect.
Tools without it default to `ReplayNever`. Extension tools default to
`ReplayNever`; the extension protocol has no replay field yet.

### Observation

Nothing reaches an observer before its commit. The engine events of a model
attempt or tool call are held by the invocation: streamed text and tool
progress are released after the `partial` or progress commit that records
them, everything else after the commit that settles the phase. An invocation
that crashes, is fenced out, or is aborted before its commit publishes
nothing, which matches what a client reads after a restart. With partials
disabled (`PartialFlushInterval < 0`) streamed text arrives with the
response commit.

A conversation view is the live state of a conversation as typed documents:
`execution` (chain and live tasks), `queue`, `usage`, `agent` (the
configuration), `provider` (provider, model, provider session), `entries`,
`partial`, `progress`, and `approvals`. They are projections of the records
the runtime writes in the same commits, so they cannot disagree with
execution. `Runtime.WatchView` delivers, per commit, the operations it
applied to these documents (`ViewChange{Revision, Ops: [{Doc, Op, Key,
Value}]}`) instead of raw store operations.

`Runtime.SubscribeAgentEvents` returns a fresh `ConversationSnapshot` and
the agent events derived from commits after it: `message_start`,
`message_update` (committed partials), `message_end` (committed entries),
`tool_execution_start` (intent committed), `tool_execution_update`
(committed progress), `tool_execution_end` (result committed),
`compaction_start`, and `compaction_end`. The buffer bounds the lag; a
subscriber that falls behind ends with `ErrEventLag` and resubscribes for a
fresh snapshot.

### Extensions and hooks

`ExecutionOptions.Extensions` is an `ExtensionRegistry` of named
`Extension`s: tools, a system prompt section, hooks, a tool wrapper, and
task definitions. A conversation selects extensions by name in
`AgentConfig.Extensions`; the store keeps only the names. Every request and
tool call resolves them at that moment, so `Register` with an existing name
replaces the extension for the next use without touching work in flight. A
conversation selecting a name the registry does not have fails its chain
(`ErrExtensionMissing`); it never runs without it. Extension tools are
subject to the conversation's allow and deny lists and may not shadow a
host tool. Extension task kinds run on the same scheduler; the `zot.`
prefix is reserved.

Hooks run inside the phase they belong to, before its commit, so what they
decide commits with the phase. A crash can run a phase and its hooks again;
hooks must be deterministic with respect to committed state.

| Hook | Runs | May |
|---|---|---|
| `BeforeRequest` | before each model attempt, after compaction and placement | change system prompt and messages |
| `AfterResponse` | after an accepted response, before its commit | observe |
| `OnYield` | when a response has no tool calls | return a prompt that continues the run (a `continue` entry) |
| `BeforeTool` | before authorization of each call | block with a reason, or rewrite the arguments |
| `AfterTool` | after an executed call, before its result commits | replace the result |
| `AfterTools` | after every result of a round committed | end the run without another request |

A hook error refuses the call (`BeforeTool`) or fails the chain. An
`AfterTool` error cannot undo the executed effect: the original result
stands and the error is recorded as a notice.

### Tools

`AgentConfig.ParallelTools` runs the calls of one round concurrently. Each
call keeps its own tool task, committed intent, replay policy, and recovery;
a crash with several calls running recovers each by its own policy. Results
commit in completion order and reach the model in call order. A round that
contains a `handoff` call stays sequential, because the handoff skips the
calls after it.

A tool result with `core.ToolResult.Terminate` asks to end the run. When
every executed result of a round asks for it, the run ends answered after
the round without another model request. A round with any other result
continues.

### Limits of the current execution

- Tools within one round execute sequentially, in call order, unless
  `ParallelTools` is set.
- Streamed text is committed as a bounded `partial` record (at most
  `MaxPartialBytes`, 256 KiB) at the flush interval; a crash loses at most
  that window. Tool progress keeps the last 16 KiB per running call.
- Queue-policy submissions made while a chain is active are answered by the
  next chain, in slot order (`Reorder` changes it); steer-policy submissions
  join the active chain at its next request boundary.
- Authorization is layered: the conversation's tool allow and deny lists,
  the host's `BeforeToolExecute` guard, and the optional `Approver`.
  `run`/`serve` do not open confirmation prompts; approvals are decided
  through the CLI (`zot continuous decide`) or the protocol
  (`approval.decide`).
- Provider sticky-session IDs are the conversation's provider session.
  Reasoning blocks and tool images round-trip through the stored message
  JSON like session files do.
- One writer process at a time. The writer lock prevents a second `run` or
  `serve` on the same store; it does not coordinate two hosts.
- Reconciliation over the extension protocol depends on the extension
  keeping its own operation ledger; without a `Reconciler` every answer is
  unknown.
- The attached TUI renders streamed text from partial records at the flush
  cadence (slower for very long answers), not per provider token. Tool
  progress is committed at most every 100 ms plus once before the result.

## Host and protocol

`continuous.NewHost(runtime, engine, HostOptions{...})` returns a `Host`.
`Host.Run(ctx)` migrates interrupted runs written by earlier builds, applies
the recovery policy, runs the task scheduler (with any additional
`HostOptions.Tasks` definitions), sleeps on store commits and persisted
deadlines, joins every task invocation, and returns when `ctx` ends.
Interrupted work stays recoverable.
`RecoveryPolicy` decides what happens at start:

- `safe` (default) runs automatic recovery actions. Chains whose interrupted
  tool cannot be replayed stay held, every task of the conversation parked,
  until `Host.Unblock(runID)` or the protocol's `recovery.unblock`.
- `all` unblocks everything, reporting interrupted unsafe tools to the model as
  errors. It never replays what the policy forbids.
- `none` performs no recovery until a held run is explicitly unblocked. New
  submissions run in conversations without a held run.

Queued submissions do not release a recovery hold or a pending approval. They
wait until the held run is unblocked or the approval is decided. An explicit
abort can settle a run waiting on approval without authorizing the tool.

`HostServer{Host, Engine, Tokens, Version, MaxWatches}` serves newline-delimited
JSON frames. Requests are `{"id","method","params"}`. Responses are
`{"type":"response","id","method","success","data"|"error","code"}`. Watches
additionally stream `{"type":"commit","id","commit"}` frames until
`watch.cancel`, the connection closes, or the cursor expires.
`conversation.watch` takes `mode`: `commits` (default, raw commits), `view`
(`{"type":"view","watch_id","change"}` frames with operation-level changes
of the conversation view), or `events` (the response carries a fresh
`snapshot`, followed by `{"type":"agent_event","watch_id","event"}` frames;
a lagging subscriber ends with `event_lag`). The first frame
must be `hello` with a token when `Tokens` is set. Without a `Tokens` map,
every connection is admin, which is only acceptable on a private local socket.

| Role | Methods |
|---|---|
| read | `hello`, `runtime.status`, `conversation.list`, `conversation.snapshot`, `conversation.watch`, `watch.cancel`, `conversation.search`, `submission.get`, `submission.wait`, `usage.get`, `recovery.preview`, `approval.get`, `approval.list`, `task.get`, `task.list`, `task.view`, `document.read`, `budget.get`, `memo.get`, `prompt.records`, `prompt.section`, `partial.get` |
| submit | read plus `conversation.create`, `conversation.submit`, `conversation.configure`, `conversation.compact`, `conversation.reset`, `conversation.fork`, `document.write`, `memo.set` |
| approve | submit plus `approval.decide` |
| admin | approve plus `conversation.abort`, `recovery.unblock`, `task.abort`, `task.retry-cleanup`, `budget.set`, `runtime.reload`, `runtime.retain`, `outbox.ack`, `submission.withdraw`, `submission.reorder` |

`conversation.create` with `workspace` opens or finds the workspace root.
`HostServer.DefaultConfig` supplies provider, model, and reasoning fields
omitted when creating a new root. It does not change existing roots. With
`owner` (`{conversation_id, id}`) and `key` it creates an owned child of that
conversation once per `(owner.id, key)` and returns the existing child on a
retry; `config` overrides the inherited parent configuration.
`conversation.submit` accepts `request_id`, `policy` (`queue` or `steer`),
`when_busy` (`reject` or `steer`), and `kind` (`write` for an entry without
a model request). Optional `images` is an array of `{mime_type, data}` image
blocks, optional `files` is an array of `{name, data}` attachments. `data` is
base64-encoded bytes, not a URL or path for the host to fetch. `content` may be
empty for attachment-only input. Image blocks are returned in submissions and
user entries as `images`. Text-file bytes become labelled text in `content`.
Request-ID deduplication includes attachment bytes and file names, changed
payloads return `duplicate_key`. `runtime.status` advertises
`capabilities.attachments: true`, clients must check it before sending new
attachment fields to an older host. These fields are additive, text-only
clients and existing stored submissions continue to work. `document.read` and `document.write` need
a `Documents` registry on the server, otherwise they return `unsupported`.

Error codes: `bad_request`, `unauthorized`, `forbidden`, `not_found`,
`conflict`, `duplicate_key`, `busy`, `queue_full`, `budget_exceeded`,
`blocked`, `unsupported`, `cursor_expired`, `event_lag`, `storage`,
`cancelled`, `limit`, `error`. A watch cursor outside the store's retained
history returns `cursor_expired` before the watch is acknowledged. The client
must take a new snapshot. Errors after acknowledgment terminate the watch,
and the Go client closes its commit channel so consumers can resnapshot or
reconnect.
`MaxWatches` bounds watches per connection, `MaxQueue` unclaimed submissions
per conversation, and `MaxConnections` concurrent clients. Excess connections
receive `limit` and are closed.

`zot continuous serve` listens on `<store>/host.sock` (mode `0600`) or
`--socket <path>`. `--listen host:port` requires `--token-file`, a file with
one `<token> <role>` per line, tokens of at least 16 characters, not group or
world readable on Unix. Loopback addresses may be plaintext. Any other address
requires `--tls-cert` and `--tls-key` (TLS 1.3); `--tls-client-ca` adds mutual
TLS on top of tokens. There is no default public listener. On Windows the
socket path names a private named pipe instead (`continuous.LocalEndpointName`
maps `<store>/host.sock` to `\\.\pipe\zot-continuous-<hash>-host-sock`) with a
security descriptor granting access to the creating user and SYSTEM only, and
remote clients rejected; `--socket` and `--continuous` accept either the store
path or the pipe name. `continuous.ListenLocal` and `DialLocal` are the
platform-neutral Go surface. Clients connect with `--tls-ca` (`attach`),
`--continuous-tls-ca` (TUI), or `continuous.DialTLS`.

`--web host:port` adds a WebSocket endpoint for browser clients next to the
local socket. Each text message carries one protocol frame, a JSON object
without the trailing newline; requests, responses, watches, and roles are
the same as on the socket. The endpoint always requires `hello` with a
token: the `--token-file` tokens when given, otherwise one generated admin
token stored in `<store>/web-token` (mode `0600`, reused across restarts,
delete it to rotate). Startup logs show only the file path, not the token.
Read the file locally to retrieve the token. The socket keeps its own
authentication. A non-loopback `--web` address requires `--tls-cert` and
`--tls-key`. To reach a host remotely, keep `--web` on loopback and put a
TLS tunnel in front of it, for example
`tailscale serve --bg --https=443 http://127.0.0.1:7787` or
`cloudflared tunnel --url http://127.0.0.1:7787`, and connect with `wss://`.
Repeat `--web-origin https://app.example` to restrict the browser `Origin`
header. That is a defense against other web pages, not authentication:
non-browser clients can send any origin, so the token is the access check.
The endpoint accepts text messages up to 1 MiB, rejects binary and unmasked
frames, and supports no WebSocket extensions or subprotocols. Go servers use
`HostServer.WebSocketHandler`.

```sh
zot continuous serve --store ~/.zot-store --web 127.0.0.1:7787
```
`zot continuous attach` is the reference client: `--socket` or `--address`,
`--token` or `--token-file`, `--workspace <id>` or `--conversation <id>`, a
prompt and/or `--follow`, and `--json`. `zot --continuous <address>` attaches
the interactive TUI with `--continuous-workspace`, `--continuous-token`, or
`--continuous-token-file`; it rejects print, stream, JSON, RPC, and session
flags. Go clients use `continuous.Dial`, `Client.Call`, `Client.Watch`, and
`AttachedDriver`.

## Tasks, documents, forks, and subagents

Tasks are code-defined state machines committed alongside the conversation.
Register kinds in a `TaskRegistry`; each `TaskDefinition` has phases that
receive a `TaskContext` (committed task, snapshot, waited outcomes) and return a
`Next` (next phase and checkpoint, `WaitOn` task IDs with a wait policy,
`WakeAt` timer, extra storage operations such as `DocumentWrite`, child task
specs, or an outcome). The scheduler commits the returned `Next` atomically,
then runs the next phase. Handlers must not perform external effects outside
that contract. Faults (no progress, undefined phase, self-wait, missing waited
task) fail the task and are recorded. `Background` tasks outlive their
conversation's run and are only aborted with `includeBackground`. Phases
starting with `__` are reserved. An initial state may carry a `WakeAt` timer
so a task starts parked (a reminder). `TaskDefinition.Retry` and `Cleanup`
add retries with persisted deadlines and compensation; `TaskScheduler.CatchUp`
selects how overdue periodic timers behave after downtime. A scheduler only
reports idle after collecting finished invocations and checking for follow-up
work, so recovery does not stop between committed phases.

Documents are typed JSON values registered in a `DocumentRegistry`. Reads
migrate older versions in memory and return `ErrDocumentBlocked` if no
migration exists. Writes and deletes require the expected revision and return
`ErrDocumentConflict` otherwise. `History: true` retains every value for
`as_of` forks and historical reads.

`Runtime.Fork(parentID, atEntry, config)` creates a conversation that shares
history up to the given entry. Forks inside a tool round or of a running parent
are refused. `Runtime.Reset(id, handoff)` appends a reset entry; the model
context starts from the handoff text. `Runtime.CreateOwnedConversation` makes a
child owned by a run or task; the `subagent` tool uses it, and aborting the
owner aborts the children.

### Task-scoped commits

A phase may need to record intent before its external effect. Inside a
handler, `tc.StartEffect(ctx, checkpoint, build)` commits the task with
`Effect: "started"`, an optional new checkpoint, and any operations added
through the `TaskTx`. It is fenced on the task's current invocation
(`Task.Invocation`, renewed at every reservation), so a stale invocation gets
`ErrStaleInvocation`, and it is refused with `ErrTaskAborting` once abort was
requested, so no effect starts after a committed abort. The next applied
`Next` clears `Effect`. A reservation alone never sets it: after a crash,
`TaskContext.Interrupted` is true only when an effect may have started.
`tc.Commit` writes task-scoped records without the effect marker.

`Next.Commit` builds the final `Next` inside the transaction that applies it,
against the fenced snapshot. `TaskTx.CreateChild` creates an owned task whose
ID the returned `Next` may wait on in the same commit. On an unrelated storage
conflict the builder reruns on fresh state, so a phase whose effect already
happened is not repeated. `Next.AfterCommit` runs once after the commit, for
observers that must not see uncommitted state.

`TaskDefinition.JoinOnAbort` keeps a running invocation alive when abort is
requested: the abort handler runs after the invocation committed what
happened. Without it, abort cancels the invocation's context. Waiting tasks
hold no worker. `TaskScheduler.Join` waits for every started invocation;
`Host.Run` joins before it returns.

`Next.WaitApproval` parks a task on an approval record until it is decided
or expires. `Next.Link` makes the task responsible for tasks outside its
ownership tree (a subagent's generation in its own conversation): abort
marks them, and the task settles after them. `TaskScheduler.Filter`,
`Hold`, and `Prepare` restrict dispatch, hold tasks for the recovery policy,
and run admission work at every tick.

Recovery cannot guarantee exactly-once external effects. A started effect is
reported as possibly applied unless the receiver enforces the operation key or
reconciliation succeeds.

### Built-in tasks

The built-in kinds are `zot.generation`, `zot.tool`, and `zot.compaction`, all
at checkpoint schema version 1. Clients see tasks as `pending`, `running`,
`waiting` (on tasks, a timer, or an approval), `aborting` (cleanup), or
`terminal`. `ConversationSnapshot` carries the active `chain`, a run-shaped
`run` projection (or the last settled one), `recovery`, and committed tool
`progress`; `task.view` returns live tasks with `owns`, `waits`, and `links`
edges.

- Approvals park the tool task on the approval record. Decisions bind to the
  effective arguments. Abort expires the chain's pending approvals.
- Compaction runs as `zot.compaction` tasks whose result is the summary.
  Threshold and overflow compactions make the generation wait; overflow
  compacts at most once per turn. Background summaries are durable
  conversation-level tasks published at the next request boundary, or
  discarded as stale after a reset or competing compaction.
- A `handoff` result commits the result, skipped results for later calls
  (their tasks settle without running), the reset entry, and the
  continuation admission together.
- The `subagent` tool creates an owned conversation and its submission in one
  commit, keyed by the call, and parks on the child's generation. The child's
  generation is linked: aborting the call aborts the child, and the call
  settles only after it. Nesting is bounded.
- Partial output is written through fenced task commits, so a stale
  invocation cannot overwrite a newer attempt; the response commit replaces
  it. Observers receive in-process events immediately; tool results are
  published to them after their commit.

### Upgrading stores from earlier builds

Earlier builds drove conversations with a run record (`run/<id>`). On open,
`run`, `serve`, `Service.Step`, and `Host.Run` convert every unfinished run
into a chain, one conversation per commit (`Runtime.MigrateLegacy`, CLI
`migrate-runs`). A pass interrupted by a crash resumes on the next open and
never converts a run twice. The chain keeps the run ID and submissions. A
`request` run becomes a generation whose effect may have started. A `tools`
run becomes a generation waiting on one tool task per intent: `pending`
intents start fresh, `running` intents carry their committed arguments and
replay policy with the effect marked started, `done` intents become terminal
tasks naming the existing result entry. Approvals, results, and usage stay
where they are. The old run settles with outcome `migrated`.

A run that cannot be converted without guessing what an interrupted effect
did (for example a running intent without committed arguments) is left as
it was and blocks its conversation: new inputs queue, nothing executes, the
outbox carries a `migration.blocked` notice, and `Runtime.Abort` (CLI
`abort`) settles it with paired aborted results, after which the queue
proceeds. Nothing is inferred.

The store carries a `runtime/format` record, separate from the backend's
commit schema. Version 1 is the run layout; the first chain raises it to 2.
A build refuses a store whose format is newer than it supports
(`ErrUnsupportedFormat`) instead of reinterpreting unknown records. `status`
reports `runtime_format`. Builds that only understand version 1 cannot open
an upgraded store; take a backup before upgrading if you may roll back.

Measured on one machine (`go test ./packages/continuous -run '^$' -bench .`):
an answered submission takes 4 commits and about 4.8 ms on memory, 6 ms on
the journal (`RunRoundTrip`); an idle host uses about 1% of one core
(`IdleHost`); opening a journal store with 200 settled chains and computing
the recovery preview takes about 6.8 ms (`RestartTasks`).

### Swarm on a host

When the interactive TUI is attached with `--continuous`, `/swarm` uses
`swarm.HostRunner` instead of spawning `zot --swarm-agent` child processes.
Each swarm agent is an owned conversation of the workspace root (owner ID
`swarm/<agent id>`, key `<agent id>`), created with `conversation.create` and
reused on resume. The initial task is submitted with a stable request ID so a
re-run does not queue it twice. `/swarm send`, `kill`, `resume`, and the
dashboard work unchanged: input is submitted to the host, `cancel` aborts the
child's active run, `kill` detaches the supervisor without aborting host work,
and `resume` reattaches to the same conversation. The agent's transcript and
`events.jsonl` mirror committed entries; the durable record is the host
conversation, whose ID the dashboard shows. `/swarm remove` deletes only the
local mirror. There is no inbox socket and no per-agent session file in this
mode. The subprocess runner remains the default without `--continuous`.

## Minimal Go example

```go
store, err := journal.Open(ctx, storePath, journal.Options{})
if err != nil {
    return err
}
runtime, err := continuous.New(store)
if err != nil {
    store.Close()
    return err
}
defer runtime.Close()

conversation, err := runtime.OpenRoot(ctx, "explicit-workspace-id", continuous.AgentConfig{
    Provider: "configured-provider",
    Model:    "configured-model",
})
if err != nil {
    return err
}
submission, err := runtime.Submit(ctx, conversation.ID, "local-actor", "request-123", "Review this checkout")
if err != nil {
    return err
}
svc, err := continuous.NewService(runtime, engine, continuous.ExecutionOptions{})
if err != nil {
    return err
}
if _, _, err := svc.Step(ctx, conversation.ID); err != nil {
    return err
}
_ = submission // Answered when Step returns, or recoverable if it did not.
```

Imports are `github.com/patriceckhart/zot/packages/continuous` and
`github.com/patriceckhart/zot/packages/continuous/storage/journal`.
The store path must have an existing parent directory. Use a dedicated store
directory. New directories use mode `0700`, new files use `0600` where supported.
Existing permissions are not rewritten. On Windows, protect the directory with
appropriate ACLs. This path handling is not a security sandbox.

Opening a root again returns its original configuration. Admission stores content
byte-for-byte. A request ID is scoped to this store, conversation, actor, and
submission operation. Identical retries return the original submission.
Different content with the same key is a conflict. An empty request ID disables
deduplication. Deduplication records currently have no expiry or retention policy.

`Configure` compares the conversation revision. A successful change advances it.
Queue admissions also advance that revision. Unrelated conversation writes do not
invalidate it. A conflicting update returns the current conversation and an error.

## Committed-state observation

Read a snapshot, retain its revision, then scan commits strictly after that
revision. Apply the returned operations in order and advance the cursor to each
commit revision. When the scan is empty, call `Wait(ctx, cursor)` and scan again.
A commit arriving between snapshot, scan, and wait is not lost. Cancelling a
wait only cancels observation. Closing the runtime wakes waiters and closes the
owned store, it does not remove queued submissions.

Pages and scans require limits from 1 through 1000. Use the last record key as the
next page's exclusive cursor. Commits are limited to 8 MiB encoded. Commit cursors
beyond the current revision are rejected. There is no retention yet, so no old
cursor expires. There are no per-client output buffers or producer goroutines.

Journal recovery streams one validated commit at a time into the current record
map. `Scan` reads history directly from the committed journal using positional
I/O, so it does not move the writer's append position. Old commit payloads,
including overwritten or deleted values, are no longer cached in the writer.
The journal layout and backup format are unchanged, existing prototype stores
still open without migration. Memory storage continues to retain its history.

Disk scan pages retain at most the requested number of commits, each subject to
the 8 MiB encoded commit limit. They currently reread and validate the prefix
from revision one through the requested page, with no offset index or checkpoint
acceleration. Scans hold the transaction serialization lock while reading and
can delay commits. Repeated paging through a large journal can be quadratic in
read work. Current records still include every retained transcript entry, and
snapshots still copy the record map. This is an incremental memory reduction,
not large-store readiness or bounded historical memory.

## Journal format and recovery

A store contains:

```text
writer.lock
commits.log                  logical offsets [0, size)
boundary                     committed revision and logical end
segments/<start>.log         rotated segments, logical offsets from <start>
indexes/offsets              derived: frame offset per revision
checkpoints/<revision>.ckpt  derived: record index at a revision
```

Authority is the committed prefix of the logical stream (`commits.log`
followed by `segments/*.log` in order), bounded by `boundary`. Each commit
frame has a magic/version marker, length, revision, payload checksum, header
checksum, and JSON payload. The boundary file records the committed byte
boundary and revision before acknowledgement; it is published by synchronized
temporary-file creation and atomic replacement. The writer rotates to a new
segment once the active one exceeds `Options.SegmentBytes` (default 64 MiB);
a frame never straddles files, and sealed segments are never written again.
A missing or shortened segment fails closed.

`indexes/` and `checkpoints/` are derived and never authoritative. The offset
index is checked entry by entry against frame headers at open and rebuilt
where it disagrees. A checkpoint is the record index (key, value offset,
length) at one revision, written every `Options.CheckpointEvery` commits
(default 1000) and at close; open loads the newest checkpoint whose recorded
end matches the journal, validates only the frames after it, and discards
checkpoints that are torn, stale, or from another journal. Deleting both
directories is safe; the next open rebuilds them by scanning the log.

Current records are an in-memory persistent tree of key to value offset:
memory grows with live keys, not with history or value size, and a snapshot
is a root pointer, so taking or holding one costs nothing and later commits
never affect it. Values are read from the log on demand. `Indexed` and
`BoundedHistoryMemory` are reported true. Recovery reads the offset index,
the newest checkpoint, and the frames after it; it does not decode every
frame. Verification (`verify`, `backup`) still scans the whole committed
prefix by design.

Recovery checks the boundary, frame sequence, checksums, schema, operations,
and epochs of the frames it decodes. Corruption or truncation inside the
recorded committed region fails closed. Missing either journal file also
fails closed, including interrupted initial file creation. There is no
automatic repair command.

Data beyond the published boundary is an unacknowledged tail and is discarded.
Interrupted boundary temporary files are ignored, never used as authority.
A truncated published boundary fails closed. A complete persisted commit can survive
even when its acknowledgement did not reach the caller. Retry admission using the
same request ID to resolve that uncertainty. A failed persistence barrier poisons
the writer, waking waiters and rejecting further operations until close/reopen.
Never edit or remove store files while the store is open.

The writer lock is held until close or process exit. Every writer open commits a
newer epoch before accepting writes. Every mutation checks that epoch. This is
local writer fencing, not a network lease or remote-worker fence. Filesystem replacement and
external mutation of a live store are unsupported.

## Durability and platform limits

- `memory`: no restart persistence.
- `process`: successful writes survive ordinary process termination through OS
  file writes. No power-loss guarantee or synchronization barrier is claimed.
- `strict`: default on supported Unix platforms. Commit data is synchronized
  before the boundary file is synchronized, replaced, and its directory
  synchronized. Store and parent directories are synchronized during open.
  macOS additionally uses `F_FULLFSYNC` for files.

Strict mode depends on a local filesystem and storage hardware honoring these
synchronization requests. Network filesystems, hardware failure, filesystem
corruption, and deletion of store files are outside the durability guarantee.
The tests exercise process termination and synthetic corruption, not physical
power loss.

Windows has exclusive writer locking and explicit process mode. Strict mode
currently returns an error there because directory creation durability has not
been validated. It never silently downgrades. Linux and Windows builds are checked,
but native lifecycle execution outside macOS remains a release gate. Other Unix
implementations are present but not platform-validated. Unsupported platforms
fail closed for writer acquisition.

## Read-only journal verification

`zot continuous verify --store <directory>` and `journal.Verify(ctx, path)` inspect
an existing journal without creating files, acquiring a new writer epoch,
truncating an unacknowledged tail, or repairing corruption. The existing writer
lock is required and held exclusively through the scan. Missing stores or
required files fail, rather than becoming fresh stores. Verification does not
run concurrently with a writer. Filesystem access timestamps may change.

The scan checks the published boundary, frame markers and lengths, header and
payload checksums, revision order, commit schema, nonzero/nondecreasing writer
epochs, valid JSON record values, and valid/nonduplicate operation keys. Recovery
uses the same validator and rejects invalid committed transactions before
truncation or writer acquisition. Unknown runtime record schemas are not checked.

A successful command emits JSON with `valid`, `scope` (always `journal`), `schema`
(the commit schema), `revision`, `epoch`, `committed_bytes`, and
`unacknowledged_tail_bytes`. Transcript content and record values are not printed.
The reported tail is outside committed authority, not a reason to silently repair
or acknowledge it. Errors produce no success report and do not change file content.

Verification reads one commit at a time, bounded by the 8 MiB encoded commit limit.
It does not rebuild a record index or retain history. Writer recovery now also
streams commits, but still retains the full current record map. Ordinary runtime
opens are not memory-bounded.

This is **journal verification**, not the specification's complete integrity or
health service. Tool-call pairing, conversation/submission references, ownership
acyclicity, artifact reachability, and hardware health remain unchecked.
Archive verification is separate and has the same journal-level semantic scope.
A valid journal is not proof of safe execution or recovery decisions.

## Admission-state integrity

`Runtime.CheckIntegrity(ctx)` checks one committed snapshot without mutating or
repairing it. Its report includes `valid`, `scope: "admission-state"`, `revision`,
and conversation, submission, and entry counts. Errors omit private record keys,
IDs, and content, and no success report is returned on failure or cancellation.

Checks cover conversation identities and revisions, contiguous queue and entry
sequences and their counters, unique entry IDs, submission/user-entry agreement,
queue backlinks, request-ID deduplication keys and payloads, root references, and
legacy import references and row ranges. Unknown record namespaces and unsupported
entry kinds fail closed. Storage accepts opaque JSON, so journal verification can
succeed while admission-state integrity fails.

`zot continuous check-state --store <directory>` exposes this check offline.
Unlike `verify`, it currently opens the ordinary writer: it acquires the writer
lock, advances the epoch and revision, and trims unpublished tails before checking
the snapshot. It requires an existing store. Strict durability is the default,
Windows requires `--durability process`. Use `verify` and `backup` first when
preserving the exact source bytes or revision matters. This command does not
repair runtime records, even when a reference check fails.

This is not complete runtime integrity. It checks current admission relationships,
not historical mutations, legacy tool pairing or export projection contents,
ownership, tasks, authorization, artifacts, or hardware health. It retains ID
sets proportional to current entries and submissions, and the ordinary writer
and snapshot still retain all current records in memory, including historical
transcript entries. No bounded-memory claim is made.
The check is not automatically applied on runtime startup or archive restore yet.

## Lossless journal backup and restore

```sh
zot continuous backup ./history.zotbackup --store ./continuous-store
zot continuous verify-backup ./history.zotbackup
zot continuous restore ./history.zotbackup --store ./restored-store
zot continuous verify --store ./restored-store
```

The Go API provides `journal.Backup(ctx, source, archive, opts)`,
`journal.VerifyBackup(ctx, archive)`, and
`journal.Restore(ctx, archive, newDirectory, opts)`. Options describe the
**destination's** durability. These operations bypass model requests and tools.
Their JSON reports contain verification metadata, not transcript content.

For an open store, `(*journal.Store).Backup(ctx, archive, opts)` captures the
committed prefix without stopping the writer, using the same archive format.
`(*sqlite.Store).Backup(ctx, destination)` copies a consistent committed database
snapshot with SQLite's online backup API. Its destination is a filesystem path,
not a SQLite URI. The parent must exist. The new file is reserved exclusively
with mode `0600` before copying, and an existing file or symlink is never
replaced. This Go API does not change the journal-only backup CLI. Interrupted
processes can leave incomplete destinations. External filesystem replacement
while a backup runs is unsupported.

Backup holds the source's existing writer lock through verification and copying.
It captures the complete committed journal prefix and its exact boundary, not a
session projection or a current-state-only dump. IDs, source rows, usage, prior
configuration revisions, deletion operations, queue admissions, and deduplication
history are preserved byte-for-byte. Unacknowledged tails and interrupted boundary
temporary files are excluded, without trimming or modifying the source. Backup
must be outside the source directory. It creates a new mode `0600` archive and
never overwrites an existing file, including a symlink.

The provisional version-one archive contains a four-byte `ZCB1` marker, the
20-byte journal boundary, exactly its committed data prefix, and a 32-byte SHA-256
digest covering marker, boundary, and data. Archive verification rejects extra
bytes, truncation, checksum mismatch, and invalid committed journal semantics.
It streams the digest and individual commits without loading the full archive.
A successful archive report uses `scope: "journal-backup"` and has no tail bytes.
`committed_bytes` describes journal data, not total archive size.

Restore validates the entire archive before creating any destination files. It
requires a nonexistent directory with an existing parent, creates it with mode
`0700`, and writes mode `0600` journal files while holding its new writer lock.
Data is written and, in strict mode, synchronized before publishing the boundary.
Strict publication also synchronizes the directory and parent. No existing store
is merged, replaced, or migrated. The restored revision and epoch exactly match
the archive. The first normal writer open subsequently advances both through
writer acquisition. Historical request-ID retries return the original submissions.

Reported write failures and cancellation attempt to remove only outputs created
by that operation. Cleanup failures are reported, never handled by recursive
removal of unexpected files. Abrupt termination can leave a partial archive or
restore directory. **Verify it before use**, especially before opening a restored
directory as a writer. A complete copy may survive without its acknowledgement.
Existing destinations are still never overwritten on retry, use a new path or
explicitly inspect the previous result. Destination publication is not an atomic
directory rename. Concurrent replacement or mutation of source/archive/destination
files outside these APIs is unsupported.

Archives contain private, unredacted history. They are **not encrypted or
cryptographically authenticated**. SHA-256 detects corruption, not a malicious
party who can rewrite the digest. Keep them under appropriate permissions or ACLs,
share them deliberately, and include them in sensitive-data deletion policies.
Do not run an original and restored copy as if they shared a writer fence, they
are independent stores. Restoring state is not proof that external effects can
be safely replayed.

This is a complete backup of the current prototype journal, not the future
runtime's full artifact-aware backup contract. Referenced blobs, external tool
files, credentials, application configuration outside the journal, migrations,
retention, and external-effect reconciliation are not captured. Legacy session
exports remain projections. The archive is the current lossless journal export.

## Persistence fault testing

The journal has a private fault-injection seam, disabled for production opens.
Tests discover checkpoints from the executed persistence path and inject failure
before and after each operation. Coverage includes directory and data-file
creation, initial boundary publication, recovery truncation, writer acquisition,
frame header and payload writes, file synchronization, boundary creation/write/
close/rename, directory synchronization, and the final device-cache flush.
Process mode and supported strict mode are tested separately.

Subprocess tests terminate without running cleanup at each checkpoint. Returned
I/O-error tests separately check error propagation, writer poisoning, observation
termination, and lock retention until close. Recovery must preserve earlier
acknowledged admissions and publish all admission records together or none.
An admission whose boundary was replaced before the failure survives an ordinary
process exit, even if no acknowledgement reached its caller. Retrying the same
request ID returns that admission rather than creating duplicate work.

Additional tests cut a real unacknowledged frame at every byte, including a
complete unpublished frame, and check that neither insertions nor deletions
change committed state. Every truncation of an acknowledged test journal or its
boundary must fail closed. Interrupted first creation with a data file but no
boundary also fails closed, rather than silently reinitializing authority.

These tests are not physical power-loss tests and do not model every filesystem
or hardware failure. Backup/restore tests additionally inject returned errors and
process termination at file creation, copy, checksum, synchronization, close,
and publication edges. Partial destinations must fail verification, complete
copies must match the recorded revision, and source authority must remain unchanged.
Archive tests reject every byte truncation of a synthetic archive.
Model generation, tool effects, and migration crash boundaries remain unimplemented.
Native non-macOS lifecycle runs remain release gates. Run the current persistence
matrix directly with:

```sh
go test ./packages/continuous/storage/journal -run 'TestPersistence|TestEvery|TestBackup|TestArchive|TestRestore' -count=1
```

## SQLite backend

`sqlite.Open(ctx, dir, sqlite.Options{Durability})` stores everything in
`<dir>/continuous.sqlite` with the same `writer.lock` fencing as the journal.
Records are versioned rows keyed by `(key, revision)`; a snapshot at revision
R reads the newest version at or below R, so snapshots are stable without
copying and history is the same table ordered by revision. Strict durability
uses WAL with `synchronous=FULL` and `fullfsync`; process durability uses
`synchronous=NORMAL`. Open reads counters only and verifies them against the
newest commit row; a database edited outside the API fails closed. The
driver is pure Go (SQLite compiled to WebAssembly on wazero); it adds no
system library or C toolchain requirement and roughly 2 MB to the binary.
`backup`, `verify-backup`, `restore`, `verify`, `format`, and `migrate` are
journal-only; copy `continuous.sqlite` while no writer holds the lock to back
up a SQLite store. Both backends pass the conformance suite and the execution
crash matrices.

The CLI picks the backend of an existing store from its files; `--backend` is
accepted only where a store may be created (`run`, `serve`, `import`) and
must match an existing store.

## Remaining work and release gates

Memory storage retains both records and commit history and is for tests and
ephemeral use. Pagination bounds response sizes. The journal's record index
holds one node per live key in memory (key bytes plus 12 bytes); the SQLite
backend holds no record state in memory. Neither has been measured against
the specification's million-entry targets on representative hardware, so
those remain release gates rather than claims.

Not implemented from the specification:

- Large-session import/export streaming (imports are bounded to 4 MiB),
  artifact-aware backup, retention compaction of the log (sealed segments
  are never rewritten, so `retain` frees no bytes).
- Swarm agents of a non-attached TUI still run as child processes with their
  own session files; only `--continuous` sessions run them on the host.
  Removing a hosted swarm agent does not delete its host conversation.
- Physical erasure of retained journal bytes (would be a segment rewrite).
  The search index is memory only and rebuilt at host start.
- Only schema 1 exists; `migrate` has no older schema to convert yet. It is
  the tested path for the first format change.
- Background summaries are durable task results; one runs per conversation
  at a time.
- The remote worker is a reference implementation of the contract: one
  process, a JSONL operation ledger, no scheduling across workers.
- Permission lists select tools by name; argument-level policies belong to
  the host guard or an `Approver`.
- Tools run sequentially. Recovery cannot guarantee exactly-once external
  effects without receiver-enforced operation keys or successful
  reconciliation.
- Native non-macOS lifecycle runs remain release gates. Linux and Windows
  builds and vet are checked; the named pipe transport is exercised by the
  endpoint tests only when they run on Windows.

Legacy JSONL files are read only during explicit import. After import, the
committed continuous entries are authoritative. Original JSONL files and exports
are not consulted for recovery. Execution continuity means a run resumes at
its last committed boundary. It does not mean exactly-once external effects:
an interrupted unsafe tool may or may not have acted, and the runtime says so
instead of guessing.

## Examples

`examples/continuous/` contains runnable examples: Go programs with a
synthetic provider (no credentials, no paid calls) and shell scripts for the
CLI flows. They cover a persistent local conversation, crash recovery with an
interrupted tool, a read-only reviewer subagent, concurrent conversations with
a fork, a reminder surviving restart, a task tree with retries and
compensation, persisted approval, a transactional todo document with an
`as_of` fork, compaction and handoff, extension reload through the host, an
idempotent tool with reconciliation, two clients on one conversation, an
integration bot with stable request IDs, and backup, verification, restore.

## Benchmarks

`go test ./packages/continuous -run '^$' -bench . -benchmem` runs the
workload benchmarks: one submission round trip through the service on memory
and process-durability journal stores, model context construction for 1000
and 4000 entry conversations, and host metrics with 100 settled conversations
(reporting goroutines and heap). Together with
`packages/continuous/storage/journal` `BenchmarkCommit` they are the inputs to
a published measurement; report hardware, OS, backend, durability mode, and
Go version alongside the numbers. Context construction is linear in entries.
Idle waiting conversations hold no goroutines. These are aggregate Go
benchmark measurements, not percentile acceptance results.

## Validation

```sh
go test ./packages/continuous/...
go test -race ./packages/continuous/...
go test -race ./packages/agent -run TestContinuous
go test ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./packages/continuous/... ./packages/agent
GOOS=windows GOARCH=amd64 go build ./packages/continuous/... ./packages/agent
go test ./packages/continuous/storage/journal -run '^$' -bench BenchmarkCommit -benchmem
```

The benchmark reports Go's aggregate latency and allocation measurements. It is
not the specification's complete percentile benchmark methodology or a published
performance acceptance result.
