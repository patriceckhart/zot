package swarm

// On-disk persistence for swarm agents.
//
// Every Spawn writes a meta.json next to the agent's events.jsonl and
// session.json. The file captures the immutable identity bits (id,
// task, branch, dir) plus the paths the runner needs to resume the
// agent later. On a fresh zot launch, Swarm.Reload() walks
// <root>/agents/*/meta.json and re-registers every agent it finds in
// StatusDetached so the user can see, view, resume, or remove them
// from the dashboard.
//
// We don't try to keep meta.json in sync with mutable state (status,
// activity, transcript). Those live in the events log (durable) and
// in-memory Agent fields (rebuilt by Reload from the log tail).
// Keeping meta.json immutable means we never have to worry about
// concurrent writers stomping on each other and the file matters
// only on the spawn/reload boundary.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// agentMeta is the durable identity record for one agent. Only fields
// the supervisor needs to rebuild an Agent after a restart live here.
// Adding a field is backwards-compatible (older meta.json files just
// leave it zero); removing or renaming one is not.
//
// Historical fields like `branch` and `isolated` are silently dropped
// by encoding/json's permissive decoder when an older meta.json is
// loaded; we don't need to keep them in the struct.
type agentMeta struct {
	ID           string    `json:"id"`
	Task         string    `json:"task"`
	Dir          string    `json:"dir"`
	Started      time.Time `json:"started"`
	Model        string    `json:"model,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	InboxPath    string    `json:"inbox_path"`
	EventLogPath string    `json:"event_log_path"`
	SessionPath  string    `json:"session_path"`

	// SessionID, when non-empty, scopes the agent to a particular
	// host zot session: the dashboard only shows agents whose
	// SessionID matches the active session. Older meta.json files
	// (and agents spawned outside of any session, e.g. by tests or
	// scripted callers that didn't call SetActiveSession) have an
	// empty SessionID and are visible from every session as a
	// backward-compat fallback.
	SessionID string `json:"session_id,omitempty"`
}

func metaPath(stateDir string) string { return filepath.Join(stateDir, "meta.json") }

// writeAgentMeta serialises a's identity into stateDir/meta.json. The
// write is atomic (tmp + rename) so a crash mid-write can't leave a
// half-parsable file that fails Reload.
func writeAgentMeta(stateDir string, a *Agent) error {
	m := agentMeta{
		ID:           a.ID,
		Task:         a.Task,
		Dir:          a.Dir,
		Started:      a.Started,
		Model:        a.Model,
		Provider:     a.Provider,
		InboxPath:    a.InboxPath,
		EventLogPath: a.EventLogPath,
		SessionPath:  a.SessionPath,
		SessionID:    a.SessionID,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("swarm meta marshal: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("swarm meta dir: %w", err)
	}
	final := metaPath(stateDir)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("swarm meta write: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("swarm meta rename: %w", err)
	}
	return nil
}

// readAgentMeta loads one meta.json. Returns os.ErrNotExist (wrapped)
// when the file is missing so callers can distinguish "no such agent"
// from "corrupt metadata".
func readAgentMeta(stateDir string) (agentMeta, error) {
	var m agentMeta
	b, err := os.ReadFile(metaPath(stateDir))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("swarm meta parse %s: %w", stateDir, err)
	}
	if m.ID == "" {
		return m, fmt.Errorf("swarm meta %s: missing id", stateDir)
	}
	return m, nil
}

// Reload scans <root>/agents/*/meta.json and re-registers every
// previously-spawned agent as a StatusDetached entry. Agents already
// present in memory are left alone (Reload is idempotent and safe to
// call after Spawn, though in practice the cli invokes it exactly
// once just after New()).
//
// The reloaded agents have no live Runner; the user can:
//   - view their transcript (the dashboard reads from EventLogPath),
//   - resume them via Swarm.Resume (starts a fresh subprocess on the
//     same worktree / session / inbox path),
//   - remove them (worktree + meta + events log gone).
//
// Reload returns the number of agents loaded plus any per-directory
// error encountered. Malformed entries are skipped rather than
// failing the whole reload — one bad meta.json shouldn't hide the
// rest of the swarm.
func (f *Swarm) Reload() (loaded int, errs []error) {
	agentsDir := filepath.Join(f.cfg.Root, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, []error{fmt.Errorf("swarm reload: %w", err)}
	}

	// Sort by directory name so the load order is stable across runs.
	// agentStateDir uses the id verbatim so name-sort == id-sort,
	// which mirrors the creation order well enough for the dashboard
	// (we also sort by Started in SnapshotAll, but having a stable
	// f.order helps tests).
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		stateDir := filepath.Join(agentsDir, name)
		m, err := readAgentMeta(stateDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Bare directory with no meta.json — probably a
				// leftover from a Spawn that failed before
				// writeAgentMeta. Ignore silently.
				continue
			}
			errs = append(errs, err)
			continue
		}

		f.mu.Lock()
		if _, exists := f.agents[m.ID]; exists {
			f.mu.Unlock()
			continue
		}
		a := f.buildDetachedAgent(m)
		if err := a.loadQueue(); err != nil {
			a.queueLoadErr = err
			a.lastErr = err
			errs = append(errs, err)
		}
		f.agents[m.ID] = a
		f.order = append(f.order, m.ID)
		f.mu.Unlock()
		loaded++
	}
	return loaded, errs
}

// detachedReplayBytes bounds startup replay; full history is loaded
// when the dashboard is opened or the agent is resumed.
const detachedReplayBytes = 4 << 20

// buildDetachedAgent constructs an Agent from a meta.json with no
// running Runner. The agent's transcript is populated from the tail
// of its event log so the dashboard immediately shows recent output;
// activity is inferred from the last lifecycle event.
//
// The returned Agent has a closed `done` channel because Wait should
// return instantly: there is nothing to wait for.
func (f *Swarm) buildDetachedAgent(m agentMeta) *Agent {
	// Older meta.json files may still record a per-agent worktree
	// path under Dir. They predate the decision to run every agent
	// in the host's repo and shouldn't continue editing that stale
	// checkout, which most likely no longer matches HEAD. Coerce
	// the dir back to the live RepoRoot so resume picks up where
	// the host is now.
	dir := m.Dir
	if f.cfg.RepoRoot != "" {
		dir = f.cfg.RepoRoot
	}
	a := &Agent{
		ID:           m.ID,
		Task:         m.Task,
		Dir:          dir,
		Started:      m.Started,
		Model:        m.Model,
		Provider:     m.Provider,
		InboxPath:    m.InboxPath,
		EventLogPath: m.EventLogPath,
		SessionPath:  m.SessionPath,
		SessionID:    m.SessionID,
		inbox:        NewInbox(m.InboxPath),
		status:       StatusDetached,
		activity:     "detached",
		done:         make(chan struct{}),
	}
	// Wait() must not block for detached agents; close the channel
	// immediately. Callers Resuming the agent will replace done with
	// a fresh channel before starting the new runner.
	close(a.done)

	// Recover transcript + activity hints from the event log. Best
	// effort: a missing or unreadable log just leaves the agent
	// detached with an empty transcript.
	if a.EventLogPath != "" {
		if evs, err := ReadEventLogTail(a.EventLogPath, detachedReplayBytes); err == nil {
			replayEventsIntoAgent(a, evs)
		}
		if fi, err := os.Stat(a.EventLogPath); err == nil && fi.Size() > detachedReplayBytes {
			a.needsFullReplay = true
		}
	}
	return a
}

// loadFullTranscript backfills a detached agent on demand. Keep the
// status inferred from the newest lifecycle event in the startup tail.
// The agent's mutex serializes concurrent dashboard refreshes and resume.
func (a *Agent) loadFullTranscript() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.needsFullReplay {
		return
	}
	evs, err := ReadEventLog(a.EventLogPath)
	if err != nil {
		return
	}
	full := &Agent{status: StatusDetached, activity: "detached"}
	replayEventsIntoAgent(full, evs)
	a.transcript = full.transcript
	a.lastAssistant = full.lastAssistant
	a.needsFullReplay = false
}

// replayEventsIntoAgent re-derives an agent's transcript and last
// known status hint from its event log. Mirrors applyEventToSink in
// runner.go but writes directly to the Agent fields (no Sink because
// the agent isn't being driven by a runner yet).
//
// Status precedence: explicit lifecycle events (agent_stopped) win
// over inferred ones (assistant_message → idle). If the log contains
// no terminator we keep status=StatusDetached so the user can resume.
func replayEventsIntoAgent(a *Agent, evs []Event) {
	terminal := false
	for _, ev := range evs {
		switch ev.Type {
		case "assistant_message", "user_message":
			var text []string
			if c, ok := ev.Data["content"].([]any); ok {
				for _, blk := range c {
					m, _ := blk.(map[string]any)
					if t, _ := m["type"].(string); t == "text" {
						if txt, _ := m["text"].(string); txt != "" {
							text = append(text, txt)
						}
					}
				}
			}
			message := strings.Join(text, "\n")
			if ev.Type == "assistant_message" {
				a.appendAssistantMessage(message)
			} else {
				a.appendUserMessage(message)
			}
		case "tool_call", "tool_result":
			a.appendTranscript(toolTranscript(ev))
		case "stdout":
			if txt, _ := ev.Data["text"].(string); txt != "" {
				a.appendTranscript(txt)
			}
		case "stderr":
			if txt, _ := ev.Data["text"].(string); txt != "" {
				a.appendTranscript("stderr: " + txt)
			}
		case "error":
			if msg, _ := ev.Data["message"].(string); msg != "" {
				a.appendTranscript("error: " + msg)
			}
		case "agent_stopped":
			terminal = true
			reason, _ := ev.Data["reason"].(string)
			a.mu.Lock()
			switch reason {
			case "cancelled":
				a.status = StatusKilled
				a.activity = "cancelled (offline)"
			case "shutdown":
				a.status = StatusDone
				a.activity = "shutdown (offline)"
			case "exit":
				if code, ok := ev.Data["code"].(float64); ok && code != 0 {
					a.status = StatusFailed
					a.activity = fmt.Sprintf("exit %d (offline)", int(code))
				} else {
					a.status = StatusDone
					a.activity = "done (offline)"
				}
			default:
				a.status = StatusDone
				a.activity = "stopped (offline)"
			}
			a.mu.Unlock()
		}
	}
	if !terminal {
		// Non-terminal log means the previous parent died mid-run.
		// Leave status=StatusDetached but record a hint so the
		// dashboard shows something useful.
		a.mu.Lock()
		if a.activity == "detached" && len(a.transcript) > 0 {
			a.activity = "detached (resume to continue)"
		}
		a.mu.Unlock()
	}
}

// Resume re-attaches a Runner to a previously-spawned agent. Durable
// session and event files are kept, while the transient inbox path is
// recalculated for the current runtime environment. Use this to
// continue a swarm session across zot restarts:
//
//	swarmMgr.Reload()
//	a, err := swarmMgr.Resume(ctx, "alpha-12345")
//	swarmMgr.SendUserTurn(a.ID, "where were we?")
//
// The agent must be in a non-running state (Detached, Done, Failed,
// Killed). Resuming a still-running agent returns an error so two
// runners don't race for the same session.
func (f *Swarm) Resume(ctx context.Context, id string) (*Agent, error) {
	f.resumeMu.Lock()
	defer f.resumeMu.Unlock()
	existing := f.Get(id)
	if existing == nil {
		return nil, fmt.Errorf("swarm: no such agent %q", id)
	}
	existing.mu.Lock()
	st := existing.status
	existing.mu.Unlock()
	if st == StatusRunning || st == StatusPending {
		return nil, fmt.Errorf("swarm: agent %s is still %s; stop it first", existing.ID, st)
	}
	select {
	case <-existing.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	existing.queueWG.Wait()
	existing.loadFullTranscript()

	// Rebuild from the meta record so we don't carry stale runner
	// state from a previous incarnation. The inbox is transient and
	// may have been persisted under an incompatible filesystem by an
	// older zot version, so always select it again on resume.
	inboxPath, err := inboxSocketPath(f.cfg.Root, existing.ID)
	if err != nil {
		return nil, fmt.Errorf("swarm inbox path: %w", err)
	}
	m := agentMeta{
		ID: existing.ID, Task: existing.Task,
		Dir: existing.Dir, Started: existing.Started,
		Model: existing.Model, Provider: existing.Provider,
		InboxPath: inboxPath, EventLogPath: existing.EventLogPath,
		SessionPath: existing.SessionPath,
		SessionID:   existing.SessionID,
	}

	a := &Agent{
		ID:           m.ID,
		Task:         m.Task,
		Dir:          m.Dir,
		Started:      m.Started,
		Model:        m.Model,
		Provider:     m.Provider,
		InboxPath:    m.InboxPath,
		EventLogPath: m.EventLogPath,
		SessionPath:  m.SessionPath,
		Resuming:     true,
		SessionID:    m.SessionID,
		inbox:        NewInbox(m.InboxPath),
		status:       StatusPending,
		activity:     "resuming",
		done:         make(chan struct{}),
	}
	// Carry the previous transcript forward so the dashboard doesn't
	// flash empty between resume and the first new event.
	prev := existing.Transcript()
	if len(prev) > 0 {
		a.appendTranscript(strings.Join(prev, "\n"))
	}
	a.lastAssistant = existing.Snapshot().LastAssistant
	if err := a.loadQueue(); err != nil {
		return nil, err
	}
	if len(a.followups) > 0 {
		if err := a.writeQueueLocked(); err != nil {
			return nil, err
		}
	}
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.runner = f.cfg.NewRunner(a)

	f.mu.Lock()
	f.agents[a.ID] = a
	// Keep the agent's slot in f.order; replacing in-place avoids
	// reshuffling the dashboard's row ordering on resume.
	found := false
	for _, k := range f.order {
		if k == a.ID {
			found = true
			break
		}
	}
	if !found {
		f.order = append(f.order, a.ID)
	}
	f.mu.Unlock()

	// Refresh the meta.json so any new path defaults (e.g. socket
	// path moved into /tmp because the root got renamed) get
	// persisted. Best-effort; resume still works if the disk is
	// read-only because everything the runner needs is in-memory.
	_ = writeAgentMeta(f.agentStateDir(a.ID), a)

	go f.run(a)
	a.mu.Lock()
	f.startQueueLocked(a)
	a.mu.Unlock()
	return a, nil
}
