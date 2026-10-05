package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
)

const continuousHelp = `zot continuous - experimental persistent execution and store management

Usage:
  zot continuous run "<prompt>" --store <directory> [--backend journal|sqlite] [--request-id <id>] [--workspace <id>] [--json] [zot flags]
  zot continuous run --resume --store <directory> [zot flags]
  zot continuous serve --store <directory> [--backend journal|sqlite] [--socket <path>] [--token-file <file>] [--recover safe|all|none] [zot flags]
  zot continuous serve --store <directory> --listen <host:port> --token-file <file> [--tls-cert <pem> --tls-key <pem> [--tls-client-ca <pem>]]
  zot continuous attach "<prompt>" --socket <path>|--address <host:port> [--tls-ca <pem>] --workspace <id> [--token-file <file>] [--follow] [--json]
  zot continuous import <session-path> --store <directory> [--backend journal|sqlite]
  zot continuous export <conversation-id> --store <directory> --format session [--output <new-file>]
  zot continuous status --store <directory>
  zot continuous verify --store <directory>
  zot continuous check-state --store <directory>
  zot continuous backup <new-archive-file> --store <directory>
  zot continuous verify-backup <archive-file>
  zot continuous restore <archive-file> --store <new-directory>
  zot continuous format --store <directory>
  zot continuous migrate <new-directory> --store <directory>
  zot continuous conversations --store <directory> [--limit <1-1000>] [--after <id>]
  zot continuous recover --dry-run --store <directory>
  zot continuous abort <conversation-id|task-id> --store <directory> [--include-background]
  zot continuous usage <conversation-id> --store <directory>
  zot continuous tasks <conversation-id> --store <directory>
  zot continuous inspect <task-id|approval-id|submission-id|conversation-id> --store <directory>
  zot continuous approvals <conversation-id> --store <directory>
  zot continuous decide <approval-id> --allow|--deny [--scope call|tool] [--reason <text>] --store <directory>
  zot continuous budget <conversation-id|runtime> [--limit-usd <n>] [--limit-tokens <n>] [--clear] --store <directory>
  zot continuous search <conversation-id> [--text <s>] [--type <entry-type>]... [--limit <n>] --store <directory>
  zot continuous prompts <conversation-id> --store <directory>
  zot continuous withdraw <submission-id> --store <directory>
  zot continuous reorder <submission-id> --store <directory>
  zot continuous outbox --store <directory>
  zot continuous retain [--dedup 720h] [--approvals 720h] [--partials 24h] [--memos 720h] [--document-history 50] [--dry-run] --store <directory>
  zot --continuous <socket|host:port> [--continuous-workspace <id>] [--continuous-token-file <file>]

Options:
  --durability strict|process   Writes only. Default strict, Windows requires process (journal).
  --backend journal|sqlite      New stores only (run, serve, import). Default journal. Existing
                               stores open with the backend that created them.
  --help                       Show this help.

run executes queued work for the workspace root conversation with the host's ordinary
provider, tool, and extension configuration (--provider, --model, --tools, --cwd, --ext ...).
Every model request and tool call is committed before and after it runs. A crash leaves a
recoverable run: --resume continues it. Interrupted tools are reported to the model as
errors and never repeated unless the tool declares replay safe and policy still allows it.
serve hosts the store independently of clients: it recovers interrupted runs by policy
(safe resumes only automatic actions, all also reports interrupted unsafe tools, none waits),
executes queued work for every conversation, and serves a newline-JSON protocol on a mode
0600 Unix socket (default <store>/host.sock; on Windows a private named pipe derived from
that path, accepted by --socket and --continuous). --listen needs --token-file; loopback may be
plaintext, any other address requires --tls-cert and --tls-key (TLS 1.3), optionally
--tls-client-ca for mutual TLS. Tokens map to roles read, submit, approve, admin; without a
token file every local socket connection is admin. SIGHUP (or runtime.reload) reloads
extensions into a new engine generation without stopping the host.
attach connects to a host, submits to the workspace root conversation, prints the answer, and
with --follow streams committed entries. Closing attach never cancels host work.
zot --continuous <address> runs the interactive TUI attached to a host: prompts are
submitted to the host, committed entries are rendered, and closing the TUI detaches.
recover --dry-run lists what the next run would do for each interrupted conversation.
abort records abort intent on a run (or a task tree); the next step settles it without new
effects. Background tasks are reached only with --include-background.
usage sums the ledger of one conversation, reporting attempts with unknown cost separately.
tasks, inspect, approvals list committed task, approval, submission, and conversation state.
decide records a human approval decision; the parked run continues on the next step.
budget sets or shows conservative spending limits; unknown-cost attempts count as reserved.
search scans full history including entries before resets and inherited fork ancestry.
prompts lists the system prompt and tool schema hashes each attempt was generated against.
withdraw removes a queued submission (history kept, settles withdrawn); reorder moves a queued
submission to the front of its conversation's queue. outbox lists undelivered notifications
(pending approvals, blocked recovery, blocked cleanup).
retain garbage-collects auxiliary records (deduplication, decided approvals, retained
partial output, settled memos, old document versions) by age; it never removes history,
runs, usage, or tasks, and journal bytes stay until the journal is rewritten.
Inspection and decision commands execute nothing.
Source journal commands take the exclusive writer lock, archive verification does not.
Import/export/status/listings/check-state advance the epoch. Backup and verify leave the source unchanged.
Restore keeps the archived epoch, the first writer open advances it.
Verify checks journal framing, checksums, commit schema, operations, revisions and epochs.
It reports unacknowledged tail bytes without repair, not runtime-level integrity or hardware health.
Check-state validates current admission references, queue/entry counters and deduplication.
It opens a writer (advancing the epoch and discarding unpublished tails), not a read-only scan.
It does not validate historical tool pairing, tasks, ownership or artifacts, or repair records.
Verify and verify-backup are read-only and do not take --durability, including on Windows.
Backup is a lossless committed-journal archive, not encrypted and not an external-artifact backup.
Backup files and restore directories must not exist. Their parents must exist.
Interrupted backup/restore can leave partial destinations, verify them before using them.
Format reports the journal's commit schema read-only and whether this build can open or
migrate it. Migrate rewrites a store into a new directory at the current schema, verifying
every converted commit; the source is locked and unchanged. Take a backup first. The new
directory keeps the archived epoch and revision. Interrupted migrations leave partial
destinations that fail verification, remove them and retry.
Existing stores only, except import and restore create stores with an existing parent.
The sqlite backend (pure Go, SQLite on the embedded WebAssembly runtime) keeps records and
history in one database with bounded memory; backup, restore, verify, format and migrate are
journal-only (use sqlite's own file copy of continuous.sqlite while no writer is open).
Status and conversation listings are JSON. Export defaults to JSONL on stdout.
Imports are atomic and bounded to 4 MiB and 10000 rows, subject to the commit limit.
Missing legacy tool results become interrupted error records, never successes.
Legacy exports are projections, not complete store backups. Existing output files are never overwritten.
`

func runContinuousCommand(rawArgs []string) (bool, error) {
	if len(rawArgs) == 0 || rawArgs[0] != "continuous" {
		return false, nil
	}
	if len(rawArgs) > 1 {
		switch rawArgs[1] {
		case "run":
			ctx, stop := signalContext()
			defer stop()
			return true, runContinuousRun(ctx, rawArgs[2:], os.Stdout)
		case "serve":
			ctx, stop := signalContext()
			defer stop()
			return true, runContinuousServe(ctx, rawArgs[2:], os.Stdout)
		case "attach":
			ctx, stop := signalContext()
			defer stop()
			return true, runContinuousAttach(ctx, rawArgs[2:], os.Stdout)
		}
	}
	return true, runContinuous(context.Background(), rawArgs[1:], os.Stdout)
}

func continuousDurability(name string) storage.Durability {
	if name == "process" {
		return storage.Process
	}
	return storage.Strict
}

type continuousOptions struct {
	command    string
	store      string
	backend    string
	durability storage.Durability
	format     string
	output     string
	limit      int
	after      string
	dryRun     bool
	positional []string
	help       bool

	decision          string
	scope             string
	reason            string
	limitUSD          float64
	limitTokens       int
	clear             bool
	text              string
	types             []string
	includeBackground bool
	retention         continuous.RetentionPolicy
}

func parseContinuousOptions(args []string) (continuousOptions, error) {
	opts := continuousOptions{limit: 100, durability: storage.Strict, format: "session"}
	if len(args) == 0 {
		opts.help = true
		return opts, nil
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		opts.help = true
		return opts, nil
	}
	opts.command = args[0]
	switch opts.command {
	case "import", "export", "status", "conversations", "verify", "check-state", "backup", "verify-backup", "restore", "recover", "abort", "usage", "tasks", "inspect", "approvals", "decide", "budget", "search", "retain", "prompts", "withdraw", "reorder", "outbox", "format", "migrate":
	default:
		return opts, fmt.Errorf("unsupported continuous command (use continuous --help)")
	}
	positionalOnly := false
	seen := make(map[string]bool)
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if positionalOnly {
			opts.positional = append(opts.positional, arg)
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		if arg == "--help" || arg == "-h" {
			opts.help = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			opts.positional = append(opts.positional, arg)
			continue
		}
		if seen[arg] {
			return opts, fmt.Errorf("duplicate continuous flag: %s", arg)
		}
		seen[arg] = true
		switch arg {
		case "--store":
			if opts.command == "verify-backup" {
				return opts, fmt.Errorf("--store does not apply to verify-backup")
			}
		case "--backend":
			if opts.command != "import" {
				return opts, fmt.Errorf("--backend applies to import; existing stores open with their own backend")
			}
		case "--durability":
			if opts.command == "verify" || opts.command == "verify-backup" || opts.command == "format" {
				return opts, fmt.Errorf("--durability does not apply to read-only verification")
			}
		case "--format", "--output":
			if opts.command != "export" {
				return opts, fmt.Errorf("%s only applies to export", arg)
			}
		case "--limit", "--after":
			if opts.command != "conversations" && opts.command != "search" {
				return opts, fmt.Errorf("%s only applies to conversations and search", arg)
			}
		case "--allow", "--deny":
			if opts.command != "decide" {
				return opts, fmt.Errorf("%s only applies to decide", arg)
			}
			opts.decision = arg
			continue
		case "--scope", "--reason":
			if opts.command != "decide" {
				return opts, fmt.Errorf("%s only applies to decide", arg)
			}
		case "--limit-usd", "--limit-tokens", "--clear":
			if opts.command != "budget" {
				return opts, fmt.Errorf("%s only applies to budget", arg)
			}
			if arg == "--clear" {
				opts.clear = true
				continue
			}
		case "--text", "--type":
			if opts.command != "search" {
				return opts, fmt.Errorf("%s only applies to search", arg)
			}
		case "--dedup", "--approvals", "--partials", "--memos", "--document-history":
			if opts.command != "retain" {
				return opts, fmt.Errorf("%s only applies to retain", arg)
			}
		case "--include-background":
			if opts.command != "abort" {
				return opts, fmt.Errorf("--include-background only applies to abort")
			}
			opts.includeBackground = true
			continue
		case "--dry-run":
			if opts.command != "recover" && opts.command != "retain" {
				return opts, fmt.Errorf("--dry-run only applies to recover and retain")
			}
			opts.dryRun = true
			continue
		default:
			return opts, fmt.Errorf("unknown continuous flag: %s", arg)
		}
		if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "--") {
			return opts, fmt.Errorf("%s requires a value", arg)
		}
		i++
		value := args[i]
		switch arg {
		case "--store":
			opts.store = value
		case "--durability":
			opts.durability = storage.Durability(value)
			if opts.durability != storage.Strict && opts.durability != storage.Process {
				return opts, fmt.Errorf("durability must be strict or process")
			}
		case "--backend":
			if !validContinuousBackend(value) {
				return opts, fmt.Errorf("backend must be journal or sqlite")
			}
			opts.backend = value
		case "--format":
			opts.format = value
			if value != "session" {
				return opts, fmt.Errorf("only session export format is implemented")
			}
		case "--output":
			opts.output = value
		case "--after":
			opts.after = value
		case "--limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 1000 {
				return opts, fmt.Errorf("limit must be between 1 and 1000")
			}
			opts.limit = limit
		case "--scope":
			opts.scope = value
		case "--reason":
			opts.reason = value
		case "--limit-usd":
			f, err := strconv.ParseFloat(value, 64)
			if err != nil || f < 0 {
				return opts, fmt.Errorf("--limit-usd must be a non-negative number")
			}
			opts.limitUSD = f
		case "--limit-tokens":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return opts, fmt.Errorf("--limit-tokens must be a non-negative integer")
			}
			opts.limitTokens = n
		case "--dedup", "--approvals", "--partials", "--memos":
			d, err := time.ParseDuration(value)
			if err != nil || d <= 0 {
				return opts, fmt.Errorf("%s must be a positive duration such as 720h", arg)
			}
			switch arg {
			case "--dedup":
				opts.retention.Dedup = d
			case "--approvals":
				opts.retention.Approvals = d
			case "--partials":
				opts.retention.Partials = d
			case "--memos":
				opts.retention.Memos = d
			}
		case "--document-history":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return opts, fmt.Errorf("--document-history must be a positive integer")
			}
			opts.retention.DocumentHistory = n
		case "--text":
			opts.text = value
		case "--type":
			opts.types = append(opts.types, value)
		}
	}
	if opts.help {
		return opts, nil
	}
	if opts.store == "" && opts.command != "verify-backup" {
		return opts, fmt.Errorf("continuous requires an explicit --store directory")
	}
	expected := 0
	switch opts.command {
	case "import", "export", "backup", "restore", "verify-backup", "abort", "usage", "tasks", "inspect", "approvals", "decide", "budget", "search", "prompts", "withdraw", "reorder", "migrate":
		expected = 1
	}
	if opts.command == "retain" && opts.retention == (continuous.RetentionPolicy{}) {
		return opts, fmt.Errorf("retain requires at least one of --dedup, --approvals, --partials, --memos, --document-history")
	}
	if opts.command == "decide" && opts.decision == "" {
		return opts, fmt.Errorf("decide requires --allow or --deny")
	}
	if opts.command == "recover" && !opts.dryRun {
		return opts, fmt.Errorf("recover currently requires --dry-run; use `continuous run --resume` to apply recovery")
	}
	if len(opts.positional) != expected {
		return opts, fmt.Errorf("continuous %s requires %d positional arguments", opts.command, expected)
	}
	return opts, nil
}

// runContinuous bypasses model resolution, credential lookup, and extension
// loading. These commands are trusted-local offline operations, not a host API.
func runContinuous(ctx context.Context, args []string, out io.Writer) (retErr error) {
	opts, err := parseContinuousOptions(args)
	if err != nil {
		return err
	}
	if opts.help {
		_, err := io.WriteString(out, continuousHelp)
		return err
	}
	if opts.command == "verify" {
		report, err := journal.Verify(ctx, opts.store)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(report)
	}
	if opts.command == "format" {
		format, err := journal.InspectFormat(ctx, opts.store)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(format)
	}
	if opts.command == "migrate" {
		report, err := journal.Migrate(ctx, opts.store, opts.positional[0], journal.Options{Durability: opts.durability})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(report)
	}
	if opts.command == "backup" || opts.command == "restore" || opts.command == "verify-backup" {
		var report journal.Verification
		switch opts.command {
		case "backup":
			report, err = journal.Backup(ctx, opts.store, opts.positional[0], journal.Options{Durability: opts.durability})
		case "restore":
			report, err = journal.Restore(ctx, opts.positional[0], opts.store, journal.Options{Durability: opts.durability})
		case "verify-backup":
			report, err = journal.VerifyBackup(ctx, opts.positional[0])
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(report)
	}
	var source *os.File
	if opts.command == "import" {
		source, err = os.Open(opts.positional[0])
		if err != nil {
			return fmt.Errorf("open import source: %w", err)
		}
		defer source.Close()
	} else {
		info, err := os.Stat(opts.store)
		if err != nil {
			return fmt.Errorf("open existing continuous store: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("continuous store must be a directory")
		}
		// An empty directory is not an existing store. Avoid creating authority as
		// a side effect of an inspection or export command.
		if existing, err := detectContinuousBackend(opts.store); err != nil {
			return err
		} else if existing == "" {
			return fmt.Errorf("open existing continuous store: %s has no journal boundary or sqlite database", opts.store)
		}
	}
	store, err := openContinuousStore(ctx, opts.store, opts.backend, opts.durability)
	if err != nil {
		return err
	}
	r, err := continuous.New(store)
	if err != nil {
		store.Close()
		return err
	}
	defer func() { retErr = errors.Join(retErr, r.Close()) }()
	enc := json.NewEncoder(out)
	switch opts.command {
	case "import":
		c, err := r.ImportSession(ctx, source)
		if err != nil {
			return err
		}
		info, err := r.SessionImport(ctx, c.ID)
		if err != nil {
			return err
		}
		return enc.Encode(struct {
			Conversation continuous.Conversation  `json:"conversation"`
			Source       continuous.SessionImport `json:"source"`
		}{c, info})
	case "export":
		id := opts.positional[0]
		if opts.output == "" {
			return r.ExportSession(ctx, id, out)
		}
		if _, err := r.Conversation(ctx, id); err != nil {
			return err
		}
		dst, err := os.OpenFile(opts.output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create new export file: %w", err)
		}
		exportErr := r.ExportSession(ctx, id, dst)
		if exportErr == nil {
			exportErr = dst.Sync()
		}
		exportErr = errors.Join(exportErr, dst.Close())
		if exportErr != nil {
			os.Remove(opts.output)
			return exportErr
		}
		return nil
	case "check-state":
		report, err := r.CheckIntegrity(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(report)
	case "recover":
		plan, err := r.RecoveryPreview(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(plan)
	case "abort":
		if _, err := r.Task(ctx, opts.positional[0]); err == nil {
			// A task ID: abort the task tree.
			if err := r.AbortTask(ctx, opts.positional[0], opts.includeBackground); err != nil {
				return err
			}
			t, err := r.Task(ctx, opts.positional[0])
			if err != nil {
				return err
			}
			return enc.Encode(t)
		}
		run, err := r.Abort(ctx, opts.positional[0])
		if err != nil {
			return err
		}
		return enc.Encode(run)
	case "tasks":
		if _, err := r.Conversation(ctx, opts.positional[0]); err != nil {
			return err
		}
		tasks, err := r.Tasks(ctx, opts.positional[0])
		if err != nil {
			return err
		}
		if tasks == nil {
			tasks = []continuous.Task{}
		}
		return enc.Encode(tasks)
	case "inspect":
		if t, err := r.Task(ctx, opts.positional[0]); err == nil {
			return enc.Encode(t)
		}
		if a, err := r.Approval(ctx, opts.positional[0]); err == nil {
			return enc.Encode(a)
		}
		if s, err := r.Submission(ctx, opts.positional[0]); err == nil {
			return enc.Encode(s)
		}
		snap, err := r.ConversationSnapshot(ctx, opts.positional[0], 50)
		if err != nil {
			return err
		}
		return enc.Encode(snap)
	case "approvals":
		if _, err := r.Conversation(ctx, opts.positional[0]); err != nil {
			return err
		}
		list, err := r.PendingApprovals(ctx, opts.positional[0])
		if err != nil {
			return err
		}
		if list == nil {
			list = []continuous.Approval{}
		}
		return enc.Encode(list)
	case "decide":
		a, err := r.Decide(ctx, opts.positional[0], "cli:"+userActor(), opts.decision == "--allow", opts.scope, opts.reason)
		if err != nil {
			return err
		}
		return enc.Encode(a)
	case "budget":
		id := opts.positional[0]
		scope := "conversation"
		if id == "runtime" {
			scope, id = "runtime", ""
		}
		if opts.clear || opts.limitUSD > 0 || opts.limitTokens > 0 {
			current, _, err := r.Budget(ctx, scope, id)
			if err != nil {
				return err
			}
			b := continuous.Budget{Scope: scope, ConversationID: id}
			if !opts.clear {
				b.LimitUSD, b.LimitTokens = opts.limitUSD, opts.limitTokens
			}
			if _, err := r.SetBudget(ctx, b, current.Revision); err != nil {
				return err
			}
		}
		statuses, err := r.CheckBudgets(ctx, id)
		exceeded := errors.Is(err, continuous.ErrBudgetExceeded)
		if err != nil && !exceeded {
			return err
		}
		if statuses == nil {
			statuses = []continuous.BudgetStatus{}
		}
		return enc.Encode(struct {
			Budgets  []continuous.BudgetStatus `json:"budgets"`
			Exceeded bool                      `json:"exceeded"`
		}{statuses, exceeded})
	case "withdraw":
		s, err := r.Withdraw(ctx, opts.positional[0], "cli:"+userActor())
		if err != nil {
			return err
		}
		return enc.Encode(s)
	case "reorder":
		queue, err := r.Reorder(ctx, opts.positional[0], "cli:"+userActor())
		if err != nil {
			return err
		}
		return enc.Encode(queue)
	case "outbox":
		list, err := r.Outbox(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(list)
	case "retain":
		report, err := r.Retain(ctx, opts.retention, opts.dryRun)
		if err != nil {
			return err
		}
		return enc.Encode(report)
	case "prompts":
		records, err := r.PromptRecords(ctx, opts.positional[0], opts.limit)
		if err != nil {
			return err
		}
		return enc.Encode(records)
	case "search":
		result, err := r.Search(ctx, continuous.SearchQuery{ConversationID: opts.positional[0], Text: opts.text, Types: opts.types, Limit: opts.limit, Ancestry: true})
		if err != nil {
			return err
		}
		return enc.Encode(result)
	case "usage":
		if _, err := r.Conversation(ctx, opts.positional[0]); err != nil {
			return err
		}
		totals, err := r.Usage(ctx, opts.positional[0])
		if err != nil {
			return err
		}
		return enc.Encode(totals)
	case "status":
		status, err := r.Status(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(status)
	case "conversations":
		conversations, revision, err := r.Conversations(ctx, opts.after, opts.limit)
		if err != nil {
			return err
		}
		after := ""
		if len(conversations) > 0 {
			after = conversations[len(conversations)-1].ID
		}
		return enc.Encode(struct {
			Revision      uint64                    `json:"revision"`
			Conversations []continuous.Conversation `json:"conversations"`
			After         string                    `json:"after"`
		}{revision, conversations, after})
	}
	return fmt.Errorf("unsupported continuous command")
}
