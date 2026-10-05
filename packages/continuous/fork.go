package continuous

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Parent records fork ancestry. A child sees its parent's entries through At
// without copying them. Nothing a child writes touches the parent.
type Parent struct {
	ConversationID string `json:"conversation_id"`
	// At is the parent's entry sequence included in the child's history.
	At uint64 `json:"at"`
}

const entryReset = "reset"

var ErrInvalidFork = errors.New("continuous fork point outside parent history")

// Fork creates a conversation that shares the parent's history through entry
// at. The child starts with the parent's configuration unless config is set,
// has its own queue, run, usage, and entries, and never rewrites the parent.
// Forking is rejected while the parent has an active run, because the
// parent's entries past the fork point are then still changing.
func (r *Runtime) Fork(ctx context.Context, parentID string, at uint64, config *AgentConfig) (Conversation, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		parent, err := conversation(snap, parentID)
		if err != nil {
			return Conversation{}, err
		}
		if at == 0 || at > parent.EntrySequence {
			return Conversation{}, ErrInvalidFork
		}
		if run, ok, err := read[Run](snap, runKey(parentID)); err != nil {
			return Conversation{}, err
		} else if ok && run.Phase != "done" {
			return Conversation{}, ErrBusy
		}
		// The fork point must be a turn boundary: forking between a tool call
		// and its result would hand the child a dangling call.
		e, ok, err := read[Entry](snap, entryKey(parentID, at))
		if err != nil {
			return Conversation{}, err
		}
		if !ok {
			return Conversation{}, fmt.Errorf("%w: missing entry", storage.ErrCorrupt)
		}
		if e.Type == entryAssistant {
			msg, err := core.DecodeMessage(e.Message)
			if err != nil {
				return Conversation{}, fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
			}
			for _, block := range msg.Content {
				if tc, ok := block.(provider.ToolCallBlock); ok && !tc.Server {
					return Conversation{}, fmt.Errorf("%w: fork point is a tool call without its results", ErrInvalidFork)
				}
			}
		}
		cfg := parent.Config
		if config != nil {
			cfg = *config
		}
		c := Conversation{ID: uuid.NewString(), Created: time.Now().UTC(), Revision: snap.Revision() + 1, Config: cfg, Parent: &Parent{ConversationID: parentID, At: at}}
		err = r.commit(ctx, snap, "conversation.fork", record("conversation/"+c.ID, c))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return c, err
	}
}

// Reset appends a context boundary. Entries before it stay stored and
// searchable, but the next model request starts from the boundary. A handoff
// note, when given, becomes the first user message of the new context.
// Reset is rejected while a run is active so no request straddles it.
func (r *Runtime) Reset(ctx context.Context, conversationID, handoff string) (Conversation, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		c, err := conversation(snap, conversationID)
		if err != nil {
			return Conversation{}, err
		}
		if run, ok, err := read[Run](snap, runKey(conversationID)); err != nil {
			return Conversation{}, err
		} else if ok && run.Phase != "done" {
			return Conversation{}, ErrBusy
		}
		if c.EntrySequence == ^uint64(0) {
			return Conversation{}, fmt.Errorf("conversation entry sequence exhausted")
		}
		c.EntrySequence++
		c.Revision = snap.Revision() + 1
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: c.Revision, Type: entryReset, Content: strings.TrimSpace(handoff), Time: time.Now().UTC()}
		err = r.commit(ctx, snap, "conversation.reset", record("conversation/"+c.ID, c), record(entryKey(c.ID, c.EntrySequence), entry))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return c, err
	}
}

// History yields the visible entries of a conversation in order, following
// fork ancestry, through the given sequence of the conversation itself.
// Visit receives the owning conversation ID and sequence of each entry. It
// stops when visit returns an error. A zero cutoff means everything.
func History(ctx context.Context, snap storage.Snapshot, conversationID string, cutoff uint64, visit func(owner string, seq uint64, e Entry) error) error {
	// Collect the chain root-first without recursion so a long fork chain
	// cannot exhaust the stack. Cycles are impossible by construction (a
	// parent always predates its child) but are still rejected.
	type segment struct {
		id  string
		end uint64
	}
	var chain []segment
	seen := map[string]bool{}
	id, end := conversationID, cutoff
	for id != "" {
		if seen[id] {
			return fmt.Errorf("%w: fork ancestry cycle", storage.ErrCorrupt)
		}
		seen[id] = true
		c, err := conversation(snap, id)
		if err != nil {
			return err
		}
		if end == 0 || end > c.EntrySequence {
			end = c.EntrySequence
		}
		chain = append(chain, segment{id: id, end: end})
		if c.Parent == nil {
			break
		}
		id, end = c.Parent.ConversationID, c.Parent.At
	}
	for i := len(chain) - 1; i >= 0; i-- {
		seg := chain[i]
		for seq := uint64(1); seq <= seg.end; seq++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			e, ok, err := read[Entry](snap, entryKey(seg.id, seq))
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: missing entry", storage.ErrCorrupt)
			}
			if err := visit(seg.id, seq, e); err != nil {
				return err
			}
		}
	}
	return nil
}
