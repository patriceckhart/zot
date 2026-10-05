package continuous

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
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
		case "user", entrySteer:
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
	return provider.RepairOrphanedToolResults(messages)
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
	seen := map[string]bool{}
	streamed := ""
	for {
		select {
		case <-ctx.Done():
			return ErrDetached
		case cm, ok := <-watch:
			if !ok {
				if ctx.Err() != nil {
					return ErrDetached
				}
				// Watch dropped: resnapshot and report the settled state.
				var s Submission
				if err := d.Client.CallInto(context.Background(), "submission.get", map[string]any{"id": sub.ID}, &s); err != nil {
					return err
				}
				if s.State == "queued" || s.State == "running" {
					return fmt.Errorf("watch ended while the submission is %s; reattach to continue following", s.State)
				}
				d.Load(context.Background(), agent)
				sink(core.EvDone{})
				return nil
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
