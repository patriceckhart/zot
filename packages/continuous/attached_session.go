package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// AttachedSession owns one conversation watch for the lifetime of a view.
// Observe is the sole writer of the mirrored transcript and event stream.
// PromptWithImages only admits input and waits for Observe to render settlement.
// Cancelling either a prompt or the view never cancels admitted host work.
type AttachedSession struct {
	driver   *AttachedDriver
	agent    *core.Agent
	ctx      context.Context
	cancel   context.CancelFunc
	watch    <-chan storage.Commit
	snapshot ConversationSnapshot
	ready    chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	started  bool
	err      error
	pending  map[string]*attachedPending
}

type attachedPending struct {
	state *waitState
	done  chan error
}

// NewAttachedSession loads committed history and opens its watch from that
// same revision, closing the snapshot/subscription race before the TUI starts.
func NewAttachedSession(ctx context.Context, driver *AttachedDriver, agent *core.Agent) (*AttachedSession, error) {
	if agent == nil {
		agent = core.NewAgent(nil, "", "", core.NewRegistry())
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &AttachedSession{driver: driver, agent: agent, ctx: ctx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), pending: map[string]*attachedPending{}}
	var err error
	s.snapshot, err = driver.Load(ctx, agent)
	if err == nil {
		s.watch, err = driver.Client.Watch(ctx, driver.ConversationID, s.snapshot.Revision)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

// Close detaches the view. The client connection is owned by the caller.
func (s *AttachedSession) Close() { s.cancel() }

// HistoryNotice describes the initial history page.
func (s *AttachedSession) HistoryNotice() string {
	return HistoryNotice(s.snapshot, s.driver.ConversationID)
}

// Observe runs until the view closes or the connection fails. A dropped watch
// is resnapshotted and reopened. reset clears transient UI overlays after a
// snapshot replacement, without replaying historical event side effects.
// Call Observe exactly once, with a non-nil sink and reset callback.
func (s *AttachedSession) Observe(ctx context.Context, sink func(core.AgentEvent), reset func()) (err error) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("attached session is already observed")
	}
	s.started = true
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()
	defer s.cancel()
	defer func() {
		s.mu.Lock()
		s.err = err
		close(s.done)
		s.mu.Unlock()
	}()
	entries := append([]Entry(nil), s.snapshot.Entries...)
	last := s.snapshot.Revision
	streamed := ""
	progress := map[string]string{}
	restore := func(snap ConversationSnapshot) {
		reset()
		streamed = ""
		progress = map[string]string{}
		if p := snap.Partial; p != nil && !p.Final && p.Text != "" {
			streamed = p.Text
			sink(core.EvAssistantStart{})
			sink(core.EvTextDelta{Delta: p.Text})
		}
	}
	restore(s.snapshot)
	close(s.ready)
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case cm, ok := <-s.watch:
			if !ok {
				// Use a new snapshot for both expired cursors and slow consumers.
				snap, err := s.driver.Load(s.ctx, s.agent)
				if err != nil {
					return fmt.Errorf("refresh attached conversation: %w", err)
				}
				entries, last = append([]Entry(nil), snap.Entries...), snap.Revision
				restore(snap)
				s.watch, err = s.driver.Client.Watch(s.ctx, s.driver.ConversationID, last)
				if err != nil {
					return fmt.Errorf("resume attached conversation watch: %w", err)
				}
				// Settlement may have been among the dropped frames. Read each
				// pending submission after restoring its committed transcript.
				s.mu.Lock()
				for id, p := range s.pending {
					var sub Submission
					if err := s.driver.Client.CallInto(s.ctx, "submission.get", map[string]any{"id": id}, &sub); err != nil {
						s.mu.Unlock()
						return err
					}
					// The submission read can be newer than our snapshot. Refresh
					// again before releasing a waiter whose settlement was missed.
					if sub.State != "queued" && sub.State != "running" {
						var latest ConversationSnapshot
						if err := s.driver.Client.CallInto(s.ctx, "conversation.snapshot", map[string]any{"id": s.driver.ConversationID, "limit": 1000}, &latest); err != nil {
							s.mu.Unlock()
							return err
						}
						entries, last = append([]Entry(nil), latest.Entries...), latest.Revision
						s.agent.SetMessages(MessagesFromEntries(entries))
						restore(latest)
						p.done <- attachedSubmissionError(sub.State, latest.Run)
						delete(s.pending, id)
					} else {
						p.state = newWaitState(sub, snap)
						s.driver.status(p.state.describe())
					}
				}
				s.mu.Unlock()
				continue
			}
			if cm.Revision <= last {
				continue
			}
			last = cm.Revision
			fresh := EntriesFromCommit(cm, s.driver.ConversationID)
			if len(fresh) > 0 {
				entries = append(entries, fresh...)
				s.agent.SetMessages(MessagesFromEntries(entries))
				for _, e := range fresh {
					if e.Type == "user" || e.Type == entrySteer || e.Type == entryContinue {
						sink(core.EvUserMessage{Message: userEntryMessage(e)})
					}
					if e.Type == entryAssistant {
						streamed = ""
					}
					if e.Type == entryReset {
						streamed = ""
						reset()
					}
				}
				ProjectEntryEvents(fresh, sink)
			}
			if p, ok := PartialFromCommit(cm, s.driver.ConversationID); ok && !p.Final && p.RunID != "" {
				if !strings.HasPrefix(p.Text, streamed) {
					streamed = ""
				}
				if delta := strings.TrimPrefix(p.Text, streamed); delta != "" {
					if streamed == "" {
						sink(core.EvAssistantStart{})
					}
					sink(core.EvTextDelta{Delta: delta})
				}
				streamed = p.Text
			}
			for _, p := range ToolProgressFromCommit(cm, s.driver.ConversationID) {
				prev := progress[p.CallID]
				progress[p.CallID] = p.Text
				if delta := strings.TrimPrefix(p.Text, prev); delta != "" {
					sink(core.EvToolProgress{ID: p.CallID, Text: delta})
				}
			}
			run, settled := RunFromCommit(cm, s.driver.ConversationID)
			if settled && run.Phase == "done" {
				streamed = ""
				progress = map[string]string{}
				if run.Outcome == "failed" || run.Outcome == "aborted" {
					reset()
				}
				sink(core.EvTurnEnd{Stop: provider.StopEnd})
				sink(core.EvDone{})
			}
			// Release prompt waiters only after their settlement was rendered.
			s.mu.Lock()
			for id, p := range s.pending {
				if p.state.apply(cm, s.driver.ConversationID) {
					s.driver.status(p.state.describe())
				}
				for _, op := range cm.Operations {
					if op.Delete || op.Key != "submission/"+id {
						continue
					}
					var sub Submission
					if json.Unmarshal(op.Value, &sub) == nil && sub.State != "queued" && sub.State != "running" {
						p.done <- attachedSubmissionError(sub.State, &run)
						delete(s.pending, id)
					}
				}
			}
			s.mu.Unlock()
		}
	}
}

func attachedSubmissionError(state string, run *Run) error {
	switch state {
	case "failed":
		if run != nil && run.Error != "" {
			return errors.New(run.Error)
		}
		return errors.New("run failed on the host")
	case "aborted", "withdrawn":
		return fmt.Errorf("submission %s on the host", state)
	}
	return nil
}

// PromptWithImages submits through the lifetime watch, never projecting a
// second copy of the prompt or assistant events. Its sink belongs to Observe.
func (s *AttachedSession) PromptWithImages(ctx context.Context, _ *core.Agent, prompt string, images []provider.ImageBlock, _ func(core.AgentEvent)) error {
	// Ending the view must also unblock any admission RPC, otherwise Observe
	// could wait on the submission mutex while the TUI waits for Observe to exit.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	select {
	case <-s.ready:
	case <-s.done:
		return s.observationError()
	case <-s.ctx.Done():
		return ErrDetached
	case <-ctx.Done():
		return ErrDetached
	}
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return ErrDetached
	}
	if len(images) > 0 {
		status, err := s.driver.Client.Call(ctx, "runtime.status", nil)
		if err != nil {
			return err
		}
		if err := RequireAttachmentSupport(status); err != nil {
			return err
		}
	}
	var snap ConversationSnapshot
	if err := s.driver.Client.CallInto(ctx, "conversation.snapshot", map[string]any{"id": s.driver.ConversationID, "limit": 1}, &snap); err != nil {
		if ctx.Err() != nil {
			return ErrDetached
		}
		return err
	}
	// Register the waiter before Observe can process admission or settlement.
	s.mu.Lock()
	if ctx.Err() != nil || s.ctx.Err() != nil {
		s.mu.Unlock()
		return ErrDetached
	}
	select {
	case <-s.done:
		err := s.err
		s.mu.Unlock()
		return err
	default:
	}
	var sub Submission
	err := s.driver.Client.CallInto(ctx, "conversation.submit", map[string]any{"id": s.driver.ConversationID, "content": prompt, "images": images, "request_id": "tui-" + uuid.NewString()}, &sub)
	if err != nil {
		s.mu.Unlock()
		if ctx.Err() != nil {
			return ErrDetached
		}
		return err
	}
	p := &attachedPending{state: newWaitState(sub, snap), done: make(chan error, 1)}
	s.pending[sub.ID] = p
	s.driver.status(p.state.describe())
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, sub.ID)
		s.driver.status("")
		s.mu.Unlock()
	}()
	select {
	case err := <-p.done:
		return err
	case <-s.done:
		return s.observationError()
	case <-ctx.Done():
		return ErrDetached
	case <-s.ctx.Done():
		return ErrDetached
	}
}

func (s *AttachedSession) observationError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
