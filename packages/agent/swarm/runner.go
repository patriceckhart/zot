package swarm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// execRunner spawns `zot --swarm-agent <inbox> --session <path>` in
// the host's working directory (Agent.Dir, which is always the parent
// zot's RepoRoot) and consumes its JSONL event stream on stdout.
//
// Why a long-lived daemon and not `zot --print`: the supervisor and
// the user expect agents to keep accepting follow-up prompts. A
// one-shot subprocess can't do that; this design gives each swarm
// agent a persistent session file plus an inbox socket the parent
// writes to, mirroring Claude Code's "Agents view" model.
//
// Events flow:
//
//	child stdout  -->  decoder  -->  EventLog (events.jsonl)
//	                              -->  Sink (Activity/Transcript)
//
// The on-disk log is the durable record. The Sink updates are an
// in-memory mirror so the dashboard doesn't have to tail the file
// for the parent's own agents. /swarm open in a separate zot would
// read the log directly.
type execRunner struct {
	agent             *Agent
	resolveCredential func(context.Context, string) (Credential, error)

	// Command overrides the default `zot --swarm-agent ...`
	// invocation. Tests set this to a fake binary (or `go run`
	// against a tiny stub program) so the supervisor logic can be
	// tested without a real child. Production code leaves it nil.
	Command []string

	// SessionPath is the agent's session file. Empty means "defer
	// to r.agent.SessionPath", which Swarm.Spawn always populates
	// with <swarm-root>/agents/<id>/session.json. Tests that
	// hand-build an Agent without going through Spawn must set
	// one of the two; the runner refuses to invent a fallback
	// because the only plausible one (<Dir>/.zot/session.json)
	// would litter the user's repo — every agent's Dir points
	// at it directly.
	SessionPath string
}

// swarmAgentArgsOpts captures every dynamic input to swarmAgentArgs
// so future per-agent overrides (e.g. tools, reasoning) can be added
// without churning the signature. The fields map 1:1 onto child CLI
// flags; empty values omit the flag entirely and let the child
// resolve a default the same way a normal `zot` invocation does.
type swarmAgentArgsOpts struct {
	Exe         string
	Dir         string
	SessionPath string
	InboxPath   string
	Task        string
	Model       string
	Provider    string
}

// defaultChildArgs builds the argv execRunner uses when its Command
// override is empty. Centralised so the spawn-vs-resume decision
// (whether to pass the original Task as a positional) lives in one
// place that tests can hit directly without going through Run's
// side effects.
//
// On Spawn (Resuming==false) we pass the task so the child's first
// turn runs immediately. On Resume (Resuming==true) we omit it: the
// child reopens the existing session file, loads the prior
// conversation, and just waits on the inbox for the next prompt.
// Re-firing the task on every resume produces a duplicate turn that
// collides with whatever the user types next, surfacing the
// "agent busy; send 'cancel' first" error between two assistant
// messages — which is exactly the bug this helper fixes.
func defaultChildArgs(exe string, a *Agent, sessionPath, inboxPath string) []string {
	task := a.Task
	if a.Resuming || len(a.Images) > 0 {
		task = ""
	}
	return swarmAgentArgs(swarmAgentArgsOpts{
		Exe:         exe,
		Dir:         a.Dir,
		SessionPath: sessionPath,
		InboxPath:   inboxPath,
		Task:        task,
		Model:       a.Model,
		Provider:    a.Provider,
	})
}

// swarmAgentArgs builds the argv used when execRunner.Command is
// empty. Pulled out so tests can lock in the flag set without
// actually spawning a subprocess.
func swarmAgentArgs(opts swarmAgentArgsOpts) []string {
	args := []string{
		opts.Exe,
		"--swarm-agent", opts.InboxPath,
		"--session", opts.SessionPath,
		"--cwd", opts.Dir,
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Provider != "" {
		args = append(args, "--provider", opts.Provider)
	}
	if opts.Task != "" {
		// First task is positional so the child treats it as the
		// initial user turn; subsequent turns arrive via the inbox.
		args = append(args, opts.Task)
	}
	return args
}

func (r *execRunner) Run(ctx context.Context, sink Sink) error {
	// SessionPath resolution order:
	//   1. explicit r.SessionPath set by the test / caller
	//   2. r.agent.SessionPath baked in by Swarm.Spawn — the
	//      production path. Always lives under
	//      <swarm-root>/agents/<id>/session.json so the per-
	//      agent state is entirely outside the working tree.
	//      Crucial because Agent.Dir points at the user's repo;
	//      any .zot/ scratch directory under Dir would litter
	//      their source tree.
	//
	// There is no third fallback. If neither path is set we
	// refuse to start instead of inventing a directory; that
	// way a misconfigured caller fails loudly the first time
	// instead of silently dumping session data into someone's
	// repo.
	sessionPath := r.SessionPath
	if sessionPath == "" {
		sessionPath = r.agent.SessionPath
	}
	if sessionPath == "" {
		return fmt.Errorf("swarm: agent missing session path (set SpawnRequest via Swarm.SpawnReq, or hand-build Agent with SessionPath populated)")
	}
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		return fmt.Errorf("session dir: %w", err)
	}

	inboxPath := r.agent.InboxPath
	logPath := r.agent.EventLogPath
	if logPath == "" {
		return fmt.Errorf("swarm: agent missing event log path")
	}
	log, err := OpenEventLog(logPath)
	if err != nil {
		return err
	}
	defer log.Close()

	args := r.Command
	if len(args) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate self: %w", err)
		}
		args = defaultChildArgs(exe, r.agent, sessionPath, inboxPath)
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = r.agent.Dir
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "ZOT_SWARM_CREDENTIAL_STDIN=") && !strings.HasPrefix(env, "ZOT_SWARM_PROMPT_STDIN=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "ZOT_SWARM_AGENT_ID="+r.agent.ID, "ZOT_SWARM_EVENT_LOG="+logPath)
	var startup StartupInput
	if !r.agent.Resuming && len(r.agent.Images) > 0 {
		prompt, err := PreparePrompt(r.agent.Task, r.agent.Images)
		if err != nil {
			return fmt.Errorf("swarm task attachments: %w", err)
		}
		startup.Prompt = &prompt
		cmd.Env = append(cmd.Env, "ZOT_SWARM_PROMPT_STDIN=1")
	}
	if r.resolveCredential != nil {
		credential, resolveErr := r.resolveCredential(ctx, r.agent.Provider)
		if resolveErr != nil {
			return fmt.Errorf("resolve swarm credential for %s: %w", r.agent.Provider, resolveErr)
		}
		if credential.Value != "" {
			startup.Credential = credential
			cmd.Env = append(cmd.Env, "ZOT_SWARM_CREDENTIAL_STDIN=1")
		}
	}
	if startup.Prompt != nil || startup.Credential.Value != "" {
		encoded, err := json.Marshal(startup)
		if err != nil || len(encoded) > MaxPromptWireBytes {
			return fmt.Errorf("swarm: startup input cannot be encoded within %d bytes", MaxPromptWireBytes)
		}
		cmd.Stdin = bytes.NewReader(encoded)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// "spawning" is briefly shown until the first event arrives;
	// the child's "spawned" lifecycle event then overwrites it.
	sink.Activity("starting")
	if err := cmd.Start(); err != nil {
		return err
	}

	// stdout: parsed as JSONL. Every well-formed event is appended
	// to the durable log AND forwarded to the in-memory sink so the
	// dashboard updates without having to tail the file. Malformed
	// lines are surfaced as plain transcript so they don't vanish.
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		dec := bufio.NewReader(stdout)
		for {
			line, err := dec.ReadBytes('\n')
			if len(line) > 0 {
				trimmed := strings.TrimRight(string(line), "\r\n")
				if trimmed == "" {
					goto next
				}
				if ev, ok := parseEventLine(trimmed); ok {
					_ = log.Append(ev)
					applyEventToSink(ev, sink)
					// Fan prompt-level task completions up to any
					// subscriber on the supervised Agent. The child
					// also forwards provider/tool-loop turn_end
					// events (for example stop=tool_use); those do
					// not contain step and must not be treated as
					// swarm task completion.
					notifyPromptTurnEnd(r.agent, ev)
				} else {
					// Non-JSON output. Keep it as transcript so an
					// accidental fmt.Println in the child still
					// shows up in the dashboard, and log a
					// lifecycle event so the durable record stays
					// in sync.
					sink.Transcript(trimmed)
					_ = log.Append(NewEvent("stdout", map[string]any{"text": trimmed}))
				}
			}
		next:
			if err != nil {
				return
			}
		}
	}()

	// stderr: lifecycle/error chatter from the child. Every line
	// is mirrored as a stderr event in the durable log AND surfaced
	// in the transcript so users can diagnose a failing agent
	// without leaving the dashboard.
	go func() {
		defer func() { done <- struct{}{} }()
		br := bufio.NewReader(stderr)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				txt := strings.TrimRight(line, "\r\n")
				sink.Transcript("stderr: " + txt)
				_ = log.Append(NewEvent("stderr", map[string]any{"text": txt}))
			}
			if err != nil {
				return
			}
		}
	}()

	<-done
	<-done

	err = cmd.Wait()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	}
	if err != nil && ctx.Err() != nil {
		_ = log.Append(NewEvent("agent_stopped", map[string]any{"reason": "cancelled"}))
		return ctx.Err()
	}
	if err != nil {
		_ = log.Append(NewEvent("agent_stopped", map[string]any{"reason": "exit", "code": exit, "error": err.Error()}))
		return err
	}
	_ = log.Append(NewEvent("agent_stopped", map[string]any{"reason": "exit", "code": 0}))
	sink.Activity("done")
	return nil
}

// parseEventLine attempts to decode one JSONL line as an Event.
// Returns ok=false for non-JSON or JSON without a "type" field.
func parseEventLine(line string) (Event, bool) {
	if len(line) == 0 || line[0] != '{' {
		return Event{}, false
	}
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return Event{}, false
	}
	if ev.Type == "" {
		return Event{}, false
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	return ev, true
}

// notifyPromptTurnEnd calls Agent.OnTurnEnd only for the swarm
// daemon's prompt-level completion event. Provider/tool-loop
// turn_end events (such as stop=tool_use) do not include step and
// are not terminal for the delegated task.
func notifyPromptTurnEnd(a *Agent, ev Event) {
	if a == nil || ev.Type != "turn_end" {
		return
	}
	step, ok := ev.Data["step"].(float64)
	if !ok {
		return
	}

	a.mu.Lock()
	fn := a.OnTurnEnd
	a.mu.Unlock()
	if fn != nil {
		errMsg, _ := ev.Data["error"].(string)
		go fn(int(step), errMsg)
	}
}

// applyEventToSink translates an Event into Sink updates. Only a
// few event types are interpreted; the rest still land in the
// durable log via the caller.
func applyEventToSink(ev Event, sink Sink) {
	type roleSink interface {
		userMessage(string)
		assistantMessage(string)
	}
	appendMessage := func(text string, assistant bool) {
		if text == "" {
			return
		}
		if roles, ok := sink.(roleSink); ok {
			if assistant {
				roles.assistantMessage(text)
			} else {
				roles.userMessage(text)
			}
			return
		}
		if assistant {
			sink.Transcript(text)
		} else {
			sink.Transcript("user: " + text)
		}
	}

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
		appendMessage(strings.Join(text, "\n"), ev.Type == "assistant_message")
		if ev.Type == "assistant_message" {
			sink.Activity("idle")
		}
	case "turn_start":
		sink.Activity("thinking")
	case "tool_call":
		if name, _ := ev.Data["name"].(string); name != "" {
			sink.Activity("tool: " + truncate(name, 60))
		}
	case "tool_result":
		sink.Activity("idle")
	case "turn_end":
		sink.Activity("idle")
	case "agent_ready":
		sink.Activity("idle")
	case "agent_stopped":
		// terminal status is decided by Swarm.run from the runner's
		// return value, not from this event. Don't overwrite the
		// activity here.
	case "error":
		if msg, _ := ev.Data["message"].(string); msg != "" {
			sink.Transcript("error: " + msg)
		}
	}
}

// RunnerFunc adapts a plain function into a Runner. Useful for tests
// and for callers who don't need their own type.
type RunnerFunc func(ctx context.Context, sink Sink) error

func (f RunnerFunc) Run(ctx context.Context, sink Sink) error { return f(ctx, sink) }

// streamLines kept around for any caller still using it directly.
//
// Deprecated: the runner now parses JSONL from stdout via
// parseEventLine; this helper is unused inside the package but
// remains exported via internal use by tests in the runner_test
// suite that pre-date the daemon switch.
func streamLines(r io.Reader, fn func(string)) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			fn(strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			return
		}
	}
}
