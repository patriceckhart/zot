package continuous

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
)

func TestAttachedReasoningStreamsBeforeAnswerAndRestores(t *testing.T) {
	for _, mode := range []string{"session", "driver"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r := newTestRuntime(t)
			release := make(chan struct{})
			engine := echoEngine(gatedAttachedClient{release: release, reasoning: "live remote reasoning"}, nil)
			host, err := NewHost(r, engine, HostOptions{Execution: ExecutionOptions{PartialFlushInterval: time.Millisecond}})
			if err != nil {
				t.Fatal(err)
			}
			hostDone := make(chan error, 1)
			go func() { hostDone <- host.Run(ctx) }()
			defer func() { cancel(); <-hostDone }()
			client := dialClient(t, &HostServer{Host: host, Engine: engine})
			var conv Conversation
			if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
				t.Fatal(err)
			}
			reasoning := make(chan string, 16)
			var observer observedSession
			var driverDone chan error
			if mode == "session" {
				observer = startObservedSession(t, ctx, client, conv.ID)
				reasoning = observer.reasoning
				if _, err := r.Submit(ctx, conv.ID, "external", "remote", "go"); err != nil {
					t.Fatal(err)
				}
			} else {
				driverDone = make(chan error, 1)
				driver := &AttachedDriver{Client: client, ConversationID: conv.ID}
				go func() {
					driverDone <- driver.Prompt(ctx, core.NewAgent(nil, "scripted", "", nil), "go", func(event core.AgentEvent) {
						if e, ok := event.(core.EvReasoningDelta); ok {
							reasoning <- e.Delta
						}
					})
				}()
			}
			select {
			case text := <-reasoning:
				if text != "live remote reasoning" {
					t.Fatalf("reasoning=%q", text)
				}
			case <-ctx.Done():
				t.Fatal("attached client did not receive live reasoning", ctx.Err())
			}
			// RAM delivery can precede the checkpoint even with a short
			// persistence interval. Wait for its independent durable write.
			checkpoint := errors.New("checkpoint observed")
			if err := r.Watch(ctx, conv.ID, 0, func(cm storage.Commit) error {
				if _, ok := PartialFromCommit(cm, conv.ID); ok {
					return checkpoint
				}
				return nil
			}); !errors.Is(err, checkpoint) {
				t.Fatal("reasoning checkpoint missing", err)
			}
			p, ok, err := r.Partial(ctx, conv.ID)
			if err != nil || !ok || p.Text != "" || p.Reasoning != "live remote reasoning" {
				t.Fatalf("reasoning-only partial=%+v, %v", p, err)
			}
			// A view joining while the provider is still thinking restores the tail.
			late := startObservedSession(t, ctx, client, conv.ID)
			select {
			case text := <-late.reasoning:
				if text != "live remote reasoning" {
					t.Fatalf("restored reasoning=%q", text)
				}
			case <-ctx.Done():
				t.Fatal("late view did not restore reasoning", ctx.Err())
			}
			close(release)
			observedAnswer(t, ctx, late, "live remote text")
			assertSavedReasoning := func(view *core.Agent) {
				t.Helper()
				for _, msg := range view.Messages() {
					if msg.ReadableReasoning() == "live remote reasoning" {
						return
					}
				}
				t.Fatal("committed assistant message lost its readable reasoning")
			}
			assertSavedReasoning(late.view)
			if mode == "session" {
				observedAnswer(t, ctx, observer, "live remote text")
			} else {
				select {
				case err := <-driverDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if _, ok, err := r.Partial(ctx, conv.ID); err != nil || ok {
				t.Fatal("reasoning partial survived completion", err)
			}
			restored := startObservedSession(t, ctx, client, conv.ID)
			assertSavedReasoning(restored.view)
		})
	}
}
