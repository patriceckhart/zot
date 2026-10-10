// Package swarm implements zot's multi-agent supervisor.
//
// A Swarm manages a set of headless zot subprocesses ("agents")
// that share the host's working directory. The interactive TUI
// exposes the supervisor through the /swarm slash command and a
// dashboard dialog; non-TUI code can drive it directly through
// this package.
//
// Every agent runs with cwd == the parent zot's RepoRoot — the
// same files the user sees, the same files the main agent edits.
// There is no git worktree, no per-agent branch, no isolation. If
// you want parallel edits on a separate branch, use normal git
// tooling (a real worktree, a different terminal) yourself.
//
// Each Agent has:
//   - a unique id (short slug + nanoseconds)
//   - a Runner (the thing that actually executes the task)
//   - a Status string + Activity string that the dashboard reads
//
// The Runner abstraction means tests can swap a fake in instead of
// really spawning a subprocess; the production Runner shells out to
// `zot --swarm-agent ...` so we reuse zot's own model resolution
// and tooling without re-implementing the agent loop.
package swarm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// Status is the high-level lifecycle state of an Agent.
type Status string

const (
	StatusPending  Status = "pending"  // created, not started yet
	StatusRunning  Status = "running"  // Runner.Run is in flight
	StatusDone     Status = "done"     // Runner.Run returned nil
	StatusFailed   Status = "failed"   // Runner.Run returned an error
	StatusKilled   Status = "killed"   // Stop() called before completion
	StatusDetached Status = "detached" // reloaded from disk; no live runner
)

// LifecycleEvent reports a locally supervised task, not a reloaded detached task.
type LifecycleEvent struct {
	Event, SessionID, AgentID, AgentRunID, Name, CWD, Status, Error string
}

// Config configures a Swarm.
type Config struct {
	// OnLifecycle observes start and terminal outcomes outside supervisor locks.
	// It must return promptly. Different agents may invoke it concurrently.
	OnLifecycle func(LifecycleEvent)

	// Root is the directory under which per-agent state files live.
	// Typically <ZotHome>/swarm, but tests pass a tempdir.
	Root string

	// RepoRoot is the working directory every spawned agent runs
	// in — the same cwd the parent zot is using. There is no
	// per-agent isolation: agents edit the host's files directly.
	RepoRoot string

	// NewRunner produces the Runner for an Agent. If nil, the default
	// `zot --swarm-agent ...` exec runner is used. Tests inject a fake
	// here.
	NewRunner func(a *Agent) Runner

	// ResolveCredential resolves the credential inherited by a child
	// process. The default runner transfers it over the child's stdin,
	// keeping command-backed credentials out of argv and the environment.
	ResolveCredential func(ctx context.Context, provider string) (Credential, error)

	// Now is a clock seam for tests; defaults to time.Now.
	Now func() time.Time
}

// Credential is the resolved authentication state inherited by a swarm child.
type Credential struct {
	Value     string `json:"value"`
	Method    string `json:"method"`
	AccountID string `json:"account_id,omitempty"`
}

// Runner executes one agent task. Run blocks until the task finishes,
// is cancelled via ctx, or hits an unrecoverable error.
//
// Run should report progress by writing short human-readable strings
// to the activity channel and final transcript text to transcript.
// Both channels are non-blocking sinks owned by the Swarm; if the
// dashboard isn't reading, sends are dropped.
type Runner interface {
	Run(ctx context.Context, sink Sink) error
}

// Sink is how a Runner reports activity and transcript back to the
// supervisor. All methods are safe to call from any goroutine and
// never block.
type Sink interface {
	// Activity sets the one-line "what is this agent doing right now"
	// string shown in the dashboard.
	Activity(msg string)
	// Transcript appends a chunk of agent output (typically a final
	// assistant message) to the agent's running transcript.
	Transcript(chunk string)
}

// Swarm supervises a set of Agents.
type Swarm struct {
	cfg Config

	mu     sync.Mutex
	agents map[string]*Agent
	order  []string // creation order for stable listing

	// activeSession is the host session id the dashboard is
	// currently scoped to. When non-empty, SnapshotAll filters out
	// agents whose SessionID doesn't match (legacy / unscoped
	// agents with SessionID == "" are always shown). Spawn stamps
	// new agents with this value so they appear only in the session
	// that created them. When empty (the default), the historical
	// "show everything" behaviour is preserved — important for
	// tests and any scripted use of the Swarm that doesn't bother
	// with sessions.
	activeSession string
}

// New constructs a Swarm from cfg. Missing config fields are filled
// with defaults.
func New(cfg Config) *Swarm {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewRunner == nil {
		cfg.NewRunner = func(a *Agent) Runner {
			return &execRunner{agent: a, resolveCredential: cfg.ResolveCredential}
		}
	}
	return &Swarm{
		cfg:    cfg,
		agents: map[string]*Agent{},
	}
}

// SetActiveSession scopes the dashboard view (and Spawn stamping)
// to a particular host zot session id. Pass empty to clear the
// scope and revert to "show every agent" (the original behaviour).
//
// Existing in-memory agents keep their SessionID; only the filter
// applied at snapshot time changes. So swapping the active session
// with /sessions instantly re-narrows the dashboard without
// touching any agent state.
func (f *Swarm) SetActiveSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeSession = id
}

// ActiveSession returns the current scope, mostly for tests and
// diagnostics. Empty means "no scope; show everything".
func (f *Swarm) ActiveSession() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeSession
}

// agentStateDir is the per-agent state directory laid out as:
//
//	<root>/agents/<id>/
//	  events.jsonl   durable event log (runner-owned)
//	  session.json   persistent agent session (child-owned)
//	  meta.json      static metadata (id, task)
//
// The transient unix socket inbox lives in a probed runtime directory,
// which may be on a different filesystem from this durable state.
func (f *Swarm) agentStateDir(id string) string {
	return filepath.Join(f.cfg.Root, "agents", id)
}

// SpawnRequest configures a Spawn. Only Task is required; the rest
// are optional. Model + Provider, when set, get baked into the
// child argv as --model / --provider so the agent runs against the
// chosen model regardless of the parent's current selection.
type SpawnRequest struct {
	Images   []provider.ImageBlock // optional initial image attachments
	Task     string
	Model    string // optional override; child resolves default if empty
	Provider string // optional override; usually paired with Model
}

// Spawn creates a new Agent for the given task, allocates its
// on-disk state directory (events log, inbox socket path, session
// file path), and starts the Runner on a background goroutine. The
// returned Agent is already in StatusRunning (or StatusFailed if
// state setup failed before the goroutine started). This is the
// historical signature; callers that want to override the child's
// model use SpawnReq instead.
func (f *Swarm) Spawn(ctx context.Context, task string) (*Agent, error) {
	return f.SpawnReq(ctx, SpawnRequest{Task: task})
}

// SpawnReq is the full-fat variant of Spawn that accepts a
// SpawnRequest. Existing callers can keep using Spawn; new code that
// wants to pin the child's model uses this.
//
// Every spawned agent runs with cwd == cfg.RepoRoot — the same
// working directory as the host. No per-agent worktree, no branch,
// no isolation. The user explicitly opted out of the worktree flow.
func (f *Swarm) SpawnReq(ctx context.Context, req SpawnRequest) (*Agent, error) {
	task := strings.TrimSpace(req.Task)
	if task == "" && len(req.Images) == 0 {
		return nil, errors.New("swarm: empty task")
	}
	prompt, err := PreparePrompt(task, req.Images)
	if err != nil {
		return nil, fmt.Errorf("swarm: task attachments: %w", err)
	}
	id := newAgentID(task, f.cfg.Now())
	dir := f.cfg.RepoRoot

	stateDir := f.agentStateDir(id)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("swarm state dir: %w", err)
	}
	logPath := filepath.Join(stateDir, "events.jsonl")
	sessionPath := filepath.Join(stateDir, "session.json")
	// Keep the transient unix socket outside the durable state root.
	// ZOT_HOME may live on a shared or network filesystem that supports
	// regular state files but cannot host unix socket nodes.
	inboxPath, err := inboxSocketPath(f.cfg.Root, id)
	if err != nil {
		return nil, fmt.Errorf("swarm inbox path: %w", err)
	}

	// Snapshot activeSession under the lock; we use it twice (struct
	// init below + persistence). Reading it without the lock would
	// race a concurrent SetActiveSession call.
	f.mu.Lock()
	sessionID := f.activeSession
	f.mu.Unlock()

	a := &Agent{
		ID:           id,
		Task:         task,
		Images:       prompt.Images,
		Dir:          dir,
		Started:      f.cfg.Now(),
		Model:        strings.TrimSpace(req.Model),
		Provider:     strings.TrimSpace(req.Provider),
		SessionID:    sessionID,
		InboxPath:    inboxPath,
		EventLogPath: logPath,
		SessionPath:  sessionPath,
		inbox:        NewInbox(inboxPath),
		status:       StatusPending,
		activity:     "queued",
		done:         make(chan struct{}),
	}
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.runner = f.cfg.NewRunner(a)

	f.mu.Lock()
	f.agents[id] = a
	f.order = append(f.order, id)
	f.mu.Unlock()

	// Persist the agent's identity so a later `zot` invocation can
	// reload it from disk via Swarm.Reload. Best-effort: if the disk
	// is read-only we still let the runner start, the user just won't
	// see this agent on the next launch.
	_ = writeAgentMeta(stateDir, a)

	go f.run(a)
	return a, nil
}

// SendInput delivers msg to the agent's inbox. msg should already
// include the leading kind ("user " + text, "cancel", "shutdown").
// Returns an error wrapping ErrNotReady if the child hasn't opened
// its listener yet; the caller can retry or surface the wait.
func (f *Swarm) SendInput(id, msg string) error {
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("no such agent %q", id)
	}
	if direct, ok := a.runner.(interface{ SendInput(string) error }); ok && a.runner != nil {
		switch a.Status() {
		case StatusRunning, StatusPending:
			return direct.SendInput(msg)
		default:
			return ErrNotReady
		}
	}
	if a.inbox == nil {
		return fmt.Errorf("agent %s has no inbox", a.ID)
	}
	return a.inbox.SendInput(msg)
}

// SendUserTurn is sugar for the common "send the next user turn"
// case. It quotes nothing and forwards verbatim; callers are
// expected to have already trimmed and expanded the text.
func (f *Swarm) SendUserTurn(id, text string) error {
	return f.SendInput(id, "user "+text)
}

// SendUserTurnWithImages transfers image bytes rather than filesystem paths.
// Text-only callers retain the legacy transport and its compatibility.
func (f *Swarm) SendUserTurnWithImages(id, text string, images []provider.ImageBlock) error {
	if len(images) == 0 {
		return f.SendUserTurn(id, text)
	}
	msg, err := EncodePrompt(text, images)
	if err != nil {
		return err
	}
	return f.SendInput(id, msg)
}

func (f *Swarm) run(a *Agent) {
	runID := rand.Text()
	sessionID := a.SessionID
	if sessionID == "" {
		sessionID = f.ActiveSession()
	}
	if f.cfg.OnLifecycle != nil {
		f.cfg.OnLifecycle(LifecycleEvent{Event: "subagent_start", SessionID: sessionID, AgentID: a.ID, AgentRunID: runID, Name: a.ID, CWD: a.Dir})
	}
	a.mu.Lock()
	if a.status == StatusPending {
		a.status = StatusRunning
		a.activity = "starting"
	}
	a.mu.Unlock()
	err := a.runner.Run(a.ctx, agentSink{a: a})
	a.mu.Lock()
	a.finished = f.cfg.Now()
	switch {
	case a.status == StatusKilled:
		// Already finalised by Stop.
	case errors.Is(err, context.Canceled):
		a.status = StatusKilled
		a.activity = "cancelled"
	case err != nil:
		a.status = StatusFailed
		a.activity = "error: " + truncate(err.Error(), 120)
		a.lastErr = err
	default:
		a.status = StatusDone
		a.activity = "done"
	}
	status := "completed"
	switch {
	case a.status == StatusKilled:
		status = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		status = "timed_out"
	case err != nil:
		status = "failed"
	}
	a.mu.Unlock()
	if f.cfg.OnLifecycle != nil {
		e := LifecycleEvent{Event: "subagent_stop", SessionID: sessionID, AgentID: a.ID, AgentRunID: runID, Name: a.ID, CWD: a.Dir, Status: status}
		if err != nil {
			e.Error = err.Error()
		}
		f.cfg.OnLifecycle(e)
	}
	close(a.done)
}

// List returns a snapshot of every agent in creation order. The
// returned slice is a copy; callers may iterate without holding the
// swarm lock. Agent fields are read under their own mutex during
// formatting in Snapshot.
func (f *Swarm) List() []*Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*Agent, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.agents[id])
	}
	return out
}

// Get returns the agent with the given (possibly truncated) id, or
// nil. Matching is prefix-based so the user can type the first few
// characters of a long id.
func (f *Swarm) Get(id string) *Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[id]; ok {
		return a
	}
	// Prefix match.
	var hits []*Agent
	for k, a := range f.agents {
		if strings.HasPrefix(k, id) {
			hits = append(hits, a)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return nil
}

// Stop cancels the agent's context. The Runner should observe the
// cancellation and return promptly; the goroutine then finalises the
// agent in StatusKilled. Also closes the supervisor-side inbox handle
// so any pending SendInput retries fail fast instead of dialing a
// socket about to be unlinked.
//
// Stop is a no-op for any agent that's not in a live runnable state
// — Done / Failed / Killed (already finalised) and Detached (no
// in-process runner; reloaded from disk). Calling Stop on a detached
// agent must not crash: buildDetachedAgent doesn't allocate a
// context/cancel pair because there's nothing to cancel.
func (f *Swarm) Stop(id string) error {
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("no such agent %q", id)
	}
	a.mu.Lock()
	switch a.status {
	case StatusDone, StatusFailed, StatusKilled, StatusDetached:
		a.mu.Unlock()
		return nil
	}
	a.status = StatusKilled
	a.activity = "stopped"
	a.mu.Unlock()
	// Belt-and-braces guard against the cancel-less case (detached
	// agents skip this branch above, but a future code path that
	// builds an Agent without a runner shouldn't crash the
	// supervisor).
	if a.cancel != nil {
		a.cancel()
	}
	if a.inbox != nil {
		_ = a.inbox.Close()
	}
	return nil
}

// StopAll cancels every running agent. Used on shutdown.
func (f *Swarm) StopAll() {
	for _, a := range f.List() {
		_ = f.Stop(a.ID)
	}
}

// Shutdown cancels local tasks and waits for terminal notifications until ctx
// expires. Detached tasks have no local runner and are not waited on.
func (f *Swarm) Shutdown(ctx context.Context) {
	f.StopAll()
	for _, a := range f.List() {
		if a.Status() == StatusDetached {
			continue
		}
		select {
		case <-a.done:
		case <-ctx.Done():
			return
		}
	}
}

// Remove tears down the per-agent state for a terminated agent. It
// is an error to remove an agent that's still running; call Stop
// first and wait for the status to settle. Detached agents
// (reloaded from disk) remove cleanly because they have no live
// runner racing for the same files.
//
// Agents share the host's working tree, so Remove never touches
// any source file — it only deletes the agent's state directory
// under <root>/agents/<id>/.
func (f *Swarm) Remove(id string) error {
	a := f.Get(id)
	if a == nil {
		return fmt.Errorf("no such agent %q", id)
	}
	a.mu.Lock()
	st := a.status
	a.mu.Unlock()
	if st == StatusRunning || st == StatusPending {
		return fmt.Errorf("agent %s still %s", a.ID, st)
	}
	// Best-effort cleanup of the per-agent state directory
	// (meta.json, events.jsonl, session.json). Failing here would leave
	// the user with no recourse,
	// so swallow the error.
	_ = os.RemoveAll(f.agentStateDir(a.ID))
	f.mu.Lock()
	delete(f.agents, a.ID)
	for i, k := range f.order {
		if k == a.ID {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	f.mu.Unlock()
	return nil
}

// Snapshot returns a read-only view of one agent. Safe for the TUI
// goroutine to call repeatedly; never blocks on the Runner.
type AgentSnapshot struct {
	ID            string
	Task          string
	Dir           string
	Status        Status
	Activity      string
	Started       time.Time
	Finished      time.Time
	Err           string
	Tail          string   // last few transcript lines, joined with "\n"
	Lines         []string // full transcript (already capped by Agent.appendTranscript)
	LastAssistant string   // complete final assistant message, without role formatting

	// Model and Provider expose the per-agent overrides set at
	// Spawn time (empty when the agent inherits the child's default
	// resolution). The dashboard surfaces these so the user can
	// confirm which model an agent is running against.
	Model    string
	Provider string

	// Paths to the agent's durable state. Surface them in the
	// snapshot so the dashboard / /swarm open can read events.jsonl
	// or resume the session without going back through the Agent.
	InboxPath    string
	EventLogPath string
	SessionPath  string

	// ConversationID is set for agents that run on a continuous host: the
	// owned conversation that holds the durable transcript.
	ConversationID string
}

// Snapshot copies the live agent state into a value the caller can
// inspect at leisure.
func (a *Agent) Snapshot() AgentSnapshot {
	conversationID := ""
	if hosted, ok := a.runner.(interface{ ConversationID() string }); ok && a.runner != nil {
		conversationID = hosted.ConversationID()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tail := strings.Join(lastN(a.transcript, 6), "\n")
	lines := make([]string, len(a.transcript))
	copy(lines, a.transcript)
	errStr := ""
	if a.lastErr != nil {
		errStr = a.lastErr.Error()
	}
	return AgentSnapshot{
		ID: a.ID, Task: a.Task, Dir: a.Dir,
		Status: a.status, Activity: a.activity,
		Started: a.Started, Finished: a.finished,
		Err: errStr, Tail: tail, Lines: lines,
		LastAssistant: a.lastAssistant,
		Model:         a.Model,
		Provider:      a.Provider,
		InboxPath:     a.InboxPath,
		EventLogPath:  a.EventLogPath,
		SessionPath:   a.SessionPath,

		ConversationID: conversationID,
	}
}

// SnapshotAll returns snapshots of every agent in creation order,
// scoped to the active session when one is set. Opening the dashboard
// backfills large detached logs once, rather than delaying startup.
//
// Scoping rules:
//   - activeSession == "": no filter, every agent is returned
//     (historical behaviour; used by tests and scripted callers).
//   - activeSession != "": include only agents whose SessionID
//     matches activeSession OR is empty. The empty-id pass-through
//     keeps pre-upgrade agents (their meta.json was written before
//     session_id existed) visible from any session so the user
//     doesn't lose access after the schema bump.
func (f *Swarm) SnapshotAll() []AgentSnapshot {
	agents := f.List()
	f.mu.Lock()
	active := f.activeSession
	f.mu.Unlock()

	out := make([]AgentSnapshot, 0, len(agents))
	for _, a := range agents {
		if active != "" && a.SessionID != "" && a.SessionID != active {
			continue
		}
		a.loadFullTranscript()
		out = append(out, a.Snapshot())
	}
	// Sort by start time for a stable, deterministic listing.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// agentSink is the Sink the Swarm hands to each Runner.
type agentSink struct{ a *Agent }

func (s agentSink) Activity(msg string) { s.a.setActivity(msg) }
func (s agentSink) Transcript(chunk string) {
	if strings.HasPrefix(chunk, "stderr: ") || strings.HasPrefix(chunk, "error: ") {
		s.a.appendTranscript(chunk)
		return
	}
	s.a.appendAssistantMessage(chunk)
}
func (s agentSink) userMessage(text string)      { s.a.appendUserMessage(text) }
func (s agentSink) assistantMessage(text string) { s.a.appendAssistantMessage(text) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	return s[:n-3] + "..."
}

func lastN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
