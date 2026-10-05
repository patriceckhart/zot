# zot continuous examples

Runnable examples for the persistent, recoverable runtime. Go programs use a
synthetic provider so no credentials or paid calls are needed; shell scripts
use the `zot` binary against your configured provider.

| Example | Shows |
|---|---|
| `01-local-conversation.sh` | A persistent local coding conversation across invocations |
| `02-crash-recovery.sh` | Safe recovery after an interrupted tool: recovery plan, resume |
| `03-reviewer-subagent/` | A read-only reviewer subagent with its own tool set |
| `04-threads-and-fork/` | Concurrent conversations and a fork sharing history |
| `05-reminder/` | A background timer task surviving a restart |
| `06-task-dependencies/` | A custom task with dependencies, retries, and cleanup |
| `07-approval/` | Persisted human approval across a restart |
| `08-todo-document/` | A transactional todo document and a historical fork |
| `09-compaction-handoff.sh` | Compaction and the `handoff` tool |
| `10-extension-reload.sh` | Reloading extensions in a running host with SIGHUP |
| `11-idempotent-tool/` | An idempotent external action with a stable operation key |
| `15-remote-worker/` | Remote tool execution and reconciliation after a lost connection |
| `12-two-clients.sh` | Two clients steering and observing one conversation |
| `13-integration-bot/` | A bot using stable submission IDs over the host protocol |
| `14-backup-restore.sh` | Backup, verification, and restore |

Run a Go example with `go run ./examples/continuous/<dir>`. Each creates its
store under a temporary directory and prints what it did. Shell scripts take
the store directory as their first argument.
