package modes

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

type turnCompactionClient struct {
	requests   []provider.Request
	compactErr error
	cancel     context.CancelFunc
	onCompact  func()
}

func (*turnCompactionClient) Name() string { return "turn-compaction-test" }
func (c *turnCompactionClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	out := make(chan provider.Event, 3)
	if req.MaxTokens == 4096 {
		if c.onCompact != nil {
			c.onCompact()
		}
		if c.cancel != nil {
			c.cancel()
		}
		out <- provider.EventTextDelta{Delta: "summary"}
		out <- provider.EventDone{Stop: provider.StopEnd, Err: c.compactErr}
	} else if len(c.requests) == 1 {
		out <- provider.EventUsage{Usage: provider.Usage{InputTokens: 170000}}
		out <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "skill", Arguments: []byte(`{}`)}},
		}}
	} else {
		out <- provider.EventUsage{Usage: provider.Usage{InputTokens: 100}}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}}
	}
	close(out)
	return out, nil
}

func waitTurnCompactionIdle(t *testing.T, i *Interactive) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		i.mu.Lock()
		busy := i.busy
		i.mu.Unlock()
		if !busy {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("agent run did not settle")
		case <-poll.C:
		}
	}
}

func TestAutoCompactionBetweenToolBatches(t *testing.T) {
	for _, threshold := range []int{80, 0, 90} {
		t.Run(strconv.Itoa(threshold), func(t *testing.T) {
			client := &turnCompactionClient{}
			agent := core.NewAgent(client, "test-model", "", core.NewRegistry(&shortcutTool{}))
			i := NewInteractive(InteractiveConfig{Agent: agent, Provider: "anthropic", Model: "claude-sonnet-4-5", AutoCompactThreshold: &threshold})
			client.onCompact = func() {
				i.mu.Lock()
				active := i.busy && i.compacting && i.autoCompacting && i.cancelTurn != nil
				i.mu.Unlock()
				if !active {
					t.Error("compaction released active run state")
				}
				i.ed.SetValue("queued during compaction")
				i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
			}
			var checkpoint []provider.Message
			agent.OnTranscriptCompacted = func(messages []provider.Message) { checkpoint = messages }
			previousHookCalls := 0
			agent.BeforeNextTurn = func(context.Context) error {
				previousHookCalls++
				return nil
			}
			i.startTurn(context.Background(), "work")
			waitTurnCompactionIdle(t, i)

			wantCalls := 2
			if threshold == 80 {
				wantCalls = 3
			}
			if len(client.requests) != wantCalls {
				t.Fatalf("requests = %d, want %d", len(client.requests), wantCalls)
			}
			if previousHookCalls != 1 {
				t.Fatalf("existing hook calls = %d, want 1", previousHookCalls)
			}
			if threshold == 80 {
				if len(checkpoint) == 0 || checkpoint[0].Meta["compaction"] != "true" {
					t.Fatal("compaction checkpoint not persisted")
				}
				if got := requestUserTextCount(client.requests[2], "queued during compaction"); got != 1 {
					t.Fatalf("queued prompt count = %d, want 1", got)
				}
				next := client.requests[2].Messages
				if next[0].Meta["compaction"] != "true" {
					t.Fatal("next model response did not use compacted context")
				}
				call := next[1].Content[0].(provider.ToolCallBlock)
				result := next[2].Content[0].(provider.ToolResultBlock)
				if call.ID != result.CallID || result.Content[0].(provider.TextBlock).Text != "instructions" {
					t.Fatal("tool batch was not preserved before compaction")
				}
			} else if checkpoint != nil {
				t.Fatal("disabled or below-threshold run compacted")
			}
			i.mu.Lock()
			defer i.mu.Unlock()
			if i.compacting || i.autoCompacting || i.statusErr != "" {
				t.Fatalf("unexpected final state: compacting=%t auto=%t error=%q", i.compacting, i.autoCompacting, i.statusErr)
			}
		})
	}
}

func TestBetweenTurnCompactionFailureStopsRun(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancelled"}[cancelled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &turnCompactionClient{compactErr: errors.New("summary unavailable")}
			if cancelled {
				client.compactErr = nil
				client.cancel = cancel
			}
			agent := core.NewAgent(client, "test-model", "", core.NewRegistry(&shortcutTool{}))
			threshold := 80
			i := NewInteractive(InteractiveConfig{Agent: agent, Provider: "anthropic", Model: "claude-sonnet-4-5", AutoCompactThreshold: &threshold})
			var before []provider.Message
			agent.OnMessageAppended = func(provider.Message) { before = agent.Messages() }
			i.startTurn(ctx, "work")
			waitTurnCompactionIdle(t, i)
			if len(client.requests) != 2 {
				t.Fatalf("requests = %d, want initial response and summary only", len(client.requests))
			}
			if !reflect.DeepEqual(before, agent.Messages()) {
				t.Fatal("failed or cancelled compaction changed transcript")
			}
			i.mu.Lock()
			defer i.mu.Unlock()
			if i.compacting || i.autoCompacting || (!cancelled && i.statusErr == "") {
				t.Fatal("compaction did not clean up or surface failure")
			}
		})
	}
}
