package codemode

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestSourceFormsAndOptions(t *testing.T) {
	for _, raw := range []string{`return 1`, `"return 1"`, `{"code":"return 1"}`} {
		source, err := parseSource(json.RawMessage(raw))
		if err != nil || source.Code != "return 1" || source.Timeout != 0 || source.MaxOutputTokens != 10000 {
			t.Fatalf("%q: %+v %v", raw, source, err)
		}
	}
	source, err := parseSource(json.RawMessage("// @options: {\"max_output_tokens\":0,\"timeout_ms\":2147483647}\nreturn 1"))
	if err != nil || source.MaxOutputTokens != 0 || source.Timeout != 2147483647*time.Millisecond {
		t.Fatalf("%+v %v", source, err)
	}
	for _, options := range []string{`null`, `[]`, `{"timeout_ms":0}`, `{"timeout_ms":1.5}`, `{"max_output_tokens":-1}`, `{"other":1}`, `{"max_output_tokens":"1"}`} {
		if _, err := parseSource(json.RawMessage("// @options: " + options + "\nreturn 1")); err == nil {
			t.Fatalf("accepted %s", options)
		}
	}
}

func TestStoreCopiesRollbackDeleteAndReplay(t *testing.T) {
	ag := newAgent()
	res := runScript(t, ag, `const x={a:1}; store("x",x); x.a=2; const y=load("x"); y.a=3; return load("x")`, 2000)
	if res.IsError || !strings.Contains(resultText(res), `{"a":1}`) || len(res.State["codemode"]) == 0 {
		t.Fatalf("%+v", res)
	}
	snapshot := res.State["codemode"]
	res = runScript(t, ag, `store("x",{a:9}); throw new Error("rollback")`, 2000)
	if !res.IsError || len(res.State) != 0 {
		t.Fatalf("%+v", res)
	}
	res = runScript(t, ag, `return load("x")`, 2000)
	if res.IsError || !strings.Contains(resultText(res), `{"a":1}`) {
		t.Fatalf("%+v", res)
	}
	replay := newAgent()
	replay.SetMessages([]provider.Message{{Role: provider.RoleTool, Meta: map[string]string{"tool_state:codemode": string(snapshot)}}})
	res = runScript(t, replay, `store("x",undefined); return typeof load("x")`, 2000)
	if res.IsError || !strings.Contains(resultText(res), "undefined") || string(res.State["codemode"]) != "{}" {
		t.Fatalf("%+v", res)
	}
	res = runScript(t, ag, `store("done",true); exit(); throw new Error("unreachable")`, 2000)
	if res.IsError || !strings.Contains(string(res.State["codemode"]), `"done":true`) {
		t.Fatalf("%+v", res)
	}
	res = runScript(t, ag, `return load("done")`, 2000)
	if res.IsError || !strings.Contains(resultText(res), "true") {
		t.Fatalf("%+v", res)
	}
}

func TestStoreLimits(t *testing.T) {
	for _, code := range []string{
		`store("x","x".repeat(262143))`,
		`for(let i=0;i<5;i++) store(String(i),"x".repeat(262142))`,
		`store(1,true)`,
		`store("x",()=>1)`,
		`const x={}; x.x=x; store("x",x)`,
	} {
		res := runScript(t, newAgent(), code, 3000)
		if !res.IsError || len(res.State) != 0 {
			t.Fatalf("accepted %s: %+v", code, res)
		}
	}
	res := runScript(t, newAgent(), `store("x","x".repeat(262142)); return load("x").length`, 3000)
	if res.IsError || !strings.Contains(resultText(res), "262142") {
		t.Fatalf("%+v", res)
	}
}

func TestStructuredErrorResolves(t *testing.T) {
	ag := newAgent(testTool{name: "command", outputSchema: json.RawMessage(`{"type":"object"}`), run: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{IsError: true, StructuredContent: json.RawMessage(`{"exit_code":7}`)}, nil
	}})
	res := runScript(t, ag, `return (await tools.command({})).exit_code`, 2000)
	if res.IsError || !strings.Contains(resultText(res), "7") {
		t.Fatalf("%+v", res)
	}
}

func TestModelConcurrencyAndUsage(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var active, peak atomic.Int32
	tool := &Tool{Models: func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, *provider.Usage, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return json.RawMessage(`{"stopReason":"stop"}`), &provider.Usage{InputTokens: 2, CostUSD: 0.01}, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}}
	ag := core.NewAgent(nil, "test", "", core.NewRegistry(tool))
	done := make(chan core.ToolResult, 1)
	go func() {
		done <- runScript(t, ag, `return await Promise.all(Array.from({length:8},()=>models.classify({},{})))`, 5000)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 4 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("model calls did not start")
		}
	}
	close(release)
	select {
	case res := <-done:
		if res.IsError || res.Usage == nil || res.Usage.InputTokens != 16 || peak.Load() != 4 || active.Load() != 0 || ag.Cost().InputTokens != 16 {
			t.Fatalf("result=%+v usage=%+v peak=%d", res, res.Usage, peak.Load())
		}
	case <-ctx.Done():
		t.Fatal("model calls did not complete")
	}
}
