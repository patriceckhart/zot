package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestModelOperationsUseGuardsAndLifecycle(t *testing.T) {
	var called atomic.Int32
	tool := &Tool{Models: func(context.Context, string, json.RawMessage) (json.RawMessage, *provider.Usage, error) {
		called.Add(1)
		return json.RawMessage(`{"model":"synthetic","stopReason":"error","errorMessage":"inference failed","answers":{}}`), &provider.Usage{InputTokens: 3}, nil
	}}
	ag := core.NewAgent(nil, "test", "", core.NewRegistry(tool))
	var events []core.AgentEvent
	raw := json.RawMessage(`{"code":"return (await models.classify({},{})).stopReason"}`)
	res := ag.CallTool(context.Background(), "script", "codemode", raw, func(ev core.AgentEvent) { events = append(events, ev) })
	if res.IsError || !strings.Contains(resultText(res), "error") || ag.Cost().InputTokens != 3 {
		t.Fatalf("%+v", res)
	}
	var call, result bool
	for _, ev := range events {
		switch e := ev.(type) {
		case core.EvToolCall:
			call = call || e.Name == "models.classify" && e.ID == "script/1"
		case core.EvToolResult:
			result = result || e.Name == "models.classify" && e.ID == "script/1" && e.Result.IsError && e.Executed
		}
	}
	if !call || !result {
		t.Fatalf("model lifecycle missing: %+v", events)
	}
	ag.BeforeToolExecuteContext = func(_ context.Context, call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return call.Name != "models.classify", "model operation denied", nil
	}
	res = ag.CallTool(context.Background(), "blocked", "codemode", raw, nil)
	if !res.IsError || !strings.Contains(resultText(res), "model operation denied") || called.Load() != 1 {
		t.Fatalf("%+v calls=%d", res, called.Load())
	}
}

func TestGeneratedImagesMustBeEmitted(t *testing.T) {
	tool := &Tool{Models: func(context.Context, string, json.RawMessage) (json.RawMessage, *provider.Usage, error) {
		return json.RawMessage(`{"model":"synthetic","stopReason":"stop","output":[{"type":"image","mimeType":"image/png","data":"iVBORw0KGgo="}]}`), nil, nil
	}}
	for _, emit := range []bool{false, true} {
		ag := core.NewAgent(nil, "test", "", core.NewRegistry(tool))
		code := `const r=await models.generateImages({},{});`
		if emit {
			code += `image(r.output[0])`
		}
		res := runScript(t, ag, code, 2000)
		if res.IsError || strings.Contains(resultText(res), "that the script did not show") == emit {
			t.Fatalf("emit=%v result=%+v", emit, res)
		}
	}
}
