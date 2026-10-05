package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestAbortDuringModelRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		step scriptStep
	}{
		{name: "answer", step: scriptStep{text: "done"}},
		{name: "tool call", step: scriptStep{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}},
		{name: "retryable failure", step: scriptStep{err: errors.New("HTTP 503 service unavailable")}},
		{name: "context overflow", step: scriptStep{err: errors.New("maximum context length exceeded")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tool := &effectTool{name: "effect"}
			h := newHarness(t, newMemoryStore(), []scriptStep{tc.step, {text: "unexpected retry"}}, tool)
			h.svc.opts.Compaction = CompactionPolicy{ContextWindow: 100000}
			defer h.r.Close()
			c := h.root(t)
			sub, err := h.r.Submit(ctx, c.ID, "actor", "", "go")
			if err != nil {
				t.Fatal(err)
			}
			h.client.onStream = func(provider.Request) {
				if _, err := h.r.Abort(ctx, c.ID); err != nil {
					t.Error(err)
				}
			}
			run, _, err := h.svc.Step(ctx, c.ID)
			if err != nil || run.Outcome != "aborted" || tool.calls.Load() != 0 || h.client.requestCount() != 1 {
				t.Fatalf("abort: run=%+v err=%v effects=%d requests=%d", run, err, tool.calls.Load(), h.client.requestCount())
			}
			settled, err := h.r.Submission(ctx, sub.ID)
			if err != nil || settled.State != "aborted" {
				t.Fatalf("submission: %+v %v", settled, err)
			}
			usage, err := h.r.Usage(ctx, c.ID)
			if err != nil || usage.Known+usage.Unknown != 1 {
				t.Fatalf("attempt usage lost: %+v %v", usage, err)
			}
			if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
				t.Fatalf("integrity: %+v %v", report, err)
			}
		})
	}
}

func TestAbortDuringFailedOverflowCompaction(t *testing.T) {
	ctx := context.Background()
	h, summaries := compactionHarness(t, []scriptStep{
		{text: long(200)}, {text: long(200)},
		{err: errors.New("maximum context length exceeded")},
	})
	defer h.r.Close()
	h.svc.opts.Compaction = CompactionPolicy{ContextWindow: 100000, KeepRecentTokens: 150}
	c := h.root(t)
	for range 2 {
		if _, err := h.r.Submit(ctx, c.ID, "actor", "", long(200)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	summaries.fail = true
	h.svc.opts.Sink = func(ev core.AgentEvent) {
		if e, ok := ev.(core.EvCompact); ok && e.Phase == "pre" {
			if _, err := h.r.Abort(ctx, c.ID); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "overflow"); err != nil {
		t.Fatal(err)
	}
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "aborted" {
		t.Fatalf("abort replaced by compaction failure: %+v %v", run, err)
	}
}

func TestReplayAuthorizesCommittedArguments(t *testing.T) {
	for _, policy := range []core.ToolReplayPolicy{core.ReplaySafe, core.ReplayIdempotent, core.ReplayReconcile} {
		for _, mode := range []string{"deny", "rewrite", "mutate", "allow"} {
			t.Run(string(policy)+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				tool := &receiverTool{policy: policy, block: make(chan struct{}), started: make(chan struct{}, 1), outcome: core.ReconcileNotStarted}
				h := newHarness(t, newMemoryStore(), []scriptStep{
					{calls: []provider.ToolCallBlock{call("c1", "send", `{"to":"original"}`)}},
					{text: "done"},
				}, tool)
				defer h.r.Close()
				c := h.root(t)
				committed := json.RawMessage(`{"to":"effective"}`)
				h.guard = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
					return true, "", committed
				}
				if _, err := h.r.Submit(ctx, c.ID, "actor", "", "send it"); err != nil {
					t.Fatal(err)
				}
				interruptDuringTool(t, h, c, tool.started)
				tool.block = nil
				var checked json.RawMessage
				h.guard = func(tc provider.ToolCallBlock) (bool, string, json.RawMessage) {
					checked = append(json.RawMessage(nil), tc.Arguments...)
					switch mode {
					case "deny":
						return string(tc.Arguments) != string(committed), "effective target denied", nil
					case "rewrite":
						return true, "", json.RawMessage(`{"to":"new-target"}`)
					case "mutate":
						copy(tc.Arguments, `{"to":"forbidden"}`)
						return true, "", nil
					default:
						return true, "", json.RawMessage(`{ "to": "effective" }`)
					}
				}
				run, _, err := h.svc.Step(ctx, c.ID)
				if err != nil || run.Outcome != "completed" {
					t.Fatalf("recovery: %+v %v", run, err)
				}
				if string(checked) != string(committed) {
					t.Fatalf("authorized %s instead of committed %s", checked, committed)
				}
				wantCalls, wantReconciles := 1, 0
				if mode == "allow" {
					wantCalls = 2
					if policy == core.ReplayReconcile {
						wantReconciles = 1
					}
				}
				if len(tool.keys) != wantCalls || tool.recCalls != wantReconciles {
					t.Fatalf("executions=%d reconciliations=%d", len(tool.keys), tool.recCalls)
				}
				if (mode == "rewrite" || mode == "mutate") && !strings.Contains(strings.Join(run.Notices, "\n"), "arguments changed") {
					t.Fatalf("missing rewrite refusal: %v", run.Notices)
				}
			})
		}
	}
}

func TestQueuedInputCannotBypassRecoveryHold(t *testing.T) {
	for _, policy := range []RecoveryPolicy{RecoverNone, RecoverSafe} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, newMemoryStore(), []scriptStep{
				{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
			}, &effectTool{name: "effect"})
			defer h.r.Close()
			c := h.root(t)
			if _, err := h.r.Submit(ctx, c.ID, "actor", "", "first"); err != nil {
				t.Fatal(err)
			}
			run, _, err := h.svc.start(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			run, err = h.svc.request(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			snap, _ := h.r.Snapshot(ctx)
			cur, _ := conversation(snap, c.ID)
			run.Tools[0].State = "running"
			run.Tools[0].Args = json.RawMessage(`{}`)
			run.Tools[0].Replay = core.ReplayNever
			if _, err := h.svc.commitRun(ctx, snap, cur, run, "test.intent"); err != nil {
				t.Fatal(err)
			}
			host, err := NewHost(h.r, h.svc.currentEngine(), HostOptions{Policy: policy})
			if err != nil {
				t.Fatal(err)
			}
			if err := host.recover(ctx); err != nil {
				t.Fatal(err)
			}
			if host.allowed(run) {
				t.Fatal("run was not held")
			}
			if _, err := h.r.Submit(ctx, c.ID, "actor", "", "second"); err != nil {
				t.Fatal(err)
			}
			snap, _ = h.r.Snapshot(ctx)
			// A cancelled dispatch context keeps a buggy scheduler from doing
			// external work, but does not prevent it from selecting a held run.
			stepCtx, cancel := context.WithCancel(ctx)
			cancel()
			busy, err := host.dispatch(stepCtx, snap)
			if err != nil || busy {
				t.Fatalf("held conversation dispatched: busy=%v err=%v", busy, err)
			}
		})
	}
}

func TestQueuedInputWaitsForApprovalButAbortCanSettle(t *testing.T) {
	ctx := context.Background()
	tool := &effectTool{name: "effect"}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "answer to queued input"},
	}, tool)
	defer h.r.Close()
	h.svc.opts.Approver = requireApproval
	c := h.root(t)
	first, err := h.r.Submit(ctx, c.ID, "actor", "", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
		t.Fatalf("approval not requested: %v", err)
	}
	second, err := h.r.Submit(ctx, c.ID, "actor", "", "second")
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewHost(h.r, h.svc.currentEngine(), HostOptions{Execution: ExecutionOptions{Approver: requireApproval}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := h.r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stepCtx, cancel := context.WithCancel(ctx)
	cancel()
	if busy, err := host.dispatch(stepCtx, snap); err != nil || busy {
		t.Fatalf("approval-held conversation dispatched: busy=%v err=%v", busy, err)
	}
	if _, err := h.r.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	hostCtx, stopHost := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- host.Run(hostCtx) }()
	defer func() {
		stopHost()
		<-done
	}()
	waitCtx, stopWait := context.WithTimeout(ctx, 5*time.Second)
	defer stopWait()
	for _, want := range []struct {
		id    string
		state string
	}{{first.ID, "aborted"}, {second.ID, "answered"}} {
		got, err := h.r.WaitSubmission(waitCtx, want.id)
		if err != nil || got.State != want.state {
			t.Fatalf("submission: %+v %v, want %s", got, err, want.state)
		}
	}
	if tool.calls.Load() != 0 {
		t.Fatal("unapproved tool executed")
	}
}

func TestHandoffResultAndContinuationCommitTogether(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "handoff", `{"note":"state","continue":"finish"}`), call("c2", "effect", `{}`)}},
		{text: "continued"},
	}, HandoffTool{}, &effectTool{name: "effect"})
	defer h.r.Close()
	c := h.root(t)
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "go"); err != nil {
		t.Fatal(err)
	}
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.svc.opts.Sink = func(ev core.AgentEvent) {
		if e, ok := ev.(core.EvToolResult); ok && e.Name == "handoff" {
			cancel()
		}
	}
	if _, _, err := h.svc.Step(stepCtx, c.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted step: %v", err)
	}
	commits, err := h.r.Scan(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cm := range commits {
		for _, e := range EntriesFromCommit(cm, c.ID) {
			if e.Type == entryToolResult && strings.Contains(e.Content, "handing off") {
				found = true
				if got := entryTypes(EntriesFromCommit(cm, c.ID)); got != "tool_result tool_result reset user" {
					t.Fatalf("handoff result committed without continuation: %s", got)
				}
			}
		}
	}
	if !found {
		t.Fatal("handoff result missing")
	}
	h.svc.opts.Sink = func(core.AgentEvent) {}
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	cur, _ := h.r.Conversation(ctx, c.ID)
	if cur.QueueSequence != 2 {
		t.Fatalf("continuation count: %d", cur.QueueSequence)
	}
	if got := h.client.requests[1].Messages; len(got) != 2 || core.MessageText(got[0]) != "state" || core.MessageText(got[1]) != "finish" {
		t.Fatalf("continuation context: %+v", got)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

type handoffFaultStore struct {
	storage.Store
	after  bool
	failed bool
}

var errHandoffCommit = errors.New("injected handoff commit failure")

func (s *handoffFaultStore) Commit(ctx context.Context, m storage.Mutation) (storage.Commit, error) {
	if !s.failed {
		for _, op := range m.Operations {
			if strings.HasPrefix(op.Key, "entry/") && strings.Contains(string(op.Value), "handing off to a fresh context") {
				s.failed = true
				if s.after {
					if _, err := s.Store.Commit(ctx, m); err != nil {
						return storage.Commit{}, err
					}
				}
				return storage.Commit{}, errHandoffCommit
			}
		}
	}
	return s.Store.Commit(ctx, m)
}

func TestHandoffRecoveryAroundCommitFailure(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := "before commit"
		if after {
			name = "after commit"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := &handoffFaultStore{Store: newMemoryStore(), after: after}
			h := newHarness(t, store, []scriptStep{
				{calls: []provider.ToolCallBlock{call("c1", "handoff", `{"note":"state","continue":"finish"}`)}},
				{text: "continued"},
			}, HandoffTool{})
			defer h.r.Close()
			c := h.root(t)
			if _, err := h.r.Submit(ctx, c.ID, "actor", "", "go"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, errHandoffCommit) {
				t.Fatalf("fault not reached: %v", err)
			}
			if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			cur, err := h.r.Conversation(ctx, c.ID)
			if err != nil || cur.QueueSequence != 2 {
				t.Fatalf("continuation count: %+v %v", cur, err)
			}
			if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result reset user assistant" {
				t.Fatalf("recovered history: %s", got)
			}
			if _, did, err := h.svc.Step(ctx, c.ID); err != nil || did {
				t.Fatalf("duplicate continuation: did=%v err=%v", did, err)
			}
			if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
				t.Fatalf("integrity: %+v %v", report, err)
			}
		})
	}
}
