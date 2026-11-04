package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// HostRunner runs a swarm agent as an owned conversation on a continuous
// host instead of a child process. Ownership, scheduling, recovery, and the
// durable transcript live in the host: the supervisor only submits input and
// mirrors committed entries into the agent's dashboard state and event log.
//
// The child conversation is created once per swarm agent ID, so a resumed or
// re-spawned agent attaches to the conversation the host already has. The
// initial task is submitted with a request ID derived from the agent ID, so a
// re-run of the same agent does not queue the task twice. Stopping the agent detaches
// the supervisor; it does not abort host work unless AbortOnStop is set.
type HostRunner struct {
	// Dial opens a client session with the host. It is called once per Run
	// so a resumed agent reconnects with fresh credentials.
	Dial func(ctx context.Context) (*continuous.Client, error)
	// Parent is the host conversation that owns every swarm child. Empty
	// means the workspace root for Workspace.
	Parent string
	// Workspace selects the root conversation when Parent is empty.
	Workspace string
	// AbortOnStop requests an abort of the child's active run when the
	// supervisor stops the agent. Off by default: the host keeps working and
	// a later Resume shows what happened.
	AbortOnStop bool

	agent *Agent

	mu       sync.Mutex
	client   *continuous.Client
	childID  string
	sendSeq  int
	inputs   chan string
	inputsMu sync.Once
}

// NewHostRunnerFactory returns a Config.NewRunner that runs every agent on the
// host reached through dial. Children are owned by the workspace root of
// workspace.
func NewHostRunnerFactory(dial func(ctx context.Context) (*continuous.Client, error), workspace string) func(*Agent) Runner {
	return func(a *Agent) Runner {
		return &HostRunner{Dial: dial, Workspace: workspace, agent: a}
	}
}

// ConversationID returns the owned conversation the agent runs in, once
// known. The dashboard shows it so a user can attach to the child directly.
func (r *HostRunner) ConversationID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.childID
}

func (r *HostRunner) inputChannel() chan string {
	r.inputsMu.Do(func() { r.inputs = make(chan string, 16) })
	return r.inputs
}

// SendInput accepts the supervisor protocol messages ("user <text>",
// "cancel", "shutdown") without a socket. It is used by Swarm.SendInput when
// the agent has no inbox.
func (r *HostRunner) SendInput(msg string) error {
	select {
	case r.inputChannel() <- msg:
		return nil
	default:
		return fmt.Errorf("swarm: host runner input queue is full")
	}
}

// Run creates or finds the child conversation, submits the initial task on
// first spawn, and mirrors committed entries until the supervisor stops it or
// a shutdown message arrives. It returns nil when the agent was shut down
// cleanly, ctx.Err() when stopped, and an error when the host rejected work.
func (r *HostRunner) runConnected(ctx context.Context, sink Sink, state *hostRunState) (runErr error) {
	if r.Dial == nil || r.agent == nil {
		return errors.New("swarm: host runner requires a dial function and an agent")
	}
	client, err := r.Dial(ctx)
	if err != nil {
		return fmt.Errorf("swarm: connect to host: %w", err)
	}
	defer client.Close()
	defer func() {
		if runErr != nil && ctx.Err() == nil {
			if hostTransportError(runErr) {
				runErr = errHostDisconnected
			}
		}
	}()
	r.mu.Lock()
	r.client = client
	r.mu.Unlock()

	var log *EventLog
	if r.agent.EventLogPath != "" {
		if log, err = OpenEventLog(r.agent.EventLogPath); err != nil {
			return err
		}
		defer log.Close()
	}
	emit := func(typ string, data map[string]any) {
		if log != nil {
			_ = log.Append(NewEvent(typ, data))
		}
	}

	parent := r.Parent
	if parent == "" {
		var root continuous.Conversation
		if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": r.Workspace}, &root); err != nil {
			return fmt.Errorf("swarm: open workspace conversation: %w", err)
		}
		parent = root.ID
	}
	var config *continuous.AgentConfig
	if r.agent.Model != "" || r.agent.Provider != "" {
		var parentConv continuous.ConversationSnapshot
		if err := client.CallInto(ctx, "conversation.snapshot", map[string]any{"id": parent, "limit": 1}, &parentConv); err != nil {
			return fmt.Errorf("swarm: read parent conversation: %w", err)
		}
		cfg := parentConv.Conversation.Config
		if r.agent.Model != "" {
			cfg.Model = r.agent.Model
		}
		if r.agent.Provider != "" {
			cfg.Provider = r.agent.Provider
		}
		config = &cfg
	}
	var child continuous.Conversation
	params := map[string]any{"owner": continuous.Owner{ConversationID: parent, ID: "swarm/" + r.agent.ID}, "key": r.agent.ID}
	if config != nil {
		params["config"] = config
	}
	if err := client.CallInto(ctx, "conversation.create", params, &child); err != nil {
		return fmt.Errorf("swarm: create owned conversation: %w", err)
	}
	r.mu.Lock()
	r.childID = child.ID
	r.mu.Unlock()
	emit("agent_ready", map[string]any{"conversation": child.ID, "host": true})
	sink.Activity("idle")

	// Mirror committed history into the dashboard, then follow new commits.
	var snap continuous.ConversationSnapshot
	if err := client.CallInto(ctx, "conversation.snapshot", map[string]any{"id": child.ID, "limit": 1000}, &snap); err != nil {
		return fmt.Errorf("swarm: snapshot child: %w", err)
	}
	if !state.attached && r.agent.Resuming {
		// New logs retain entry IDs, allowing offline commits to be backfilled.
		events, _ := ReadEventLog(r.agent.EventLogPath)
		for _, ev := range events {
			if id, _ := ev.Data["entry_id"].(string); id != "" {
				state.seen[id] = true
			}
		}
		if len(state.seen) == 0 {
			for _, e := range snap.Entries {
				state.seen[e.ID] = true
			}
		}
	}
	state.mirror(r, snap.Entries, sink, emit)
	state.attached = true
	watch, err := client.Watch(ctx, child.ID, snap.Revision)
	if err != nil {
		return fmt.Errorf("swarm: watch child: %w", err)
	}

	// Follow-up request IDs are unique per supervisor incarnation: a resumed
	// agent must not collide with the IDs an earlier incarnation used.
	incarnation := state.incarnation
	requireImages := func(images []provider.ImageBlock) error {
		if len(images) == 0 {
			return nil
		}
		status, err := client.Call(ctx, "runtime.status", nil)
		if err != nil {
			return err
		}
		return continuous.RequireAttachmentSupport(status)
	}
	submit := func(prompt Prompt) error {
		if err := requireImages(prompt.Images); err != nil {
			return err
		}
		requestID := "swarm/" + r.agent.ID + "/" + prompt.RequestID
		var sub continuous.Submission
		params := map[string]any{"id": child.ID, "content": prompt.Text, "request_id": requestID}
		if len(prompt.Images) > 0 {
			params["images"] = prompt.Images
		}
		if err := client.CallInto(ctx, "conversation.submit", params, &sub); err != nil {
			return err
		}
		state.submissions[sub.ID] = prompt.RequestID
		sink.Activity("queued")
		return nil
	}
	if !state.initialAdded {
		state.initialAdded = true
		if !r.agent.Resuming && (r.agent.Task != "" || len(r.agent.Images) > 0) {
			state.prompts["task"] = Prompt{Text: r.agent.Task, Images: r.agent.Images, RequestID: "task"}
		}
	}
	// Only stable, idempotent host request IDs are retried after transport loss.
	for _, prompt := range state.prompts {
		if err := submit(prompt); err != nil {
			return fmt.Errorf("swarm: submit pending prompt: %w", err)
		}
	}
	if err := state.reconcile(ctx, client, child.ID, r, sink, emit); err != nil {
		return err
	}
	notifyPromptTurnEnd(r.agent, NewEvent("agent_ready", map[string]any{"busy": len(state.submissions) > 0 || (snap.Run != nil && snap.Run.Phase != "done")}))

	inputs := r.inputChannel()
	activeRun := ""
	if snap.Run != nil && snap.Run.Phase != "done" {
		activeRun = snap.Run.ID
		notifyPromptTurnEnd(r.agent, NewEvent("turn_start", map[string]any{"step": float64(snap.Run.Turn)}))
		sink.Activity("thinking")
	}
	for {
		select {
		case <-ctx.Done():
			if r.AbortOnStop {
				abortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, _ = client.Call(abortCtx, "conversation.abort", map[string]any{"id": child.ID})
				cancel()
			}
			emit("agent_stopped", map[string]any{"reason": "cancelled", "conversation": child.ID})
			return ctx.Err()
		case <-client.Done():
			emit("agent_disconnected", map[string]any{"conversation": child.ID})
			return errHostDisconnected
		case msg := <-inputs:
			switch {
			case msg == "shutdown":
				emit("agent_stopped", map[string]any{"reason": "shutdown", "conversation": child.ID})
				return nil
			case msg == "cancel":
				if _, err := client.Call(ctx, "conversation.abort", map[string]any{"id": child.ID}); err != nil {
					var ce *continuous.ClientError
					if !errors.As(err, &ce) || ce.Code != "not_found" {
						sink.Transcript("error: cancel: " + err.Error())
					}
				}
			case strings.HasPrefix(msg, "user ") || strings.HasPrefix(msg, "user-prompt "):
				prompt, err := DecodePrompt(msg)
				if err == nil {
					if prompt.RequestID == "" {
						r.sendSeq++
						prompt.RequestID = fmt.Sprintf("%s/%d", incarnation, r.sendSeq)
					}
					state.prompts[prompt.RequestID] = prompt
					err = submit(prompt)
				}
				if err != nil {
					if hostTransportError(err) {
						return errHostDisconnected
					}
					delete(state.prompts, prompt.RequestID)
					data := map[string]any{"step": float64(1), "request_id": prompt.RequestID, "error": "submit: " + err.Error()}
					emit("turn_end", data)
					notifyPromptTurnEnd(r.agent, NewEvent("turn_end", data))
				} else if err := state.reconcile(ctx, client, child.ID, r, sink, emit); err != nil {
					return err
				}
			default:
				sink.Transcript("error: unknown supervisor message")
			}
		case cm, ok := <-watch:
			if !ok {
				if ctx.Err() != nil {
					continue
				}
				// The watch fell behind: resnapshot and continue following.
				if err := client.CallInto(ctx, "conversation.snapshot", map[string]any{"id": child.ID, "limit": 1000}, &snap); err != nil {
					return fmt.Errorf("swarm: resnapshot child: %w", err)
				}
				state.mirror(r, snap.Entries, sink, emit)
				r.agent.setPromptBusy(snap.Run != nil && snap.Run.Phase != "done")
				if err := state.reconcile(ctx, client, child.ID, r, sink, emit); err != nil {
					return err
				}
				if watch, err = client.Watch(ctx, child.ID, snap.Revision); err != nil {
					return fmt.Errorf("swarm: rewatch child: %w", err)
				}
				continue
			}
			entries := continuous.EntriesFromCommit(cm, child.ID)
			state.mirror(r, entries, sink, emit)
			if run, ok := continuous.RunFromCommit(cm, child.ID); ok {
				switch {
				case run.Phase == "done":
					activeRun = ""
					r.agent.setPromptBusy(false)
					if err := state.reconcile(ctx, client, child.ID, r, sink, emit); err != nil {
						return err
					}
					sink.Activity("idle")
				case run.Phase != "done" && run.ID != activeRun:
					activeRun = run.ID
					emit("turn_start", map[string]any{"step": run.Turn})
					notifyPromptTurnEnd(r.agent, NewEvent("turn_start", map[string]any{"step": float64(run.Turn)}))
					sink.Activity("thinking")
				}
			}
		}
	}
}

// mirrorEntries applies committed entries to the dashboard sink, skipping
// entries already seen when seen is not nil.
func (r *HostRunner) mirrorEntries(entries []continuous.Entry, sink Sink, seen map[string]bool) {
	for _, e := range entries {
		if seen != nil {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
		}
		// Decode the same wire shape used by event-log replay so live and
		// restored transcripts retain identical tool arguments and results.
		emitEntry(func(typ string, data map[string]any) {
			raw, err := json.Marshal(NewEvent(typ, data))
			if err != nil {
				return
			}
			if ev, ok := parseEventLine(string(raw)); ok {
				applyEventToSink(ev, sink)
			}
		}, e)
		if e.Type == "user" || e.Type == "steer" {
			sink.Activity("thinking")
		}
		if e.Type == "reset" {
			sink.Transcript("context reset")
		}
	}
}

// emitEntry records a committed entry in the agent's event log using the
// event types the dashboard replays.
func emitEntry(emit func(string, map[string]any), e continuous.Entry) {
	switch e.Type {
	case "user", "steer":
		content := []map[string]any{{"type": "text", "text": e.Content}}
		for _, image := range e.Images {
			content = append(content, map[string]any{"type": "image", "mime_type": image.MimeType, "bytes": len(image.Data)})
		}
		emit("user_message", map[string]any{"content": content, "time": e.Time})
	case "assistant":
		msg, err := core.DecodeMessage(e.Message)
		if err != nil {
			return
		}
		var content []map[string]any
		for _, block := range msg.Content {
			switch v := block.(type) {
			case provider.TextBlock:
				content = append(content, map[string]any{"type": "text", "text": v.Text})
			}
		}
		if len(content) > 0 {
			emit("assistant_message", map[string]any{"content": content, "time": e.Time})
		}
		for _, block := range msg.Content {
			if v, ok := block.(provider.ToolCallBlock); ok {
				emit("tool_call", map[string]any{"id": v.ID, "name": v.Name, "args": v.Arguments})
			}
		}
	case "tool_result":
		msg, err := core.DecodeMessage(e.Message)
		if err != nil {
			return
		}
		for _, block := range msg.Content {
			if tr, ok := block.(provider.ToolResultBlock); ok {
				var content []map[string]any
				for _, block := range tr.Content {
					switch v := block.(type) {
					case provider.TextBlock:
						content = append(content, map[string]any{"type": "text", "text": v.Text})
					case provider.ImageBlock:
						content = append(content, map[string]any{"type": "image", "mime_type": v.MimeType, "bytes": len(v.Data)})
					}
				}
				emit("tool_result", map[string]any{"id": tr.CallID, "is_error": tr.IsError, "content": content})
			}
		}
	}
}
