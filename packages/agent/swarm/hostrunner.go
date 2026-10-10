package swarm

import (
	"context"
	"crypto/rand"
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
func (r *HostRunner) Run(ctx context.Context, sink Sink) error {
	if r.Dial == nil || r.agent == nil {
		return errors.New("swarm: host runner requires a dial function and an agent")
	}
	client, err := r.Dial(ctx)
	if err != nil {
		return fmt.Errorf("swarm: connect to host: %w", err)
	}
	defer client.Close()
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
	seen := map[string]bool{}
	for _, e := range snap.Entries {
		seen[e.ID] = true
	}
	if !r.agent.Resuming {
		r.mirrorEntries(snap.Entries, sink, nil)
	}
	watch, err := client.Watch(ctx, child.ID, snap.Revision)
	if err != nil {
		return fmt.Errorf("swarm: watch child: %w", err)
	}

	// Follow-up request IDs are unique per supervisor incarnation: a resumed
	// agent must not collide with the IDs an earlier incarnation used.
	incarnation := rand.Text()
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
		r.mu.Lock()
		r.sendSeq++
		requestID := fmt.Sprintf("swarm/%s/%s/%d", r.agent.ID, incarnation, r.sendSeq)
		r.mu.Unlock()
		var sub continuous.Submission
		params := map[string]any{"id": child.ID, "content": prompt.Text, "request_id": requestID}
		if len(prompt.Images) > 0 {
			params["images"] = prompt.Images
		}
		if err := client.CallInto(ctx, "conversation.submit", params, &sub); err != nil {
			return err
		}
		sink.Activity("queued")
		return nil
	}
	if !r.agent.Resuming && (r.agent.Task != "" || len(r.agent.Images) > 0) {
		if err := requireImages(r.agent.Images); err != nil {
			return fmt.Errorf("swarm: task images: %w", err)
		}
		// The first submission reuses a stable request ID so a re-run of
		// the same agent ID does not queue the task twice.
		var sub continuous.Submission
		params := map[string]any{"id": child.ID, "content": r.agent.Task, "request_id": "swarm/" + r.agent.ID + "/task"}
		if len(r.agent.Images) > 0 {
			params["images"] = r.agent.Images
		}
		if err := client.CallInto(ctx, "conversation.submit", params, &sub); err != nil {
			return fmt.Errorf("swarm: submit task: %w", err)
		}
		sink.Activity("queued")
	}

	inputs := r.inputChannel()
	activeRun := ""
	if snap.Run != nil && snap.Run.Phase != "done" {
		activeRun = snap.Run.ID
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
			emit("agent_stopped", map[string]any{"reason": "exit", "code": 1, "error": "host connection closed", "conversation": child.ID})
			return errors.New("swarm: host connection closed")
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
					err = submit(prompt)
				}
				if err != nil {
					sink.Transcript("error: submit: " + err.Error())
					emit("error", map[string]any{"message": "submit: " + err.Error()})
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
				r.mirrorEntries(snap.Entries, sink, seen)
				if watch, err = client.Watch(ctx, child.ID, snap.Revision); err != nil {
					return fmt.Errorf("swarm: rewatch child: %w", err)
				}
				continue
			}
			entries := continuous.EntriesFromCommit(cm, child.ID)
			r.mirrorEntries(entries, sink, seen)
			for _, e := range entries {
				emitEntry(emit, e)
			}
			if run, ok := continuous.RunFromCommit(cm, child.ID); ok {
				switch {
				case run.Phase == "done" && run.ID == activeRun:
					activeRun = ""
					errMsg := ""
					if run.Outcome != "completed" {
						errMsg = strings.TrimSuffix(run.Outcome+": "+run.Error, ": ")
					}
					emit("turn_end", map[string]any{"step": run.Turn, "error": errMsg, "outcome": run.Outcome})
					notifyPromptTurnEnd(r.agent, Event{Type: "turn_end", Time: time.Now(), Data: map[string]any{"step": float64(run.Turn), "error": errMsg}})
					if errMsg != "" {
						sink.Transcript("error: " + errMsg)
					}
					sink.Activity("idle")
				case run.Phase != "done" && run.ID != activeRun:
					activeRun = run.ID
					emit("turn_start", map[string]any{"step": run.Turn})
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
		switch e.Type {
		case "user", "steer":
			if roles, ok := sink.(interface{ userMessage(string) }); ok {
				roles.userMessage(e.Content)
			} else {
				sink.Transcript("user: " + e.Content)
			}
			sink.Activity("thinking")
		case "assistant":
			msg, err := core.DecodeMessage(e.Message)
			if err != nil {
				continue
			}
			for _, block := range msg.Content {
				if tc, ok := block.(provider.ToolCallBlock); ok {
					sink.Activity("tool: " + truncate(tc.Name, 60))
				}
			}
			if text := core.MessageText(msg); text != "" {
				if roles, ok := sink.(interface{ assistantMessage(string) }); ok {
					roles.assistantMessage(text)
				} else {
					sink.Transcript(text)
				}
			}
		case "reset":
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
			case provider.ToolCallBlock:
				emit("tool_call", map[string]any{"id": v.ID, "name": v.Name})
			}
		}
		if len(content) > 0 {
			emit("assistant_message", map[string]any{"content": content, "time": e.Time})
		}
	case "tool_result":
		msg, err := core.DecodeMessage(e.Message)
		if err != nil {
			return
		}
		for _, block := range msg.Content {
			if tr, ok := block.(provider.ToolResultBlock); ok {
				emit("tool_result", map[string]any{"id": tr.CallID, "is_error": tr.IsError})
			}
		}
	}
}
