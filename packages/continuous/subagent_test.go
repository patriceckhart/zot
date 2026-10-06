package continuous

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// subagentHarness wires the subagent tool into the harness engine so the
// child conversation is stepped by the same service and scripted client.
func subagentHarness(t *testing.T, store storage.Store, steps []scriptStep, extra ...core.Tool) *harness {
	t.Helper()
	h := newHarness(t, store, steps, extra...)
	tool := &SubagentTool{Runtime: h.r, Service: h.svc}
	h.tools[tool.Name()] = tool
	return h
}

func TestSubagentOwnedConversationAnswersParent(t *testing.T) {
	ctx := context.Background()
	h := subagentHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("s1", "subagent", `{"task":"count the files"}`)}},
		{text: "there are 3 files"}, // child's answer
		{text: "parent done: 3 files"},
	})
	defer h.r.Close()
	parent := h.root(t)
	h.r.Submit(ctx, parent.ID, "actor", "", "how many files")
	run, _, err := h.svc.Step(ctx, parent.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("parent run: %+v %v", run, err)
	}
	children, err := h.r.OwnedConversations(ctx, ToolCallIdentity{RunID: run.ID, ConversationID: parent.ID, CallID: "s1"}.OwnerID())
	if err != nil || len(children) != 1 {
		t.Fatalf("owned: %v %v", children, err)
	}
	child, _ := h.r.Conversation(ctx, children[0])
	if child.Owner == nil || child.Owner.ConversationID != parent.ID || child.Config.Model != parent.Config.Model {
		t.Fatalf("child: %+v", child)
	}
	if got := entryTypes(h.entries(t, child.ID)); got != "user assistant" {
		t.Fatalf("child entries: %s", got)
	}
	// The parent's second request carried the child's answer as the tool result.
	result := h.client.requests[2].Messages[2].Content[0].(provider.ToolResultBlock)
	if result.IsError || core.MessageText(provider.Message{Content: result.Content}) != "there are 3 files" {
		t.Fatalf("tool result: %+v", result)
	}
	if childReq := h.client.requests[1]; childReq.SessionID != child.ID || len(childReq.Messages) != 1 {
		t.Fatalf("child request: %+v", childReq)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid || report.Conversations != 2 {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestSubagentReplayAttachesToExistingChild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	// The child calls a blocking tool, simulating a crash while the subagent
	// is mid-work.
	block := make(chan struct{})
	childTool := &effectTool{name: "effect", block: block}
	h := subagentHarness(t, store, []scriptStep{
		{calls: []provider.ToolCallBlock{call("s1", "subagent", `{"task":"slow work","key":"job-1"}`)}},
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
	}, childTool)
	parent := h.root(t)
	h.r.Submit(ctx, parent.ID, "actor", "", "do slow work")
	stepCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(stepCtx, parent.ID)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for childTool.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("child tool never started")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	parentRun, _, _ := h.r.Run(ctx, parent.ID)
	if parentRun.Phase != "tools" || parentRun.Tools[0].State != "running" {
		t.Fatalf("parent call after crash: %+v", parentRun)
	}
	childrenBefore, _ := h.r.OwnedConversations(ctx, ToolCallIdentity{RunID: parentRun.ID, ConversationID: parent.ID, CallID: "s1"}.OwnerID())
	if len(childrenBefore) != 1 {
		t.Fatalf("children before restart: %v", childrenBefore)
	}
	h.r.Close()
	// Restart: the subagent tool is replay safe, so it reruns, finds the same
	// child and submission, steps the child to completion, and answers.
	store, err = journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	// On restart the child's interrupted effect tool is reported (not replay
	// safe), then the child answers, then the parent answers.
	h2 := subagentHarness(t, store, []scriptStep{{text: "slow result"}, {text: "parent got: slow result"}}, &effectTool{name: "effect"})
	defer h2.r.Close()
	run, _, err := h2.svc.Step(ctx, parent.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("recovered: %+v %v", run, err)
	}
	childrenAfter, _ := h2.r.OwnedConversations(ctx, ToolCallIdentity{RunID: run.ID, ConversationID: parent.ID, CallID: "s1"}.OwnerID())
	if len(childrenAfter) != 1 || childrenAfter[0] != childrenBefore[0] {
		t.Fatalf("duplicate child after replay: before=%v after=%v", childrenBefore, childrenAfter)
	}
	child, _ := h2.r.Conversation(ctx, childrenAfter[0])
	if child.QueueSequence != 1 {
		t.Fatalf("duplicate submission after replay: %+v", child)
	}
	// The child's interrupted, non-replayable tool is reported in the
	// child's chain; the parent's call simply resumed its wait.
	// Request 0 is the child's resumed turn (with the interrupted result),
	// request 1 is the parent's turn with the child's answer.
	childReq := h2.client.requests[0]
	if len(childReq.Messages) != 3 || !childReq.Messages[2].Content[0].(provider.ToolResultBlock).IsError {
		t.Fatalf("child did not see its interrupted tool: %+v", childReq.Messages)
	}
	result := h2.client.requests[1].Messages[2].Content[0].(provider.ToolResultBlock)
	if result.IsError || core.MessageText(provider.Message{Content: result.Content}) != "slow result" {
		t.Fatalf("tool result: %+v", result)
	}
	if report, err := h2.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestSubagentAbortCascadesToChild(t *testing.T) {
	ctx := context.Background()
	block := make(chan struct{})
	childTool := &effectTool{name: "effect", block: block}
	h := subagentHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("s1", "subagent", `{"task":"use the tool"}`)}},
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}, // child calls a blocking tool
		{text: "never"},
	}, childTool)
	defer h.r.Close()
	parent := h.root(t)
	s, _ := h.r.Submit(ctx, parent.ID, "actor", "", "go")
	stepCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(stepCtx, parent.ID)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for childTool.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("child tool never started")
		}
		time.Sleep(time.Millisecond)
	}
	parentRun, _, _ := h.r.Run(ctx, parent.ID)
	children, _ := h.r.OwnedConversations(ctx, ToolCallIdentity{RunID: parentRun.ID, ConversationID: parent.ID, CallID: "s1"}.OwnerID())
	if len(children) != 1 {
		t.Fatalf("children: %v", children)
	}
	// Abort the parent: the child's run is marked in the same commit.
	if _, err := h.r.Abort(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	childRun, _, _ := h.r.Run(ctx, children[0])
	if !childRun.AbortRequested {
		t.Fatalf("abort did not cascade: %+v", childRun)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatalf("step: %v", err)
	}
	cancel()
	final, _, _ := h.r.Run(ctx, parent.ID)
	childFinal, _, _ := h.r.Run(ctx, children[0])
	if final.Outcome != "aborted" || childFinal.Outcome != "aborted" {
		t.Fatalf("outcomes: parent=%s child=%s", final.Outcome, childFinal.Outcome)
	}
	if sub, _ := h.r.Submission(ctx, s.ID); sub.State != "aborted" {
		t.Fatalf("submission: %+v", sub)
	}
	if h.client.requestCount() != 2 {
		t.Fatalf("requests after abort: %d", h.client.requestCount())
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid || report.ActiveRuns != 0 {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestSubagentNestingLimitAndOutsideRun(t *testing.T) {
	ctx := context.Background()
	h := subagentHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("s1", "subagent", `{"task":"level 1"}`)}},
		{calls: []provider.ToolCallBlock{call("s2", "subagent", `{"task":"level 2"}`)}},
		{calls: []provider.ToolCallBlock{call("s3", "subagent", `{"task":"level 3"}`)}},
		{text: "level 2 answer after limit"},
		{text: "level 1 answer"},
		{text: "root answer"},
	})
	defer h.r.Close()
	h.tools["subagent"].(*SubagentTool).MaxDepth = 2
	root := h.root(t)
	h.r.Submit(ctx, root.ID, "actor", "", "nest")
	run, _, err := h.svc.Step(ctx, root.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	// Level 2 tried to spawn level 3 and was refused.
	var refused bool
	for _, req := range h.client.requests {
		for _, m := range req.Messages {
			for _, c := range m.Content {
				if tr, ok := c.(provider.ToolResultBlock); ok && tr.IsError && strings.Contains(core.MessageText(provider.Message{Content: tr.Content}), "nesting limit") {
					refused = true
				}
			}
		}
	}
	if !refused {
		t.Fatal("nesting limit not enforced")
	}
	report, err := h.r.CheckIntegrity(ctx)
	if err != nil || !report.Valid || report.Conversations != 3 {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	tool := &SubagentTool{Runtime: h.r, Service: h.svc}
	if _, err := tool.Execute(ctx, []byte(`{"task":"x"}`), func(string) {}); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("outside run: %v", err)
	}
	if _, err := h.r.CreateOwnedConversation(ctx, root.ID, "", "k", nil); err == nil {
		t.Fatal("owner required")
	}
	if _, err := h.r.CreateOwnedConversation(ctx, "missing", "o", "k", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing parent: %v", err)
	}
}
