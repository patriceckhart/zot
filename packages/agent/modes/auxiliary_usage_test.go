package modes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type auxiliaryUsageTool struct{}

func (auxiliaryUsageTool) Name() string            { return "inference" }
func (auxiliaryUsageTool) Description() string     { return "Synthetic inference" }
func (auxiliaryUsageTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (auxiliaryUsageTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Usage: &provider.Usage{InputTokens: 3, CostUSD: 0.01}}, nil
}

func TestAuxiliaryUsagePreservesInteractiveChatContext(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	chatUsage := provider.Usage{InputTokens: 100, CacheReadTokens: 20}
	i.handleEvent(core.EvUsage{Usage: chatUsage})
	ag := core.NewAgent(nil, "test", "", core.NewRegistry(auxiliaryUsageTool{}))
	ag.SeedCost(chatUsage)
	ag.SeedLastTurnUsage(chatUsage)
	var wire map[string]any
	res := ag.CallTool(context.Background(), "inference", "inference", json.RawMessage(`{}`), func(ev core.AgentEvent) {
		i.handleEvent(ev)
		if _, ok := ev.(core.EvUsage); ok {
			wire = EventToJSON(ev)
		}
	})
	if res.IsError {
		t.Fatal(res)
	}
	if i.lastCtxInput != 120 || i.cumUsage.InputTokens != 103 || i.cumUsage.CostUSD != 0.01 {
		t.Fatalf("context=%d cumulative=%+v", i.lastCtxInput, i.cumUsage)
	}
	if ag.LastTurnUsage() != chatUsage {
		t.Fatalf("auxiliary usage replaced the last chat turn: %+v", ag.LastTurnUsage())
	}
	if wire["auxiliary"] != true {
		t.Fatalf("auxiliary usage was not identified on the wire: %v", wire)
	}
	if _, exists := EventToJSON(core.EvUsage{})["auxiliary"]; exists {
		t.Fatal("ordinary usage gained an unnecessary wire field")
	}
}
