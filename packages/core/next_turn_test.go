package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestBeforeNextTurnRunsAfterToolResultsBeforeQueuedMessages(t *testing.T) {
	client := &queueFakeClient{}
	tool := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	close(tool.release)
	a := NewAgent(client, "model", "", NewRegistry(tool))
	calls := 0
	a.BeforeNextTurn = func(ctx context.Context) error {
		calls++
		msgs := a.Messages()
		if len(msgs) != 3 {
			t.Fatalf("boundary messages = %d, want prompt, assistant, and tool results", len(msgs))
		}
		call := msgs[1].Content[1].(provider.ToolCallBlock)
		result := msgs[2].Content[0].(provider.ToolResultBlock)
		if call.ID != result.CallID {
			t.Fatal("boundary reached before complete tool batch")
		}
		// A host can replace context here without losing pending user work.
		a.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "summary"}}}})
		a.QueueMessage("follow up")
		return nil
	}
	if err := a.Prompt(context.Background(), "work", nil, nil); err != nil {
		t.Fatal(err)
	}
	msgs := a.Messages()
	if calls != 1 || len(msgs) != 3 || extractText(msgs[0]) != "summary" || extractText(msgs[1]) != "follow up" {
		t.Fatalf("hook calls=%d transcript=%+v", calls, msgs)
	}
}

func TestBeforeNextTurnFailureAndCancellationStopRequests(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "failure"
		if cancelled {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			client := &queueFakeClient{}
			tool := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
			close(tool.release)
			a := NewAgent(client, "model", "", NewRegistry(tool))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("boundary failed")
			a.BeforeNextTurn = func(context.Context) error {
				if cancelled {
					cancel()
					return nil
				}
				return want
			}
			if cancelled {
				want = context.Canceled
			}
			done := 0
			err := a.Prompt(ctx, "work", nil, func(ev AgentEvent) {
				if _, ok := ev.(EvDone); ok {
					done++
				}
			})
			if !errors.Is(err, want) || atomic.LoadInt32(&client.calls) != 1 || done != 1 {
				t.Fatalf("error=%v requests=%d done=%d", err, client.calls, done)
			}
		})
	}
}

func TestBeforeNextTurnSkipsStepLimit(t *testing.T) {
	client := &queueFakeClient{}
	a := NewAgent(client, "model", "", nil)
	a.MaxSteps = 1
	a.BeforeNextTurn = func(context.Context) error {
		t.Fatal("hook ran with no next response")
		return nil
	}
	if err := a.Prompt(context.Background(), "work", nil, nil); err == nil {
		t.Fatal("expected step limit error")
	}
}
