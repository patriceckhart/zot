package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func requireApproval(ctx context.Context, c Conversation, call provider.ToolCallBlock, args json.RawMessage) (ApprovalRequest, bool) {
	if call.Name == "effect" {
		return ApprovalRequest{Summary: "run effect " + string(args)}, true
	}
	return ApprovalRequest{}, false
}

// A pending approval is a committed record: the run parks, nothing executes,
// a restart (new runtime over the same store) finds the same pending record
// and still does not execute, and the decision binds to the exact arguments.
func TestApprovalPersistsAcrossRestartAndIsNeverImplicit(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "store")
	open := func(steps []scriptStep, tool *effectTool) *harness {
		store, err := journal.Open(ctx, dir, journal.Options{Durability: storage.Process})
		if err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, store, steps, tool)
		h.svc.opts.Approver = requireApproval
		return h
	}
	tool := &effectTool{name: "effect"}
	h := open([]scriptStep{{calls: []provider.ToolCallBlock{call("c1", "effect", `{"n":1}`)}}, {text: "done"}}, tool)
	c := h.root(t)
	if _, err := h.r.Submit(ctx, c.ID, "a", "", "go"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("step without approval: %v", err)
	}
	pending, err := h.r.PendingApprovals(ctx, c.ID)
	if err != nil || len(pending) != 1 || pending[0].State != "pending" || pending[0].Tool != "effect" {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	plan, _ := h.r.RecoveryPreview(ctx)
	if len(plan.Actions) != 1 || plan.Actions[0].Action != "approve" || plan.Actions[0].Automatic {
		t.Fatalf("plan: %+v", plan)
	}
	snap, _ := h.r.ConversationSnapshot(ctx, c.ID, 10)
	if len(snap.Approvals) != 1 {
		t.Fatalf("snapshot approvals: %+v", snap.Approvals)
	}
	// Stepping again does not ask twice and does not execute.
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("second step: %v", err)
	}
	if again, _ := h.r.PendingApprovals(ctx, c.ID); len(again) != 1 || again[0].ID != pending[0].ID {
		t.Fatalf("duplicate approval: %+v", again)
	}
	if tool.calls.Load() != 0 {
		t.Fatal("tool executed before approval")
	}
	// Restart: a fresh runtime over the same store attaches to the same record.
	h.r.Close()
	tool2 := &effectTool{name: "effect"}
	h = open([]scriptStep{{text: "done"}}, tool2)
	defer h.r.Close()
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("step after restart: %v", err)
	}
	if tool2.calls.Load() != 0 {
		t.Fatal("tool executed after restart without decision")
	}
	// A decision for a different approval ID is not found; deny then allow is a conflict.
	if _, err := h.r.Decide(ctx, "missing", "human", true, "", ""); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("missing approval: %v", err)
	}
	a, err := h.r.Decide(ctx, pending[0].ID, "human", true, "call", "looks fine")
	if err != nil || a.State != "allowed" || a.Actor != "human" {
		t.Fatalf("decide: %+v %v", a, err)
	}
	if _, err := h.r.Decide(ctx, pending[0].ID, "other", false, "", ""); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("second decision accepted: %v", err)
	}
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatalf("step after approval: %v", err)
	}
	if tool2.calls.Load() != 1 {
		t.Fatalf("tool calls after approval: %d", tool2.calls.Load())
	}
	run, _, _ := h.r.Run(ctx, c.ID)
	if run.Outcome != "completed" {
		t.Fatalf("run: %+v", run)
	}
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result assistant" {
		t.Fatalf("entries: %s", got)
	}
	if a, _ := h.r.Approval(ctx, pending[0].ID); a.State != "allowed" || a.Reason != "looks fine" {
		t.Fatalf("decided approval: %+v", a)
	}
	report, err := h.r.CheckIntegrity(ctx)
	if err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// A denial produces a blocked tool result, a tool-scoped allow covers later
// calls of the same tool until the configuration changes, and aborting a
// parked run expires its pending approval.
func TestApprovalDenyScopeAndAbort(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect"}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{"n":1}`)}},
		{calls: []provider.ToolCallBlock{call("c2", "effect", `{"n":2}`)}},
		{calls: []provider.ToolCallBlock{call("c3", "effect", `{"n":3}`)}},
		{text: "done"},
		{calls: []provider.ToolCallBlock{call("c4", "effect", `{"n":4}`)}},
		{calls: []provider.ToolCallBlock{call("c5", "effect", `{"n":5}`)}},
	}, tool)
	defer h.r.Close()
	h.svc.opts.Approver = requireApproval
	c := h.root(t)
	h.r.Submit(ctx, c.ID, "a", "", "go")
	// c1: denied.
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatal(err)
	}
	pending, _ := h.r.PendingApprovals(ctx, c.ID)
	if _, err := h.r.Decide(ctx, pending[0].ID, "human", false, "", "not now"); err != nil {
		t.Fatal(err)
	}
	// c2: allowed for the whole tool.
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatal(err)
	}
	pending, _ = h.r.PendingApprovals(ctx, c.ID)
	if _, err := h.r.Decide(ctx, pending[0].ID, "human", true, "tool", ""); err != nil {
		t.Fatal(err)
	}
	// c3 is covered by the tool-scoped allow: the run finishes without asking.
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	entries := h.entries(t, c.ID)
	if got := entryTypes(entries); got != "user assistant tool_result assistant tool_result assistant tool_result assistant" {
		t.Fatalf("entries: %s", got)
	}
	if entries[2].Content == "" || tool.calls.Load() != 2 {
		t.Fatalf("denied call: %q calls=%d", entries[2].Content, tool.calls.Load())
	}
	var denied provider.Message
	denied, _ = core.DecodeMessage(entries[2].Message)
	if !denied.Content[0].(provider.ToolResultBlock).IsError {
		t.Fatal("denied result not an error")
	}
	// A configuration change invalidates the tool-scoped allow.
	cur, _ := h.r.Conversation(ctx, c.ID)
	if _, err := h.r.Configure(ctx, c.ID, cur.Revision, AgentConfig{Model: "scripted", Instructions: "changed"}); err != nil {
		t.Fatal(err)
	}
	h.r.Submit(ctx, c.ID, "a", "", "again")
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("tool-scoped allow survived configuration change: %v", err)
	}
	// Abort the parked run: the pending approval expires and cannot be decided.
	pending, _ = h.r.PendingApprovals(ctx, c.ID)
	if _, err := h.r.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	a, _ := h.r.Approval(ctx, pending[0].ID)
	if a.State != "expired" {
		t.Fatalf("approval after abort: %+v", a)
	}
	if _, err := h.r.Decide(ctx, pending[0].ID, "human", true, "", ""); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("expired approval decided: %v", err)
	}
	if tool.calls.Load() != 2 {
		t.Fatalf("aborted run executed: %d", tool.calls.Load())
	}
	// Expiry by TTL is honoured at decision time.
	h.svc.opts.Approver = func(ctx context.Context, c Conversation, call provider.ToolCallBlock, args json.RawMessage) (ApprovalRequest, bool) {
		return ApprovalRequest{Summary: "short", TTL: time.Nanosecond}, true
	}
	h.r.Submit(ctx, c.ID, "a", "", "once more")
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatal(err)
	}
	pending, _ = h.r.PendingApprovals(ctx, c.ID)
	if _, err := h.r.Decide(ctx, pending[0].ID, "human", true, "", ""); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("expired-by-ttl approval decided: %v", err)
	}
	if a, _ := h.r.Approval(ctx, pending[0].ID); a.State != "expired" {
		t.Fatalf("ttl approval: %+v", a)
	}
	report, err := h.r.CheckIntegrity(ctx)
	if err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// Through the host: an approver role decides, a submit role cannot, and the
// parked run resumes after the decision commits.
func TestHostApprovalProtocol(t *testing.T) {
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	tool := &effectTool{name: "effect"}
	client := &scriptedClient{steps: []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}, {text: "done"}}}
	engine := EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(client, "scripted", "", core.NewRegistry(tool))
		a.MaxRetries = 0
		return a, nil
	})
	host, err := NewHost(r, engine, HostOptions{Execution: ExecutionOptions{Approver: requireApproval}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go host.Run(ctx)
	t.Cleanup(func() { cancel(); r.Close() })
	server := &HostServer{Host: host, Engine: engine, Tokens: map[string]Role{"approver-token-0123": RoleApprove, "submit-token-01234": RoleSubmit}}
	submitter := dialHost(t, server)
	submitter.must("hello", map[string]any{"token": "submit-token-01234"})
	approver := dialHost(t, server)
	approver.must("hello", map[string]any{"token": "approver-token-0123"})
	conv := submitter.must("conversation.create", map[string]any{"workspace": "w"})
	id := conv["id"].(string)
	sub := submitter.must("conversation.submit", map[string]any{"id": id, "content": "go"})
	deadline := time.Now().Add(5 * time.Second)
	var pendingID string
	for pendingID == "" && time.Now().Before(deadline) {
		list := approver.must("approval.list", map[string]any{"conversation": id})
		if items := list["approvals"].([]any); len(items) == 1 {
			pendingID = items[0].(map[string]any)["id"].(string)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if pendingID == "" {
		t.Fatal("no pending approval")
	}
	if resp := submitter.call("approval.decide", map[string]any{"id": pendingID, "allow": true}); resp["code"] != "forbidden" {
		t.Fatalf("submit role decided: %v", resp)
	}
	snap := submitter.must("conversation.snapshot", map[string]any{"id": id, "limit": 10})
	if rec, _ := snap["recovery"].(map[string]any); rec["action"] != "approve" {
		t.Fatalf("snapshot recovery: %v", snap["recovery"])
	}
	approver.must("approval.decide", map[string]any{"id": pendingID, "allow": true, "reason": "ok"})
	settled := submitter.must("submission.wait", map[string]any{"id": sub["id"]})
	if settled["state"] != "answered" || tool.calls.Load() != 1 {
		t.Fatalf("after approval: %v calls=%d", settled, tool.calls.Load())
	}
	if resp := approver.call("approval.decide", map[string]any{"id": pendingID, "allow": false}); resp["code"] != "conflict" {
		t.Fatalf("redecide: %v", resp)
	}
}
