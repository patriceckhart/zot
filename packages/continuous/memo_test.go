package continuous

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// A memoized decision survives a crash and re-execution: the second attempt
// reads the first attempt's value instead of deciding again.
func TestMemoFirstWriteWinsAcrossReexecution(t *testing.T) {
	ctx := context.Background()
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m1, created, err := r.Memoize(ctx, "run/r1", "ticket", "hook", "TICKET-001")
	if err != nil || !created || string(m1.Value) != `"TICKET-001"` {
		t.Fatalf("first write: %+v %v %v", m1, created, err)
	}
	m2, created, err := r.Memoize(ctx, "run/r1", "ticket", "hook", "TICKET-002")
	if err != nil || created || string(m2.Value) != `"TICKET-001"` {
		t.Fatalf("second write must lose: %+v %v %v", m2, created, err)
	}
	if _, _, err := r.Memoize(ctx, "", "x", "", 1); err == nil {
		t.Fatal("empty scope accepted")
	}
	if _, ok, _ := r.Memo(ctx, "run/r1", "other"); ok {
		t.Fatal("missing memo found")
	}
	// Inside a durable tool call the scope is the run.
	id := ToolCallIdentity{RunID: "r9", ConversationID: "c", CallID: "x"}
	scope, ok := MemoScope(context.WithValue(ctx, toolCallKey{}, id))
	if !ok || scope != "run/r9" {
		t.Fatalf("scope: %q %v", scope, ok)
	}
	if _, ok := MemoScope(ctx); ok {
		t.Fatal("scope outside a run")
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// Every attempt records the request shape; identical prompts share one
// section record, and a changed system prompt produces a new hash.
func TestPromptRecordsExplainAttempts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "one"}, {text: "two"}})
	defer h.r.Close()
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "a", "", "first")
	h.svc.Step(ctx, c.ID)
	records, err := h.r.PromptRecords(ctx, c.ID, 10)
	if err != nil || len(records) != 1 || records[0].Turn != 1 || records[0].Attempt != 1 || len(records[0].ToolNames) != 0 {
		t.Fatalf("records: %+v %v", records, err)
	}
	section, ok, _ := h.r.PromptSection(ctx, records[0].SystemHash)
	if !ok || !strings.Contains(section.Text, "You are synthetic.") || !strings.Contains(section.Text, "Answer briefly.") {
		t.Fatalf("system section: %+v %v", section, ok)
	}
	// Changing the instructions changes the hash; the old section stays.
	cur, _ := h.r.Conversation(ctx, c.ID)
	h.r.Configure(ctx, c.ID, cur.Revision, AgentConfig{Provider: "synthetic", Model: "scripted", Instructions: "Be verbose."})
	h.r.Submit(ctx, c.ID, "a", "", "second")
	h.svc.Step(ctx, c.ID)
	records, _ = h.r.PromptRecords(ctx, c.ID, 10)
	if len(records) != 2 || records[0].SystemHash == records[1].SystemHash {
		t.Fatalf("records after change: %+v", records)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// Reload publishes a new engine generation atomically: a failing engine is
// rejected and the old one stays active; an in-flight call finishes under
// the generation it started with; new calls use the new one.
func TestServiceReloadGenerations(t *testing.T) {
	ctx := context.Background()
	oldTool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	newTool := &effectTool{name: "effect"}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{calls: []provider.ToolCallBlock{call("c2", "effect", `{}`)}},
		{text: "done"},
	}, oldTool)
	defer h.r.Close()
	if h.svc.Generation() != 1 {
		t.Fatalf("initial generation %d", h.svc.Generation())
	}
	// A broken replacement is rejected; generation unchanged.
	bad := EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		return nil, errors.New("extension failed to start")
	})
	if _, err := h.svc.Reload(ctx, bad); err == nil || h.svc.Generation() != 1 {
		t.Fatalf("broken reload accepted: %v gen=%d", err, h.svc.Generation())
	}
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "a", "", "go")
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.Step(ctx, c.ID); done <- err }()
	<-oldTool.started
	// Reload while c1 runs under generation 1.
	gen, err := h.svc.Reload(ctx, EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(h.client, "scripted", "", core.NewRegistry(newTool))
		a.MaxRetries = 0
		return a, nil
	}))
	if err != nil || gen != 2 {
		t.Fatalf("reload: gen=%d %v", gen, err)
	}
	close(oldTool.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// c1 ran on the old tool, c2 (a new request after the reload) on the new one.
	if oldTool.calls.Load() != 1 || newTool.calls.Load() != 1 {
		t.Fatalf("old=%d new=%d", oldTool.calls.Load(), newTool.calls.Load())
	}
}
