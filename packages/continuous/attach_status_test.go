package continuous

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// progressTool reports output in two chunks, waiting between them so each
// is committed as its own progress record.
type progressTool struct{ release chan struct{} }

func (progressTool) Name() string                        { return "effect" }
func (progressTool) Description() string                 { return "streams progress" }
func (progressTool) Schema() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (progressTool) ReplayPolicy() core.ToolReplayPolicy { return "" }
func (t progressTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	progress("line one\n")
	<-t.release
	time.Sleep(PartialFlushInterval + 10*time.Millisecond) // the next chunk must clear the progress throttle
	progress("line two\n")
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "done"}}}, nil
}

// An attached prompt shows why it waits on a pending approval, clears the
// status once the run continues, and renders committed tool progress as
// deltas, once each.
func TestAttachedDriverReportsWaitingAndProgress(t *testing.T) {
	tool := progressTool{release: make(chan struct{})}
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{steps: []scriptStep{
		{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}},
		{text: "finished"},
	}}
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
	c := dialClient(t, &HostServer{Host: host, Engine: engine})
	var conv Conversation
	if err := c.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var statuses []string
	var progress []string
	driver := &AttachedDriver{Client: c, ConversationID: conv.ID, OnStatus: func(s string) {
		mu.Lock()
		statuses = append(statuses, s)
		mu.Unlock()
	}}
	view := core.NewAgent(nil, "scripted", "", core.NewRegistry())
	done := make(chan error, 1)
	go func() {
		done <- driver.Prompt(ctx, view, "go", func(ev core.AgentEvent) {
			if p, ok := ev.(core.EvToolProgress); ok {
				mu.Lock()
				progress = append(progress, p.Text)
				mu.Unlock()
			}
		})
	}()
	approval := waitApproval(t, r, conv.ID)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(statuses) > 0 && strings.HasPrefix(statuses[len(statuses)-1], "awaiting approval: effect")
	}, "awaiting-approval status")
	if _, err := r.Decide(ctx, approval.ID, "tester", true, "call", "ok"); err != nil {
		t.Fatal(err)
	}
	host.Nudge()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(progress) >= 1
	}, "first progress chunk")
	close(tool.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	// The second chunk lands after the throttle; the final flush commits
	// it before the result, so both deltas arrive exactly once.
	if got := strings.Join(progress, ""); got != "line one\nline two\n" {
		t.Fatalf("progress deltas: %q", progress)
	}
	if statuses[len(statuses)-1] != "" {
		t.Fatalf("status not cleared: %q", statuses)
	}
}

func dialClient(t *testing.T, server *HostServer) *Client {
	t.Helper()
	a, b := net.Pipe()
	go server.ServeConn(context.Background(), b)
	c, err := NewClient(context.Background(), a, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func waitApproval(t *testing.T, r *Runtime, conversationID string) Approval {
	t.Helper()
	var out Approval
	waitFor(t, func() bool {
		snap, err := r.Snapshot(context.Background())
		if err != nil {
			return false
		}
		list, err := pendingApprovals(snap, conversationID)
		if err != nil || len(list) == 0 {
			return false
		}
		out = list[0]
		return true
	}, "pending approval")
	return out
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Wait-state descriptions follow committed records only.
func TestWaitStateDescribe(t *testing.T) {
	ws := newWaitState(Submission{ID: "s1", State: "queued"}, ConversationSnapshot{})
	if ws.describe() != "queued on host" {
		t.Fatalf("queued: %q", ws.describe())
	}
	running, _ := json.Marshal(Submission{ID: "s1", State: "running"})
	if !ws.apply(storage.Commit{Operations: []storage.Operation{{Key: "submission/s1", Value: running}}}, "c") || ws.describe() != "" {
		t.Fatalf("running: %q", ws.describe())
	}
	held := newWaitState(Submission{ID: "s1", State: "queued"}, ConversationSnapshot{Recovery: &RecoveryAction{Action: "report"}})
	if !strings.HasPrefix(held.describe(), "recovery blocked (report)") {
		t.Fatalf("recovery: %q", held.describe())
	}
	if !held.apply(storage.Commit{Operations: []storage.Operation{{Key: chainKey("c"), Delete: true}}}, "c") {
		t.Fatal("chain movement did not clear the recovery hold")
	}
	// Progress of another conversation's task is ignored.
	task, _ := json.Marshal(Task{ID: "t1", ConversationID: "other"})
	prog, _ := json.Marshal(ToolProgress{TaskID: "t1", CallID: "x", Text: "secret"})
	if got := ToolProgressFromCommit(storage.Commit{Operations: []storage.Operation{{Key: "task/t1", Value: task}, {Key: "progress/t1", Value: prog}}}, "c"); len(got) != 0 {
		t.Fatalf("foreign progress: %+v", got)
	}
	if HistoryNotice(ConversationSnapshot{Entries: make([]Entry, 3)}, "c") != "" || HistoryNotice(ConversationSnapshot{More: true, Entries: make([]Entry, 3)}, "c") == "" {
		t.Fatal("history notice")
	}
}
