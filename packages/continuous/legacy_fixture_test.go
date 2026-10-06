package continuous

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// legacyIntent describes one tool intent of a fixture run.
type legacyIntent struct {
	call   provider.ToolCallBlock
	state  string // pending, running, or done
	replay core.ToolReplayPolicy
}

// writeLegacyRun commits the records an earlier build left for an
// interrupted run of the run-based executor: the queued submissions claimed
// into a run in the given phase and, for a tools phase, the assistant entry
// and its intents. Done intents get their result entries. Nothing executes.
func writeLegacyRun(t *testing.T, r *Runtime, conversationID, phase string, intents ...legacyIntent) Run {
	t.Helper()
	ctx := context.Background()
	snap, err := r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := conversation(snap, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: uuid.NewString(), ConversationID: c.ID, Phase: phase, Turn: 1, Attempt: 1, Cutoff: c.EntrySequence}
	var ops []storage.Operation
	queue, _ := snap.Page("queue/"+c.ID+"/", "", 100)
	for _, row := range queue {
		var id string
		json.Unmarshal(row.Value, &id)
		sub, _, _ := read[Submission](snap, "submission/"+id)
		sub.State = "running"
		run.Submissions = append(run.Submissions, id)
		ops = append(ops, record("submission/"+id, sub), storage.Operation{Key: row.Key, Delete: true})
	}
	if len(run.Submissions) == 0 {
		t.Fatal("fixture needs a queued submission")
	}
	if phase == "tools" {
		msg := provider.Message{Role: provider.RoleAssistant, Time: time.Now().UTC()}
		for _, in := range intents {
			msg.Content = append(msg.Content, in.call)
		}
		c.EntrySequence++
		ops = append(ops, record(entryKey(c.ID, c.EntrySequence), Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryAssistant, Message: marshalMessage(msg), Time: time.Now().UTC()}))
		run.Cutoff = c.EntrySequence
		for _, in := range intents {
			intent := ToolIntent{CallID: in.call.ID, Name: in.call.Name, State: in.state}
			switch in.state {
			case "running":
				intent.Args, intent.Replay = in.call.Arguments, in.replay
				if intent.Replay == "" {
					intent.Replay = core.ReplayNever
				}
			case "done":
				c.EntrySequence++
				block := provider.ToolResultBlock{CallID: in.call.ID, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}
				ops = append(ops, record(entryKey(c.ID, c.EntrySequence), Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryToolResult, Content: "ok", Message: marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}}), Time: time.Now().UTC()}))
				intent.Args, intent.Replay, intent.Entry = in.call.Arguments, core.ReplayNever, c.EntrySequence
			}
			run.Tools = append(run.Tools, intent)
		}
	}
	c.Revision = snap.Revision() + 1
	run.Revision = c.Revision
	ops = append(ops, record("conversation/"+c.ID, c), record(runKey(c.ID), run))
	if err := r.commit(ctx, snap, "legacy.fixture", ops...); err != nil {
		t.Fatal(err)
	}
	return run
}

// queueOnly admits a submission without starting a chain, as an earlier
// build did while a run was active.
func queueOnly(t *testing.T, r *Runtime, conversationID, content string) Submission {
	t.Helper()
	ctx := context.Background()
	snap, _ := r.Snapshot(ctx)
	c, err := conversation(snap, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	s, ops, err := admissionOps(snap, &c, "actor", "", content, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.commit(ctx, snap, "legacy.queue", append(ops, record("conversation/"+c.ID, c))...); err != nil {
		t.Fatal(err)
	}
	return s
}
