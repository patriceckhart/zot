package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// echoClient answers every request with the last user message, so many
// conversations can run concurrently without a shared script.
type echoClient struct {
	mu       sync.Mutex
	requests int
	delay    time.Duration
}

func (c *echoClient) Name() string { return "synthetic" }
func (c *echoClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.requests++
	c.mu.Unlock()
	ch := make(chan provider.Event, 4)
	go func() {
		defer close(ch)
		if c.delay > 0 {
			select {
			case <-time.After(c.delay):
			case <-ctx.Done():
				ch <- provider.EventDone{Stop: provider.StopAborted, Err: ctx.Err()}
				return
			}
		}
		text := "echo: " + core.MessageText(req.Messages[len(req.Messages)-1])
		ch <- provider.EventTextDelta{Delta: text}
		ch <- provider.EventUsage{Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}}
		ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}}}
	}()
	return ch, nil
}

func (c *echoClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func echoEngine(client provider.Client, tools core.Registry) Engine {
	return EngineFunc(func(ctx context.Context, c Conversation) (*core.Agent, error) {
		a := core.NewAgent(client, "echo", "", tools)
		a.MaxRetries = 0
		return a, nil
	})
}

func TestHostRunsManyConversationsAndSleeps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	client := &echoClient{}
	host, err := NewHost(r, echoEngine(client, core.NewRegistry()), HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	const n = 20
	var subs []Submission
	for i := 0; i < n; i++ {
		c, err := r.OpenRoot(ctx, fmt.Sprint("workspace-", i), AgentConfig{})
		if err != nil {
			t.Fatal(err)
		}
		s, err := r.Submit(ctx, c.ID, "actor", "", fmt.Sprint("question ", i))
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, s)
	}
	host.Nudge()
	for _, s := range subs {
		waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
		settled, err := r.WaitSubmission(waitCtx, s.ID)
		cancelWait()
		if err != nil || settled.State != "answered" {
			t.Fatalf("submission %s: %+v %v", s.ID, settled, err)
		}
	}
	if client.count() != n {
		t.Fatalf("requests: %d", client.count())
	}
	// Idle host sleeps: no requests happen without input.
	time.Sleep(20 * time.Millisecond)
	if client.count() != n {
		t.Fatal("idle host sent requests")
	}
	// A second round after idle wakes through the store's Wait.
	s, _ := r.Submit(ctx, subs[0].ConversationID, "actor", "", "follow-up")
	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	settled, err := r.WaitSubmission(waitCtx, s.ID)
	cancelWait()
	if err != nil || settled.State != "answered" {
		t.Fatalf("follow-up: %+v %v", settled, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("host exit: %v", err)
	}
	if report, err := r.CheckIntegrity(context.Background()); err != nil || !report.Valid || report.Conversations != n || report.ActiveRuns != 0 {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestHostRecoveryPolicyBlocksUnsafeRuns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	// Leave two interrupted runs as an earlier build would: one with a
	// pending (safe to continue) tool, one with a running unsafe tool.
	h := newHarness(t, store, nil)
	a := h.root(t)
	b, _ := h.r.OpenRoot(ctx, "second", AgentConfig{Provider: "synthetic", Model: "scripted"})
	queueOnly(t, h.r, a.ID, "a")
	queueOnly(t, h.r, b.ID, "b")
	writeLegacyRun(t, h.r, a.ID, "tools", legacyIntent{call: call("u1", "unsafe", `{}`), state: "running"})
	runB := writeLegacyRun(t, h.r, b.ID, "tools", legacyIntent{call: call("p1", "unsafe", `{}`), state: "pending"})
	h.r.Close()
	// Reopen under a host with the safe policy: b (pending) resumes, a
	// (running unsafe) is blocked until a human unblocks it.
	store, err = journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := New(store)
	defer r2.Close()
	client := &scriptedClient{steps: []scriptStep{{text: "b done"}, {text: "a done"}}}
	tools := core.NewRegistry(&effectTool{name: "unsafe"})
	host, err := NewHost(r2, echoEngine(client, tools), HostOptions{Policy: RecoverSafe})
	if err != nil {
		t.Fatal(err)
	}
	hostCtx, cancelHost := context.WithCancel(ctx)
	defer cancelHost()
	hostDone := make(chan error, 1)
	go func() { hostDone <- host.Run(hostCtx) }()
	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	bSub, _ := r2.Submission(ctx, runB.Submissions[0])
	settled, err := r2.WaitSubmission(waitCtx, bSub.ID)
	cancelWait()
	if err != nil || settled.State != "answered" {
		t.Fatalf("b not recovered: %+v %v", settled, err)
	}
	runA, _, _ := r2.Run(ctx, a.ID)
	if runA.Phase == "done" || client.requestCount() != 1 {
		t.Fatal("blocked run a was stepped")
	}
	blocked := host.Blocked()
	if len(blocked) != 1 || blocked[0] != runA.ID {
		t.Fatalf("blocked: %v", blocked)
	}
	// A human decides: unblock. The unsafe tool is reported, not rerun.
	host.Unblock(runA.ID)
	aSub, _ := r2.Submission(ctx, runA.Submissions[0])
	waitCtx, cancelWait = context.WithTimeout(ctx, 10*time.Second)
	settled, err = r2.WaitSubmission(waitCtx, aSub.ID)
	cancelWait()
	if err != nil || settled.State != "answered" {
		t.Fatalf("a not recovered after unblock: %+v %v", settled, err)
	}
	if tools["unsafe"].(*effectTool).calls.Load() != 1 {
		// Only b's pending call ran; a's running call was not repeated.
		t.Fatalf("unsafe tool executions after recovery: %d", tools["unsafe"].(*effectTool).calls.Load())
	}
	cancelHost()
	<-hostDone
}

func TestSnapshotThenWatchHasNoGap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := New(newMemoryStore())
	defer r.Close()
	client := &echoClient{delay: time.Millisecond}
	host, _ := NewHost(r, echoEngine(client, core.NewRegistry()), HostOptions{})
	go host.Run(ctx)
	c, _ := r.OpenRoot(ctx, "workspace", AgentConfig{})
	other, _ := r.OpenRoot(ctx, "other", AgentConfig{})
	r.Submit(ctx, c.ID, "actor", "", "first")
	snap, err := r.ConversationSnapshot(ctx, c.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// Commits race with the snapshot; the watch must deliver exactly the
	// ones after snap.Revision that touch c, and none from other.
	var seenMu sync.Mutex
	var seen []uint64
	lastSeen := func() uint64 {
		seenMu.Lock()
		defer seenMu.Unlock()
		if len(seen) == 0 {
			return 0
		}
		return seen[len(seen)-1]
	}
	watchCtx, cancelWatch := context.WithCancel(ctx)
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- r.Watch(watchCtx, c.ID, snap.Revision, func(cm storage.Commit) error {
			for _, op := range cm.Operations {
				if strings.Contains(op.Key, other.ID) {
					return fmt.Errorf("foreign commit delivered")
				}
			}
			seenMu.Lock()
			seen = append(seen, cm.Revision)
			seenMu.Unlock()
			return nil
		})
	}()
	r.Submit(ctx, other.ID, "actor", "", "noise")
	s2, _ := r.Submit(ctx, c.ID, "actor", "", "second")
	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	if _, err := r.WaitSubmission(waitCtx, s2.ID); err != nil {
		t.Fatal(err)
	}
	cancelWait()
	// Replay: snapshot entries plus watched commits must reconstruct the
	// final entry count.
	final, _ := r.Conversation(ctx, c.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if lastSeen() >= final.Revision {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch did not reach revision %d: %v", final.Revision, seen)
		}
		time.Sleep(time.Millisecond)
	}
	cancelWatch()
	if err := <-watchDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("watch: %v", err)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] || seen[0] <= snap.Revision {
			t.Fatalf("watch order: %v after %d", seen, snap.Revision)
		}
	}
	// Reconstruct entries from snapshot + watched commits.
	entries := map[string]bool{}
	for _, e := range snap.Entries {
		entries[e.ID] = true
	}
	commits, _ := r.Scan(ctx, snap.Revision, 1000)
	for _, cm := range commits {
		for _, op := range cm.Operations {
			if strings.HasPrefix(op.Key, "entry/"+c.ID+"/") {
				var e Entry
				json.Unmarshal(op.Value, &e)
				entries[e.ID] = true
			}
		}
	}
	if uint64(len(entries)) != final.EntrySequence {
		t.Fatalf("reconstructed %d entries, store has %d", len(entries), final.EntrySequence)
	}
	if err := r.Watch(ctx, c.ID, final.Revision+5, func(storage.Commit) error { return nil }); !errors.Is(err, storage.ErrCursor) {
		t.Fatalf("future cursor: %v", err)
	}
	if _, err := r.ConversationSnapshot(ctx, c.ID, 0); err == nil {
		t.Fatal("unbounded snapshot")
	}
	small, _ := r.ConversationSnapshot(ctx, c.ID, 1)
	if len(small.Entries) != 1 || !small.More {
		t.Fatalf("paged snapshot: %+v", small)
	}
}

func TestEventFanoutBoundsSlowConsumers(t *testing.T) {
	f := newEventFanout()
	fast, stopFast := f.subscribe(1000)
	defer stopFast()
	slow, stopSlow := f.subscribe(2)
	defer stopSlow()
	for i := 0; i < 10; i++ {
		f.publish(core.EvTextDelta{Delta: fmt.Sprint(i)})
	}
	if len(fast) != 10 {
		t.Fatalf("fast consumer: %d", len(fast))
	}
	if len(slow) != 2 {
		t.Fatalf("slow consumer buffer: %d", len(slow))
	}
	// Drain the slow consumer; the next publish delivers a gap first.
	<-slow
	<-slow
	f.publish(core.EvTextDelta{Delta: "after"})
	gap := <-slow
	if g, ok := gap.(EvGap); !ok || g.Dropped != 8 {
		t.Fatalf("gap: %+v", gap)
	}
	if next := <-slow; next.Type() != "text_delta" {
		t.Fatalf("after gap: %+v", next)
	}
	stopSlow()
	if _, open := <-slow; open {
		t.Fatal("unsubscribed channel still open")
	}
	stopSlow() // idempotent
}
