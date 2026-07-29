package modes

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

type compactQueueClient struct {
	compactionStarted chan struct{}
	releaseCompaction chan struct{}
	followUpRequest   chan provider.Request

	mu    sync.Mutex
	calls int
}

func (c *compactQueueClient) Name() string { return "compact-queue-test" }

func (c *compactQueueClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()

	out := make(chan provider.Event, 2)
	go func() {
		defer close(out)
		if call == 1 {
			close(c.compactionStarted)
			select {
			case <-ctx.Done():
				out <- provider.EventDone{Stop: provider.StopAborted, Err: ctx.Err()}
			case <-c.releaseCompaction:
				out <- provider.EventTextDelta{Delta: "summary"}
				out <- provider.EventDone{Stop: provider.StopEnd}
			}
			return
		}

		c.followUpRequest <- req
		out <- provider.EventDone{
			Stop: provider.StopEnd,
			Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "done"}},
			},
		}
	}()
	return out, nil
}

func TestPromptSubmittedDuringCompactionStartsFollowUpTurn(t *testing.T) {
	client := &compactQueueClient{
		compactionStarted: make(chan struct{}),
		releaseCompaction: make(chan struct{}),
		followUpRequest:   make(chan provider.Request, 1),
	}
	agent := core.NewAgent(client, "test-model", "", nil)
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "one"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "two"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "three"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "four"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "five"}}},
	})
	interactive := NewInteractive(InteractiveConfig{Agent: agent})
	interactive.runCtx = context.Background()

	interactive.runCompact(context.Background(), false)
	select {
	case <-client.compactionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("compaction did not start")
	}

	interactive.ed.SetValue("follow up")
	interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	close(client.releaseCompaction)

	select {
	case req := <-client.followUpRequest:
		if !requestContainsUserText(req, "follow up") {
			t.Fatalf("follow-up request does not contain queued prompt: %#v", req.Messages)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued prompt did not start a turn after compaction")
	}
}

type contextRecoveryClient struct {
	mu      sync.Mutex
	calls   int
	retried chan provider.Request
}

func (c *contextRecoveryClient) Name() string { return "context-recovery-test" }

func (c *contextRecoveryClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()

	out := make(chan provider.Event, 2)
	go func() {
		defer close(out)
		switch call {
		case 1:
			out <- provider.EventDone{
				Stop: provider.StopError,
				Err:  errors.New("codex error: Your input exceeds the context window of this model. Please adjust your input and try again."),
			}
		case 2:
			out <- provider.EventTextDelta{Delta: "summary"}
			out <- provider.EventDone{Stop: provider.StopEnd}
		default:
			c.retried <- req
			out <- provider.EventDone{
				Stop: provider.StopEnd,
				Message: provider.Message{
					Role:    provider.RoleAssistant,
					Content: []provider.Content{provider.TextBlock{Text: "done"}},
				},
			}
		}
	}()
	return out, nil
}

func TestContextWindowErrorCompactsAndRetriesPrompt(t *testing.T) {
	client := &contextRecoveryClient{retried: make(chan provider.Request, 1)}
	agent := core.NewAgent(client, "gpt-5.6-sol", "", nil)
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "one"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "two"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "three"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "four"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "five"}}},
	})
	interactive := NewInteractive(InteractiveConfig{Agent: agent})
	interactive.runCtx = context.Background()

	interactive.startTurn(context.Background(), "retry me")

	select {
	case req := <-client.retried:
		if !requestContainsUserText(req, "retry me") {
			t.Fatalf("retried request does not contain original prompt: %#v", req.Messages)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context-window error did not compact and retry the prompt")
	}
}

func requestContainsUserText(req provider.Request, want string) bool {
	for _, message := range req.Messages {
		if message.Role != provider.RoleUser {
			continue
		}
		for _, content := range message.Content {
			if text, ok := content.(provider.TextBlock); ok && text.Text == want {
				return true
			}
		}
	}
	return false
}
