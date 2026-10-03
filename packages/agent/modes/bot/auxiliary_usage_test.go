package bot

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type auxiliaryUsageClient struct{}

func (auxiliaryUsageClient) Name() string { return "test" }
func (auxiliaryUsageClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	events := make(chan provider.Event, 2)
	events <- provider.EventUsage{Usage: provider.Usage{InputTokens: 100, CacheReadTokens: 20}}
	events <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{ID: "inference", Name: "inference", Arguments: json.RawMessage(`{}`)}},
	}}
	close(events)
	return events, nil
}

type auxiliaryUsageTool struct{}

func (auxiliaryUsageTool) Name() string            { return "inference" }
func (auxiliaryUsageTool) Description() string     { return "Synthetic inference" }
func (auxiliaryUsageTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (auxiliaryUsageTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Usage: &provider.Usage{InputTokens: 3, CostUSD: 0.01}}, nil
}

func TestAuxiliaryUsagePreservesBotChatContext(t *testing.T) {
	ag := core.NewAgent(auxiliaryUsageClient{}, "test", "", core.NewRegistry(auxiliaryUsageTool{}))
	// End after tool execution so no later chat usage can mask a bad update.
	ag.MaxSteps = 1
	r := NewRunner(testAdapter{}, ag, Config{})
	r.runTurn(context.Background(), queuedTurn{channelID: "test", prompt: "test"})
	if r.lastCtxInput != 120 || ag.Cost().InputTokens != 103 || ag.Cost().CostUSD != 0.01 {
		t.Fatalf("context=%d cost=%+v", r.lastCtxInput, ag.Cost())
	}
}
