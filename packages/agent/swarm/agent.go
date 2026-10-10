package swarm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// Agent is one supervised task. Public fields are immutable after
// Spawn; mutable state (status, activity, transcript) lives behind
// the embedded mutex.
type Agent struct {
	ID   string
	Task string
	// Images is the initial task's copied payload. Resume uses the persisted
	// transcript rather than resending this input.
	Images  []provider.ImageBlock
	Dir     string // always the host's RepoRoot; agents share its cwd.
	Started time.Time

	// Model and Provider, when non-empty, override the child
	// subprocess's model resolution. Empty means "inherit whatever
	// the child resolves on its own from config / env / flags" —
	// the historical behaviour. Persisted in meta.json so Resume
	// keeps using the same model across zot restarts.
	Model    string
	Provider string

	// SessionID, when non-empty, scopes the agent to a particular
	// host zot session: the dashboard only surfaces agents whose
	// SessionID matches the active session. Empty means "unscoped"
	// (legacy meta files from before the field existed, or agents
	// spawned without a session context such as in tests). Set at
	// Spawn time from Swarm.activeSession and persisted in
	// meta.json so the scope survives restarts.
	SessionID string

	// Resuming is true when this Agent struct was built by Resume
	// rather than Spawn. The runner consults it to decide whether
	// to pass the original Task as a positional argv to the child:
	// on first Spawn we want the child to run the task immediately;
	// on Resume the task was already run last time and replaying it
	// would produce a second turn that collides ("agent busy; send
	// 'cancel' first") with whatever the user types next. Not
	// persisted — every Resume sets it explicitly.
	Resuming bool

	// InboxPath is the unix socket the child agent listens on for
	// follow-up prompts and control messages. The supervisor opens
	// an Inbox at this path; the child opens a Listener.
	InboxPath string

	// EventLogPath is the durable JSONL event log for this agent.
	// The runner appends every well-formed event from the child
	// (plus lifecycle events of its own) here. /swarm open in any
	// zot process reads from this file to replay the full history.
	EventLogPath string

	// SessionPath is the child's persistent session file. Surfaced
	// so the dashboard / /swarm open can resume the agent later.
	SessionPath string

	// inbox is the supervisor-side socket handle. Populated by
	// Swarm.Spawn so SendInput can route messages without each
	// caller redialing.
	inbox *Inbox

	mu            sync.Mutex
	status        Status
	activity      string
	transcript    []string
	lastAssistant string
	// Large detached logs are loaded in full only when their transcript is viewed or resumed.
	needsFullReplay bool
	finished        time.Time
	lastErr         error

	// OnTurnEnd, if set, fires once per prompt-level turn_end event
	// emitted by the swarm daemon wrapper. Provider/tool-loop
	// turn_end events (for example stop=tool_use) are ignored by the
	// runner because they do not mean the sub-agent task finished.
	// Used by auto-swarm watchers to detect that a sub-agent's first
	// (or n-th) task has finished without waiting for the long-lived
	// daemon itself to exit — sub-agents keep running on the inbox
	// even after the initial task completes, so Wait() never unblocks
	// for them.
	OnTurnEnd func(step int, errMsg string)

	ctx    context.Context
	cancel context.CancelFunc
	runner Runner

	// done closes when the run goroutine finalises the agent's
	// status (done / failed / killed). Wait blocks on this so
	// callers don't have to poll.
	done chan struct{}
}

// Inbox exposes the supervisor-side socket handle. Returns nil for
// agents without inboxes (e.g. tests using a custom Runner that
// doesn't speak the daemon protocol).
func (a *Agent) Inbox() *Inbox { return a.inbox }

// Status returns the current high-level status. Cheap; safe from any goroutine.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// Activity returns the current one-line activity string.
func (a *Agent) Activity() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activity
}

// Transcript returns a copy of the running transcript.
func (a *Agent) Transcript() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.transcript))
	copy(out, a.transcript)
	return out
}

// Err returns the runner's terminal error, if any.
func (a *Agent) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// Wait blocks until the agent reaches a terminal state. Used by tests
// and by /swarm wait <id>.
func (a *Agent) Wait() { <-a.done }

// SetOnTurnEnd installs (or clears, with nil) the callback fired
// from the runner when the child daemon emits a prompt-level
// turn_end event with a step field. Safe to call from any goroutine:
// the runner reads the callback under the same mutex.
func (a *Agent) SetOnTurnEnd(fn func(step int, errMsg string)) {
	a.mu.Lock()
	a.OnTurnEnd = fn
	a.mu.Unlock()
}

func (a *Agent) setStatus(s Status) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
}

func (a *Agent) setActivity(msg string) {
	a.mu.Lock()
	a.activity = strings.TrimSpace(msg)
	a.mu.Unlock()
}

func (a *Agent) appendTranscript(chunk string) {
	a.appendTranscriptLocked(chunk, "", false)
}

func (a *Agent) appendUserMessage(text string) {
	a.appendTranscriptLocked(text, "user: ", false)
}

func (a *Agent) appendAssistantMessage(text string) {
	a.appendTranscriptLocked(text, "", true)
}

func (a *Agent) appendTranscriptLocked(chunk, linePrefix string, assistant bool) {
	message := chunk
	chunk = strings.TrimRight(chunk, "\n")
	if chunk == "" {
		return
	}
	a.mu.Lock()
	for _, line := range strings.Split(chunk, "\n") {
		a.transcript = append(a.transcript, linePrefix+line)
	}
	if assistant {
		a.lastAssistant = message
	}
	// Bound the transcript so dashboards don't hold gigabytes for
	// long-running agents. 2000 lines is plenty for inspection;
	// the durable record lives in the agent's session file.
	const cap = 2000
	if len(a.transcript) > cap {
		a.transcript = a.transcript[len(a.transcript)-cap:]
	}
	a.mu.Unlock()
}

// newAgentID returns a short, mostly-collision-free identifier of the
// form "<slug>-<nano>". The slug is derived from the task text so
// dashboards stay readable; the nano suffix guarantees uniqueness
// even when two agents are spawned in the same millisecond.
func newAgentID(task string, now time.Time) string {
	slug := taskSlug(task)
	return fmt.Sprintf("%s-%d", slug, now.UnixNano()%1_000_000)
}

func taskSlug(task string) string {
	task = strings.ToLower(task)
	var b strings.Builder
	dash := false
	for _, r := range task {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= 24 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "agent"
	}
	return out
}
