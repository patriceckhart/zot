package continuous

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// AttachedDriver drives an interactive view against a remote host. Prompts
// are submitted to the host; committed entries are watched and projected as
// agent events; the agent's transcript mirrors the committed model context.
// Cancelling a prompt's context detaches the view: the host keeps working,
// and the next prompt or reattach shows what happened.
type AttachedDriver struct {
	Client         *Client
	ConversationID string
	// OnStatus, when set, receives a short description of why the
	// followed submission is not producing output ("queued", "awaiting
	// approval: bash", "recovery blocked: ..."), and "" when it is
	// running normally or has settled.
	OnStatus func(string)
}

func (d *AttachedDriver) status(s string) {
	if d.OnStatus != nil {
		d.OnStatus(s)
	}
}

// Load replaces the agent's transcript with the host's committed context and
// returns the snapshot revision to watch from.
func (d *AttachedDriver) Load(ctx context.Context, agent *core.Agent) (ConversationSnapshot, error) {
	var snap ConversationSnapshot
	if err := d.Client.CallInto(ctx, "conversation.snapshot", map[string]any{"id": d.ConversationID, "limit": 1000}, &snap); err != nil {
		return snap, err
	}
	agent.SetMessages(MessagesFromEntries(snap.Entries))
	return snap, nil
}

// MessagesFromEntries projects a conversation's own entries (as returned by a
// snapshot) into the display transcript: user and steer entries, assistant
// messages, tool results merged per round, and resets that clear the view.
// Attempts and compactions are not display messages.
func MessagesFromEntries(entries []Entry) []provider.Message {
	var messages []provider.Message
	var pendingTool *provider.Message
	flushTool := func() {
		if pendingTool != nil {
			messages = append(messages, *pendingTool)
			pendingTool = nil
		}
	}
	for _, e := range entries {
		switch e.Type {
		case "user", entrySteer, entryContinue:
			flushTool()
			messages = append(messages, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: e.Content}}, Time: e.Time})
		case entryAssistant:
			flushTool()
			if msg, err := core.DecodeMessage(e.Message); err == nil {
				messages = append(messages, msg)
			}
		case entryToolResult:
			if msg, err := core.DecodeMessage(e.Message); err == nil {
				if pendingTool == nil {
					m := msg
					pendingTool = &m
				} else {
					pendingTool.Content = append(pendingTool.Content, msg.Content...)
				}
			}
		case entryReset:
			flushTool()
			messages = messages[:0]
			if e.Content != "" {
				messages = append(messages, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: e.Content}}, Time: e.Time})
			}
		}
	}
	flushTool()
	return provider.RepairOrphanedToolResults(orderToolResults(messages))
}

// Prompt is the PromptDriver: submit, then watch commits until the run that
// claimed the submission settles. Events are projected from committed
// entries, so the view shows exactly what the host recorded.
func (d *AttachedDriver) Prompt(ctx context.Context, agent *core.Agent, prompt string, sink func(core.AgentEvent)) error {
	err := d.prompt(ctx, agent, prompt, sink)
	if err != nil && ctx.Err() != nil && !errors.Is(err, ErrDetached) {
		// The view's context ended mid-call: whatever the host has is still
		// its work, not a failure of the prompt.
		return ErrDetached
	}
	return err
}

func (d *AttachedDriver) prompt(ctx context.Context, agent *core.Agent, prompt string, sink func(core.AgentEvent)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var snap ConversationSnapshot
	if err := d.Client.CallInto(ctx, "conversation.snapshot", map[string]any{"id": d.ConversationID, "limit": 1}, &snap); err != nil {
		return err
	}
	watch, err := d.Client.Watch(ctx, d.ConversationID, snap.Revision)
	if err != nil {
		return err
	}
	var sub Submission
	requestID := "tui-" + uuid.NewString()
	if err := d.Client.CallInto(ctx, "conversation.submit", map[string]any{"id": d.ConversationID, "content": prompt, "request_id": requestID}, &sub); err != nil {
		return err
	}
	user := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: prompt}}}
	agent.AppendUserContext(prompt, nil)
	sink(core.EvUserMessage{Message: user})
	ws := newWaitState(sub, snap)
	d.status(ws.describe())
	defer d.status("")
	seen := map[string]bool{}
	streamed := ""
	// progress is the last committed tool output per call, so only the
	// new tail is rendered.
	progress := map[string]string{}
	// last is the newest revision applied to the view; a dropped watch
	// resumes after it so no commit is shown twice or skipped.
	last := snap.Revision
	for {
		select {
		case <-ctx.Done():
			return ErrDetached
		case cm, ok := <-watch:
			if !ok {
				if ctx.Err() != nil {
					return ErrDetached
				}
				// Watch dropped (slow consumer or host limit). The
				// submission is never resent: follow it again.
				var s Submission
				if err := d.Client.CallInto(ctx, "submission.get", map[string]any{"id": sub.ID}, &s); err != nil {
					if ctx.Err() != nil {
						return ErrDetached
					}
					return err
				}
				if s.State == "queued" || s.State == "running" {
					if watch, err = d.rewatch(ctx, agent, &last, seen); err != nil {
						if ctx.Err() != nil {
							return ErrDetached
						}
						return fmt.Errorf("resume watch while the submission is %s: %w", s.State, err)
					}
					// The run may have settled before the new watch opened;
					// anything later arrives on the watch.
					if err := d.Client.CallInto(ctx, "submission.get", map[string]any{"id": sub.ID}, &s); err != nil {
						if ctx.Err() != nil {
							return ErrDetached
						}
						return err
					}
					if s.State == "queued" || s.State == "running" {
						continue
					}
				}
				d.Load(context.Background(), agent)
				sink(core.EvDone{})
				return nil
			}
			last = cm.Revision
			if ws.apply(cm, d.ConversationID) {
				d.status(ws.describe())
			}
			for _, p := range ToolProgressFromCommit(cm, d.ConversationID) {
				prev := progress[p.CallID]
				progress[p.CallID] = p.Text
				delta := p.Text
				if strings.HasPrefix(p.Text, prev) {
					delta = p.Text[len(prev):]
				}
				if delta != "" {
					sink(core.EvToolProgress{ID: p.CallID, Text: delta})
				}
			}
			// Streamed text arrives as partial records; render the delta since
			// the last one. A completed attempt clears the record and the
			// assistant entry replaces the streamed text in the same commit.
			if p, ok := PartialFromCommit(cm, d.ConversationID); ok && !p.Final && p.RunID != "" {
				if strings.HasPrefix(p.Text, streamed) {
					if delta := p.Text[len(streamed):]; delta != "" {
						if streamed == "" {
							sink(core.EvAssistantStart{})
						}
						sink(core.EvTextDelta{Delta: delta})
					}
				}
				streamed = p.Text
			}
			entries := EntriesFromCommit(cm, d.ConversationID)
			var fresh []Entry
			for _, e := range entries {
				if e.SubmissionID == sub.ID && e.Type == "user" {
					continue
				}
				if !seen[e.ID] {
					seen[e.ID] = true
					fresh = append(fresh, e)
				}
			}
			for _, e := range fresh {
				if e.Type == entryAssistant {
					// The streamed text is now committed; the message event
					// below replaces it in the view.
					streamed = ""
				}
			}
			ProjectEntryEvents(fresh, sink)
			for _, e := range fresh {
				switch e.Type {
				case entryAssistant:
					if msg, err := core.DecodeMessage(e.Message); err == nil {
						agent.SetMessages(append(agent.Messages(), msg))
					}
				case entryToolResult:
					if msg, err := core.DecodeMessage(e.Message); err == nil {
						agent.SetMessages(append(agent.Messages(), msg))
					}
				case entryReset:
					agent.SetMessages(nil)
					if e.Content != "" {
						agent.AppendUserContext(e.Content, map[string]string{"handoff": "true"})
					}
				}
			}
			if run, ok := RunFromCommit(cm, d.ConversationID); ok && run.Phase == "done" {
				for _, id := range run.Submissions {
					if id == sub.ID {
						sink(core.EvTurnEnd{Stop: provider.StopEnd})
						sink(core.EvDone{})
						switch run.Outcome {
						case "failed":
							return errors.New(run.Error)
						case "aborted":
							return errors.New("run aborted on the host")
						}
						return nil
					}
				}
			}
		}
	}
}

// rewatch reopens a dropped watch after the last applied revision. When the
// host no longer retains that history, the view is reloaded from a fresh
// snapshot and watched from there; entries already in it are marked seen.
func (d *AttachedDriver) rewatch(ctx context.Context, agent *core.Agent, last *uint64, seen map[string]bool) (<-chan storage.Commit, error) {
	if w, err := d.Client.Watch(ctx, d.ConversationID, *last); err == nil {
		return w, nil
	} else if ctx.Err() != nil {
		return nil, err
	}
	snap, err := d.Load(ctx, agent)
	if err != nil {
		return nil, err
	}
	for _, e := range snap.Entries {
		seen[e.ID] = true
	}
	*last = snap.Revision
	return d.Client.Watch(ctx, d.ConversationID, snap.Revision)
}
