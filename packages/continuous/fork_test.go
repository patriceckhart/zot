package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestForkSharesHistoryWithoutCopying(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "parent answer"},
		{text: "child answer"},
		{text: "parent continues"},
	}, &effectTool{name: "effect"})
	defer h.r.Close()
	parent := h.root(t)
	h.r.Submit(ctx, parent.ID, "actor", "", "first")
	if run, _, err := h.svc.Step(ctx, parent.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("parent run: %+v %v", run, err)
	}
	// Entries: user(1) assistant(2, tool call) tool_result(3) assistant(4).
	cur, _ := h.r.Conversation(ctx, parent.ID)
	if cur.EntrySequence != 4 {
		t.Fatalf("parent entries: %d", cur.EntrySequence)
	}
	for _, at := range []uint64{0, 5} {
		if _, err := h.r.Fork(ctx, parent.ID, at, nil); !errors.Is(err, ErrInvalidFork) {
			t.Fatalf("fork at %d: %v", at, err)
		}
	}
	if _, err := h.r.Fork(ctx, parent.ID, 2, nil); !errors.Is(err, ErrInvalidFork) {
		t.Fatalf("fork inside a tool round: %v", err)
	}
	child, err := h.r.Fork(ctx, parent.ID, 3, &AgentConfig{Provider: "synthetic", Model: "other", Instructions: "You are the fork."})
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent == nil || child.Parent.ConversationID != parent.ID || child.Parent.At != 3 || child.Config.Model != "other" || child.EntrySequence != 0 {
		t.Fatalf("child: %+v", child)
	}
	// No entries were copied.
	if got := len(h.entries(t, child.ID)); got != 0 {
		t.Fatalf("copied %d entries", got)
	}
	h.r.Submit(ctx, child.ID, "actor", "", "child question")
	if run, _, err := h.svc.Step(ctx, child.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("child run: %+v %v", run, err)
	}
	// The child's request saw parent entries 1..3 and its own input, not the
	// parent's final answer (entry 4).
	req := h.client.requests[2]
	if len(req.Messages) != 4 || req.Messages[0].Role != provider.RoleUser || req.Messages[2].Role != provider.RoleTool || core.MessageText(req.Messages[3]) != "child question" {
		t.Fatalf("child context: %+v", req.Messages)
	}
	if req.Model != "other" || !strings.Contains(req.System, "You are the fork.") || req.SessionID != child.ID {
		t.Fatalf("child configuration: %q %q %q", req.Model, req.System, req.SessionID)
	}
	// The parent is untouched and continues on its own history.
	after, _ := h.r.Conversation(ctx, parent.ID)
	if after.EntrySequence != 4 || after.Revision != cur.Revision {
		t.Fatalf("parent changed by fork: before=%+v after=%+v", cur, after)
	}
	h.r.Submit(ctx, parent.ID, "actor", "", "second")
	if run, _, err := h.svc.Step(ctx, parent.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("parent second run: %+v %v", run, err)
	}
	last := h.client.requests[3]
	for _, m := range last.Messages {
		if strings.Contains(core.MessageText(m), "child") {
			t.Fatalf("child entries leaked into parent: %+v", last.Messages)
		}
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid || report.Conversations != 2 {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	// Export of the fork stands alone with inherited history.
	var exported strings.Builder
	if err := h.r.ExportSession(ctx, child.ID, &exported); err != nil {
		t.Fatal(err)
	}
	if strings.Count(exported.String(), `"type":"message"`) != 5 || !strings.Contains(exported.String(), "child answer") || strings.Contains(exported.String(), "parent answer") {
		t.Fatalf("fork export: %s", exported.String())
	}
	// Grandchild chains through two parents.
	grandchild, err := h.r.Fork(ctx, child.ID, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	snap, _ := h.r.Snapshot(ctx)
	if err := History(ctx, snap, grandchild.ID, 0, func(owner string, seq uint64, e Entry) error {
		seen = append(seen, owner[:4]+e.Type)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 5 || !strings.HasPrefix(seen[0], parent.ID[:4]) || !strings.HasPrefix(seen[3], child.ID[:4]) {
		t.Fatalf("grandchild history: %v", seen)
	}
}

func TestForkRejectedWhileParentBusy(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "never"}})
	defer h.r.Close()
	parent := h.root(t)
	h.r.Submit(ctx, parent.ID, "actor", "", "first")
	if _, _, err := h.svc.start(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Fork(ctx, parent.ID, 1, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("fork during run: %v", err)
	}
	if _, err := h.r.Reset(ctx, parent.ID, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("reset during run: %v", err)
	}
}

func TestResetStartsNewContextAndKeepsHistory(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "first answer"}, {text: "after reset"}})
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "actor", "", "remember the number 42")
	if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("first run: %+v %v", run, err)
	}
	reset, err := h.r.Reset(ctx, c.ID, "We were discussing 42. Continue.")
	if err != nil || reset.EntrySequence != 3 {
		t.Fatalf("reset: %+v %v", reset, err)
	}
	h.r.Submit(ctx, c.ID, "actor", "", "what number")
	if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
		t.Fatalf("second run: %+v %v", run, err)
	}
	req := h.client.requests[1]
	if len(req.Messages) != 2 || core.MessageText(req.Messages[0]) != "We were discussing 42. Continue." || core.MessageText(req.Messages[1]) != "what number" {
		t.Fatalf("context after reset: %+v", req.Messages)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant reset user assistant" {
		t.Fatalf("history not retained: %s", got)
	}
	var exported strings.Builder
	if err := h.r.ExportSession(ctx, c.ID, &exported); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exported.String(), `"type":"compaction"`) || !strings.Contains(exported.String(), "remember the number 42") {
		t.Fatalf("export: %s", exported.String())
	}
	// A legacy reader of the export keeps only the post-reset context.
	msgs, err := legacyMessages(exported.String())
	if err != nil || len(msgs) != 3 || core.MessageText(msgs[0]) != "We were discussing 42. Continue." {
		t.Fatalf("legacy view: %+v %v", msgs, err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	// Empty reset: next context starts empty.
	if _, err := h.r.Reset(ctx, c.ID, "  "); err != nil {
		t.Fatal(err)
	}
	snap, _ := h.r.Snapshot(ctx)
	cur, _ := h.r.Conversation(ctx, c.ID)
	msgs2, err := ModelContext(ctx, snap, c.ID, cur.EntrySequence)
	if err != nil || len(msgs2) != 0 {
		t.Fatalf("empty reset context: %+v %v", msgs2, err)
	}
}

// legacyMessages reads an exported JSONL projection the way the legacy
// session loader does: compaction rows replace the accumulated messages.
func legacyMessages(jsonl string) ([]provider.Message, error) {
	var out []provider.Message
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		var head struct {
			Type     string            `json:"type"`
			Message  json.RawMessage   `json:"message"`
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			return nil, err
		}
		switch head.Type {
		case "message":
			m, err := core.DecodeMessage(head.Message)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		case "compaction":
			out = out[:0]
			for _, raw := range head.Messages {
				m, err := core.DecodeMessage(raw)
				if err != nil {
					return nil, err
				}
				out = append(out, m)
			}
		}
	}
	return out, nil
}
