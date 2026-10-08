package continuous

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestAttachedDriverRefreshesBetweenPrompts(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "another client"
		if detached {
			name = "detached prompt"
		}
		t.Run(name, func(t *testing.T) {
			tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
			steps := []scriptStep{{text: "first answer"}, {text: "second answer"}}
			if detached {
				steps = append([]scriptStep{{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}}, steps...)
			}
			r, dial, hostCtx := attachHarness(t, tool, steps)
			ctx, cancel := context.WithTimeout(hostCtx, 5*time.Second)
			defer cancel()
			c := dial()
			var conv Conversation
			if err := c.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
				t.Fatal(err)
			}
			d := &AttachedDriver{Client: c, ConversationID: conv.ID}
			view := core.NewAgent(nil, "scripted", "", core.NewRegistry())
			if _, err := d.Load(ctx, view); err != nil {
				t.Fatal(err)
			}
			var subID string
			if detached {
				promptCtx, detach := context.WithCancel(ctx)
				defer detach()
				done := make(chan error, 1)
				go func() { done <- d.Prompt(promptCtx, view, "first", func(core.AgentEvent) {}) }()
				select {
				case <-tool.started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				run, _, err := r.Run(ctx, conv.ID)
				if err != nil || len(run.Submissions) != 1 {
					t.Fatalf("run: %+v, %v", run, err)
				}
				subID = run.Submissions[0]
				detach()
				if err := <-done; !errors.Is(err, ErrDetached) {
					t.Fatalf("detach: %v", err)
				}
				close(tool.block)
			} else {
				sub, err := r.Submit(ctx, conv.ID, "other", "first", "first")
				if err != nil {
					t.Fatal(err)
				}
				subID = sub.ID
			}
			if _, err := r.WaitSubmission(ctx, subID); err != nil {
				t.Fatal(err)
			}
			if err := d.Prompt(ctx, view, "second", func(core.AgentEvent) {}); err != nil {
				t.Fatal(err)
			}
			snap, err := r.ConversationSnapshot(ctx, conv.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			want := MessagesFromEntries(snap.Entries)
			got := view.Messages()
			if len(got) != len(want) {
				t.Fatalf("mirrored messages=%d, committed messages=%d", len(got), len(want))
			}
			found := false
			for _, msg := range got {
				found = found || core.MessageText(msg) == "first answer"
			}
			if !found {
				t.Fatal("missing answer completed while detached or idle")
			}
		})
	}
}
