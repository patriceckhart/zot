package continuous

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
)

func livePartialFixture(t *testing.T, interval time.Duration) (*Runtime, *taskPartial) {
	t.Helper()
	return livePartialOnRuntime(t, newTestRuntime(t), interval)
}

func livePartialOnRuntime(t *testing.T, r *Runtime, interval time.Duration) (*Runtime, *taskPartial) {
	t.Helper()
	ctx := context.Background()
	c, err := r.OpenRoot(ctx, "live-preview", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	snap := mustSnap(t, r)
	task := Task{ID: "generation", ConversationID: c.ID, Invocation: "attempt", State: "running", Revision: snap.Revision() + 1}
	if err := r.commit(ctx, snap, "test", record(taskKey(task.ID), task)); err != nil {
		t.Fatal(err)
	}
	tc := TaskContext{inv: &taskInvocation{s: &TaskScheduler{r: r}, task: task}}
	p := newTaskPartial(tc, interval, Partial{ConversationID: c.ID, RunID: "run", Turn: 1, Attempt: 1}, newEventGate(func(core.AgentEvent) {}))
	t.Cleanup(p.stop)
	return r, p
}

func TestLivePartialDefaultPersistenceAndFence(t *testing.T) {
	r, p := livePartialFixture(t, 0)
	if p.interval != time.Second {
		t.Fatalf("checkpoint interval = %v", p.interval)
	}
	// Keep the loop dormant so this test controls checkpoint timing.
	p.mu.Lock()
	p.buf.WriteString("answer")
	p.reasoning, p.reasoningBytes, p.dirty = "thinking", 8, true
	p.mu.Unlock()
	ctx := context.Background()
	id := p.base.ConversationID
	preview, ok, _, err := r.livePartial(ctx, id)
	if err != nil || !ok || preview.Text != "answer" || preview.Reasoning != "thinking" {
		t.Fatalf("RAM preview = %+v, %v, %v", preview, ok, err)
	}
	if _, ok, err := r.Partial(ctx, id); err != nil || ok {
		t.Fatalf("preview persisted before checkpoint: %v, %v", ok, err)
	}
	p.flush()
	if saved, ok, err := r.Partial(ctx, id); err != nil || !ok || saved.Text != preview.Text || saved.Reasoning != preview.Reasoning {
		t.Fatalf("checkpoint = %+v, %v, %v", saved, ok, err)
	}
	// Even if a stale invocation remains in RAM, it cannot preview output.
	snap := mustSnap(t, r)
	task, _, _ := read[Task](snap, taskKey("generation"))
	task.Invocation = "replacement"
	if err := r.commit(ctx, snap, "replace", record(taskKey(task.ID), task)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _, err := r.livePartial(ctx, id); err != nil || ok {
		t.Fatalf("stale invocation previewed: %v, %v", ok, err)
	}
}

func TestLiveWatchOrdersFinalAfterCapturedPreview(t *testing.T) {
	r, p := livePartialFixture(t, time.Hour)
	p.add("preview", core.EvTextDelta{Delta: "preview"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := errors.New("finished")
	var order []string
	finalized := false
	err := r.watchLive(ctx, p.base.ConversationID, 0, func(cm storage.Commit) error {
		if cm.Actor == "final" {
			order = append(order, "final")
			return finished
		}
		if !finalized {
			finalized = true
			// Finish after the watch captured RAM but before it has drained
			// the durable history. The final commit must follow the preview.
			r.livePartials.Delete(p.base.ConversationID)
			snap := mustSnap(t, r)
			task, _, _ := read[Task](snap, taskKey("generation"))
			task.State = "terminal"
			return r.commit(ctx, snap, "final", record(taskKey(task.ID), task))
		}
		return nil
	}, func(rec Partial) error {
		if rec.Text != "preview" || rec.PreviewAfter == 0 {
			t.Fatalf("preview = %+v", rec)
		}
		order = append(order, "preview")
		return nil
	})
	if !errors.Is(err, finished) || !reflect.DeepEqual(order, []string{"preview", "final"}) {
		t.Fatalf("order=%v, error=%v", order, err)
	}
}

func TestPartialStreamDoesNotRewindOnCheckpoint(t *testing.T) {
	var stream partialStream
	var text, reasoning string
	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvTextDelta:
			text += e.Delta
		case core.EvReasoningDelta:
			reasoning += e.Delta
		}
	}
	preview := Partial{RunID: "run", Turn: 1, Attempt: 1, Text: "hello world", Reasoning: "thinking now", ReasoningBytes: 12}
	stream.apply(preview, sink)
	checkpoint := preview
	checkpoint.Text, checkpoint.Reasoning, checkpoint.ReasoningBytes = "hello", "thinking", 8
	stream.apply(checkpoint, sink)
	stream.apply(preview, sink)
	preview.Text += "!"
	preview.Reasoning, preview.ReasoningBytes = "now done", 17
	stream.apply(preview, sink)
	if text != "hello world!" || reasoning != "thinking now done" {
		t.Fatalf("rewound streams: text=%q reasoning=%q", text, reasoning)
	}
}

func TestAttachedSessionIgnoresPreviewBeforeRefreshedSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch := make(chan storage.Commit, 2)
	for _, p := range []Partial{
		{ConversationID: "c", RunID: "old", Text: "stale", PreviewAfter: 4},
		{ConversationID: "c", RunID: "new", Text: "current", PreviewAfter: 5},
	} {
		watch <- storage.Commit{Operations: []storage.Operation{record(partialKey("c"), p)}}
	}
	s := &AttachedSession{
		driver: &AttachedDriver{ConversationID: "c"},
		agent:  core.NewAgent(nil, "test", "", nil),
		ctx:    ctx, cancel: cancel, watch: watch,
		snapshot: ConversationSnapshot{Revision: 5},
		ready:    make(chan struct{}), done: make(chan struct{}),
	}
	var deltas []string
	err := s.Observe(ctx, func(ev core.AgentEvent) {
		if d, ok := ev.(core.EvTextDelta); ok {
			deltas = append(deltas, d.Delta)
			if d.Delta == "current" {
				cancel()
			}
		}
	}, func() {})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(deltas, []string{"current"}) {
		t.Fatalf("stale preview after refresh: %v, %v", deltas, err)
	}
}

func TestAttachedRAMPreviewWithoutCheckpoint(t *testing.T) {
	testAttachedRAMPreviewWithoutCheckpoint(t, newTestRuntime)
}

func testAttachedRAMPreviewWithoutCheckpoint(t *testing.T, newRuntime func(*testing.T) *Runtime) {
	t.Helper()
	for _, mode := range []string{"session", "driver"} {
		for _, reasoning := range []string{"", "live reasoning"} {
			t.Run(mode+"/"+reasoning, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				r := newRuntime(t)
				release := make(chan struct{})
				engine := echoEngine(gatedAttachedClient{release: release, reasoning: reasoning}, nil)
				// An hour-long checkpoint interval makes live delivery before
				// persistence deterministic without a wall-clock assertion.
				host, err := NewHost(r, engine, HostOptions{Execution: ExecutionOptions{PartialFlushInterval: time.Hour}})
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- host.Run(ctx) }()
				defer func() { cancel(); <-done }()
				client := dialClient(t, &HostServer{Host: host, Engine: engine})
				conv, err := r.OpenRoot(ctx, "ram", AgentConfig{})
				if err != nil {
					t.Fatal(err)
				}
				durableClient := dialClient(t, &HostServer{Host: host, Engine: engine})
				durable, err := durableClient.Watch(ctx, conv.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				updates := make(chan string, 8)
				var driverDone chan error
				if mode == "session" {
					o := startObservedSession(t, ctx, client, conv.ID)
					updates = o.deltas
					if reasoning != "" {
						updates = o.reasoning
					}
					if _, err := r.Submit(ctx, conv.ID, "external", "", "go"); err != nil {
						t.Fatal(err)
					}
				} else {
					driverDone = make(chan error, 1)
					driver := &AttachedDriver{Client: client, ConversationID: conv.ID}
					go func() {
						driverDone <- driver.Prompt(ctx, core.NewAgent(nil, "test", "", nil), "go", func(ev core.AgentEvent) {
							switch e := ev.(type) {
							case core.EvTextDelta:
								if reasoning == "" {
									updates <- e.Delta
								}
							case core.EvReasoningDelta:
								updates <- e.Delta
							}
						})
					}()
				}
				want := "live remote text"
				if reasoning != "" {
					want = reasoning
				}
				await := func(ch <-chan string) {
					t.Helper()
					select {
					case got := <-ch:
						if got != want {
							t.Fatalf("preview = %q, want %q", got, want)
						}
					case <-ctx.Done():
						t.Fatal("preview waited for persistence", ctx.Err())
					}
				}
				await(updates)
				if _, ok, err := r.Partial(ctx, conv.ID); err != nil || ok {
					t.Fatalf("RAM preview was persisted: %v, %v", ok, err)
				}
				late := startObservedSession(t, ctx, client, conv.ID)
				lateUpdates := late.deltas
				if reasoning != "" {
					lateUpdates = late.reasoning
				}
				await(lateUpdates)
				// Rewatch restores RAM even with no new provider delta and no
				// persisted partial. It must not depend on a future checkpoint.
				dropWatches(client)
				await(lateUpdates)
				close(release)
				observedAnswer(t, ctx, late, "live remote text")
				// Commit-only clients never receive RAM previews, and the
				// final response persists without waiting for the hour timer.
				answered := false
				for !answered {
					select {
					case cm, ok := <-durable:
						if !ok || cm.Revision == 0 {
							t.Fatal("invalid durable watch update")
						}
						if _, ok := PartialFromCommit(cm, conv.ID); ok {
							t.Fatal("durable watcher received a RAM preview")
						}
						for _, entry := range EntriesFromCommit(cm, conv.ID) {
							answered = answered || entry.Type == entryAssistant
						}
					case <-ctx.Done():
						t.Fatal("final response waited for checkpoint timer", ctx.Err())
					}
				}
				if driverDone != nil {
					select {
					case err := <-driverDone:
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				if _, ok := r.livePartials.Load(conv.ID); ok {
					t.Fatal("completed attempt retained RAM preview")
				}
				if _, ok, err := r.Partial(ctx, conv.ID); err != nil || ok {
					t.Fatal("completed attempt retained checkpoint", err)
				}
			})
		}
	}
}
