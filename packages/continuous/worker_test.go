package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type remoteEffect struct {
	calls   atomic.Int32
	block   chan struct{}
	started chan struct{}
}

func (t *remoteEffect) Name() string            { return "deploy" }
func (t *remoteEffect) Description() string     { return "remote deploy" }
func (t *remoteEffect) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *remoteEffect) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.calls.Add(1)
	if t.started != nil {
		t.started <- struct{}{}
	}
	if t.block != nil {
		<-t.block
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "deployed with " + core.ToolOperationKey(ctx)}}}, nil
}

func startWorker(t *testing.T, tool core.Tool) (*WorkerServer, func() net.Conn) {
	t.Helper()
	w := &WorkerServer{Environment: "staging", Tools: core.NewRegistry(tool), Token: "worker-secret"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go w.Serve(ctx, ln)
	t.Cleanup(cancel)
	return w, func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
}

// Lost connectivity does not imply the remote process stopped: the host
// records an unknown outcome, the worker finishes the effect, and recovery
// reconciles the completed result without executing again.
func TestRemoteWorkerDisconnectReconciles(t *testing.T) {
	ctx := context.Background()
	effect := &remoteEffect{block: make(chan struct{}), started: make(chan struct{}, 1)}
	_, dial := startWorker(t, effect)
	if _, err := DialWorker(ctx, dial(), "wrong"); err == nil {
		t.Fatal("worker accepted a bad token")
	}
	first, err := DialWorker(ctx, dial(), "worker-secret")
	if err != nil {
		t.Fatal(err)
	}
	if first.Hello.Environment != "staging" || !first.Hello.Lookup {
		t.Fatalf("hello: %+v", first.Hello)
	}
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	remote := &RemoteTool{Worker: first, ToolName: "deploy", Environment: "staging", Epoch: r.Epoch}
	client := &scriptedClient{steps: []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "deploy", `{"env":"staging"}`)}}, {text: "done"}}}
	engine := EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(client, "scripted", "", core.NewRegistry(remote))
		a.MaxRetries = 0
		return a, nil
	})
	svc, _ := NewService(r, engine, ExecutionOptions{})
	c, _ := r.OpenRoot(ctx, "w", AgentConfig{Model: "scripted"})
	r.Submit(ctx, c.ID, "a", "", "deploy it")
	done := make(chan error, 1)
	go func() { _, _, err := svc.Step(ctx, c.ID); done <- err }()
	<-effect.started
	// The host loses the worker connection while the effect is in flight.
	first.Close()
	err = <-done
	if !errors.Is(err, core.ErrToolOutcomeUnknown) {
		t.Fatalf("step after disconnect: %v", err)
	}
	run, _, _ := r.Run(ctx, c.ID)
	if run.Phase != "tools" || run.Tools[0].State != "running" || run.Tools[0].Replay != core.ReplayReconcile {
		t.Fatalf("intent after disconnect: %+v", run)
	}
	plan, _ := r.RecoveryPreview(ctx)
	if plan.Actions[0].Action != "reconcile" {
		t.Fatalf("plan: %+v", plan)
	}
	// The worker finishes the effect on its own.
	close(effect.block)
	// Reconnect and recover: lookup finds the completed operation, the
	// result is recovered, and the tool is not executed a second time.
	second, err := DialWorker(ctx, dial(), "worker-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	remote.Worker = second
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, _, err = svc.Step(ctx, c.ID)
		if err == nil || time.Now().After(deadline) {
			break
		}
		// The worker may still be marking the operation completed.
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("recovered run: %+v %v", run, err)
	}
	if effect.calls.Load() != 1 {
		t.Fatalf("effect executed %d times", effect.calls.Load())
	}
	result := client.requests[1].Messages[2].Content[0].(provider.ToolResultBlock)
	if result.IsError || !strings.Contains(core.MessageText(provider.Message{Content: result.Content}), "deployed with zot-op-") {
		t.Fatalf("recovered result: %+v", result)
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "reconciled as completed") {
		t.Fatalf("notices: %v", run.Notices)
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

// A stale writer epoch cannot execute through the worker, and a repeated
// operation key returns the original result without a second effect.
func TestRemoteWorkerFencesEpochAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	effect := &remoteEffect{}
	w, dial := startWorker(t, effect)
	_ = w
	conn, err := DialWorker(ctx, dial(), "worker-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	call := WorkerCall{OperationKey: "zot-op-test", RunID: "r", ConversationID: "c", CallID: "c1", Epoch: 5, Environment: "staging", Tool: "deploy", Args: json.RawMessage(`{}`)}
	if _, err := conn.call(ctx, "tool.execute", call); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.call(ctx, "tool.execute", call); err != nil {
		t.Fatal(err)
	}
	if effect.calls.Load() != 1 {
		t.Fatalf("repeated key executed %d times", effect.calls.Load())
	}
	stale := call
	stale.OperationKey, stale.Epoch = "zot-op-other", 4
	if _, err := conn.call(ctx, "tool.execute", stale); err == nil || !strings.Contains(err.Error(), "stale writer epoch") {
		t.Fatalf("stale epoch accepted: %v", err)
	}
	data, err := conn.call(ctx, "tool.lookup", WorkerLookup{OperationKey: "never-started"})
	if err != nil {
		t.Fatal(err)
	}
	var res WorkerLookupResult
	json.Unmarshal(data, &res)
	if res.State != "not_started" {
		t.Fatalf("lookup of unknown key: %+v", res)
	}
}

// A worker with a ledger answers lookups after a restart: completed operations
// return their result, operations that were running at the crash are unknown.
func TestRemoteWorkerLedgerSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	ledger := filepath.Join(t.TempDir(), "ops.jsonl")
	effect := &remoteEffect{}
	w1 := &WorkerServer{Environment: "staging", Tools: core.NewRegistry(effect), LedgerPath: ledger}
	if err := w1.Open(); err != nil {
		t.Fatal(err)
	}
	done := WorkerCall{OperationKey: "zot-op-done", RunID: "r", ConversationID: "c", CallID: "c1", Epoch: 3, Tool: "deploy", Args: json.RawMessage(`{}`)}
	if _, err := w1.execute(ctx, done); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-operation: write the running line without completion.
	w1.mu.Lock()
	w1.appendLedger(workerLedgerLine{Key: "zot-op-crashed", State: "running", Epoch: 3, Conv: "c"})
	w1.mu.Unlock()
	w1.Close()

	w2 := &WorkerServer{Environment: "staging", Tools: core.NewRegistry(effect), LedgerPath: ledger}
	if err := w2.Open(); err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if res := w2.lookup("zot-op-done"); res.State != "completed" || res.Result == nil {
		t.Fatalf("completed lost: %+v", res)
	}
	if res := w2.lookup("zot-op-crashed"); res.State != "unknown" {
		t.Fatalf("crashed operation must be unknown: %+v", res)
	}
	if res := w2.lookup("zot-op-never"); res.State != "not_started" {
		t.Fatalf("unknown key: %+v", res)
	}
	// Re-executing a crashed key is refused: the effect may have happened.
	if _, err := w2.execute(ctx, WorkerCall{OperationKey: "zot-op-crashed", ConversationID: "c", Epoch: 3, Tool: "deploy", Args: json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("crashed key re-executed: %v", err)
	}
	if effect.calls.Load() != 1 {
		t.Fatalf("effects: %d", effect.calls.Load())
	}
	// Epochs are restored too: a stale host is still fenced.
	if _, err := w2.execute(ctx, WorkerCall{OperationKey: "zot-op-new", ConversationID: "c", Epoch: 2, Tool: "deploy", Args: json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale epoch after restart: %v", err)
	}
}
