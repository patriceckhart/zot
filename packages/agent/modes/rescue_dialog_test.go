package modes

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestRescueDialogRetryWithoutAlternatives(t *testing.T) {
	d := newRescueDialog()
	d.catalog = func() []provider.Model { return nil }
	d.Open("custom-model", nil, "custom", "custom-model", "network failure", "prompt")
	if rendered := strings.Join(d.Render(tui.Dark, 90), "\n"); !strings.Contains(rendered, "Retry current model") {
		t.Fatalf("retry action missing: %s", rendered)
	}
	for _, kind := range []tui.KeyKind{tui.KeyDown, tui.KeyUp} {
		d.HandleKey(tui.Key{Kind: kind})
	}
	act := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !act.Retry || act.Select || act.Close || d.Active() {
		t.Fatalf("retry action = %+v, active = %v", act, d.Active())
	}
}

func TestRescueDialogModelSelectionAndFiltering(t *testing.T) {
	d := newRescueDialog()
	d.catalog = func() []provider.Model {
		return []provider.Model{
			{Provider: "custom", ID: "failed"},
			{Provider: "custom", ID: "alternative", DisplayName: "Alternative"},
		}
	}
	open := func() { d.Open("failed", []string{"custom"}, "custom", "failed", "failure", "prompt") }
	open()
	if len(d.view) != 1 || d.cursor != 0 {
		t.Fatalf("models = %+v, cursor = %d", d.view, d.cursor)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	act := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !act.Select || act.Retry || act.Provider != "custom" || act.Model != "alternative" || act.Prompt != "prompt" {
		t.Fatalf("model selection = %+v", act)
	}
	open()
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "alternative"})
	if act := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); !act.Select || act.Model != "alternative" {
		t.Fatalf("filtered selection = %+v", act)
	}
	open()
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "no match"})
	if act := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); !act.Retry {
		t.Fatalf("retry unavailable with empty filter results: %+v", act)
	}
	open()
	if act := d.HandleKey(tui.Key{Kind: tui.KeyEsc}); !act.Close || act.Retry || act.Select || d.Active() {
		t.Fatalf("cancel = %+v", act)
	}
}

type rescueRetryClient struct {
	responses []provider.EventDone
	requests  chan provider.Request
	calls     atomic.Int32
}

func (*rescueRetryClient) Name() string { return "custom" }
func (c *rescueRetryClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	index := int(c.calls.Add(1)) - 1
	c.requests <- req
	if index >= len(c.responses) {
		return nil, errors.New("unexpected provider request")
	}
	out := make(chan provider.Event, 1)
	out <- c.responses[index]
	close(out)
	return out, nil
}

func waitRescueDialog(t *testing.T, i *Interactive) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		i.mu.Lock()
		ready := !i.busy && i.rescueDialog.Active()
		i.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("rescue dialog did not open")
		case <-poll.C:
		}
	}
}

func TestRescueRetryContinuesWithExistingClient(t *testing.T) {
	for _, tc := range []struct {
		name           string
		tools, partial bool
	}{
		{name: "before response"},
		{name: "after completed tool", tools: true},
		{name: "partial tool response", tools: true, partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := provider.EventDone{Stop: provider.StopError, Err: errors.New("connection reset")}
			if tc.partial {
				failure.Message = provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
					provider.TextBlock{Text: "partial"},
					provider.ToolCallBlock{ID: "unfinished", Name: "effect", Arguments: []byte(`{}`)},
				}}
			}
			client := &rescueRetryClient{
				requests: make(chan provider.Request, 8),
				responses: []provider.EventDone{
					{Stop: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
						provider.ToolCallBlock{ID: "completed", Name: "effect", Arguments: []byte(`{}`)},
					}}},
					failure,
					{Stop: provider.StopError, Err: errors.New("connection refused")},
					{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}},
				},
			}
			if !tc.tools {
				client.responses = client.responses[1:]
			}
			tool := &attachedExecutionTool{}
			agent := core.NewAgent(client, "custom-model", "system", core.NewRegistry(tool))
			agent.MaxRetries = 0
			i := NewInteractive(InteractiveConfig{
				Agent: agent, Provider: "custom", Model: "custom-model", Theme: tui.Dark,
				ModelCatalog: func() []provider.Model { return nil },
				BuildAgentForRescue: func(string, string) (*core.Agent, string, string, error) {
					t.Error("retry rebuilt the client, losing custom credentials and endpoint")
					return nil, "", "", errors.New("must keep existing client")
				},
				PersistModel: func(string, string) { t.Error("retry persisted a model change") },
			})
			image := provider.ImageBlock{MimeType: "image/png", Data: []byte("image-data")}
			i.startTurnWithImages(context.Background(), "retry me", []provider.ImageBlock{image})
			waitRescueDialog(t, i)
			i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
			waitRescueDialog(t, i)
			i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
			waitTurnCompactionIdle(t, i)

			if i.agent != agent || agent.Client != client || agent.Model != "custom-model" || i.cfg.Provider != "custom" || i.cfg.Model != "custom-model" {
				t.Fatal("retry changed agent, client, provider, or model")
			}
			wantTools := int32(0)
			if tc.tools {
				wantTools = 1
			}
			if got := tool.calls.Load(); got != wantTools {
				t.Fatalf("tool executed %d times, want %d", got, wantTools)
			}
			if got := client.calls.Load(); got != int32(len(client.responses)) {
				t.Fatalf("provider calls = %d, want %d", got, len(client.responses))
			}
			for call := 0; call < len(client.responses); call++ {
				req := <-client.requests
				if got := requestUserTextCount(req, "retry me"); got != 1 {
					t.Fatalf("call %d contains original prompt %d times", call, got)
				}
				if got := requestImageCount(req, image); got != 1 {
					t.Fatalf("call %d contains image %d times", call, got)
				}
				if !tc.tools || call < 2 {
					continue
				}
				calls, results := map[string]bool{}, map[string]bool{}
				for _, msg := range req.Messages {
					for _, block := range msg.Content {
						switch b := block.(type) {
						case provider.ToolCallBlock:
							calls[b.ID] = true
						case provider.ToolResultBlock:
							results[b.CallID] = true
						}
					}
				}
				if !calls["completed"] || !results["completed"] || len(calls) != len(results) {
					t.Fatalf("call %d lost tool pairing: calls=%v, results=%v", call, calls, results)
				}
				for id := range calls {
					if !results[id] {
						t.Fatalf("call %d has unmatched tool call %q", call, id)
					}
				}
			}
			i.mu.Lock()
			defer i.mu.Unlock()
			if i.pendingRescuePrompt != "" || len(i.pendingRescueImages) != 0 || i.rescueDialog.Active() || i.statusErr != "" {
				t.Fatal("successful retry retained pending rescue state or error")
			}
		})
	}
}

func TestRescueRetryPreTurnCompactionContinuesExistingInput(t *testing.T) {
	client := &compactQueueClient{
		compactionStarted: make(chan struct{}),
		releaseCompaction: make(chan struct{}),
		followUpRequest:   make(chan provider.Request, 1),
	}
	agent := core.NewAgent(client, "test-model", "", nil)
	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("image-data")}
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "one"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "two"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "three"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "four"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "retry me"}, image}},
	})
	threshold := 70
	i := NewInteractive(InteractiveConfig{
		Agent: agent, Provider: "anthropic", Model: "claude-sonnet-4-5", Theme: tui.Dark,
		AutoCompactThreshold: &threshold,
	})
	i.lastCtxInput = 150000
	i.openRescueDialog("anthropic", "anthropic", "claude-sonnet-4-5", "network failure", "retry me", []provider.ImageBlock{image})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	select {
	case <-client.compactionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not compact before continuing")
	}
	close(client.releaseCompaction)
	select {
	case req := <-client.followUpRequest:
		if got := requestUserTextCount(req, "retry me"); got != 1 {
			t.Fatalf("compacted retry contains prompt %d times", got)
		}
		if got := requestImageCount(req, image); got != 1 {
			t.Fatalf("compacted retry contains image %d times", got)
		}
		for _, msg := range req.Messages {
			if msg.Role == provider.RoleUser && len(msg.Content) == 0 {
				t.Fatal("compacted retry appended an empty user message")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not continue after compaction")
	}
	waitTurnCompactionIdle(t, i)
}

func TestRescueCancelDoesNotRetry(t *testing.T) {
	for _, key := range []tui.KeyKind{tui.KeyEsc, tui.KeyCtrlC} {
		i := NewInteractive(InteractiveConfig{Theme: tui.Dark})
		i.openRescueDialog("custom", "custom", "model", "network failure", "prompt", nil)
		i.handleKey(context.Background(), tui.Key{Kind: key})
		if i.rescueDialog.Active() || i.busy {
			t.Fatalf("cancel key %v did not dismiss rescue", key)
		}
	}
}
