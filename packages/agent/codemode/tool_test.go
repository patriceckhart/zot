package codemode

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type testTool struct {
	name         string
	run          func(context.Context, json.RawMessage) (core.ToolResult, error)
	outputSchema json.RawMessage
}

func (t testTool) Name() string                  { return t.name }
func (testTool) Description() string             { return "Test tool" }
func (testTool) Schema() json.RawMessage         { return json.RawMessage(`{"type":"object"}`) }
func (t testTool) OutputSchema() json.RawMessage { return t.outputSchema }
func (t testTool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	return t.run(ctx, args)
}
func textResult(text string) core.ToolResult {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}}
}
func runScript(t *testing.T, ag *core.Agent, code string, timeout int) core.ToolResult {
	t.Helper()
	args, err := json.Marshal(map[string]any{"code": code, "timeout_ms": timeout})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ag.CallTool(ctx, "script", "codemode", args, nil)
}
func resultText(res core.ToolResult) string {
	var text strings.Builder
	for _, block := range res.Content {
		if b, ok := block.(provider.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}
func newAgent(tools ...core.Tool) *core.Agent {
	return core.NewAgent(nil, "test", "", core.NewRegistry(append(tools, &Tool{})...))
}

func TestScriptOutput(t *testing.T) {
	ag := newAgent()
	res := runScript(t, ag, `text("hello"); console.log({n: 2}); return [1, 2]`, 1000)
	if res.IsError || len(res.Content) != 4 || res.Content[1].(provider.TextBlock).Text != "hello" || res.Content[2].(provider.TextBlock).Text != "{\"n\":2}" || res.Content[3].(provider.TextBlock).Text != "[1,2]" {
		t.Fatalf("%+v", res)
	}
	if len(ag.Messages()) != 0 {
		t.Fatal("script added messages to transcript")
	}
}

func TestParallelToolsAndFiltering(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	tool := testTool{name: "lookup", run: func(ctx context.Context, args json.RawMessage) (core.ToolResult, error) {
		started <- struct{}{}
		select {
		case <-release:
			return textResult(`{"name":"result","large":"discard"}`), nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}}
	ag := newAgent(tool)
	finished := make(chan core.ToolResult, 1)
	go func() {
		finished <- runScript(t, ag, `const values = await Promise.all([tools.lookup({}), tools.lookup({})]); return values.map(v => JSON.parse(v).name)`, 3000)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("calls did not overlap")
		}
	}
	close(release)
	select {
	case res := <-finished:
		if res.IsError || !strings.Contains(resultText(res), `["result","result"]`) || strings.Contains(resultText(res), "discard") {
			t.Fatalf("%+v", res)
		}
		if len(res.NestedCalls) != 2 {
			t.Fatalf("parallel calls lost display records: %+v", res.NestedCalls)
		}
		for _, call := range res.NestedCalls {
			if call.Name != "lookup" || call.Status != "completed" || !strings.Contains(call.Result, "discard") {
				t.Fatalf("nested display did not retain intermediate result: %+v", call)
			}
		}
	case <-ctx.Done():
		t.Fatal("script did not finish")
	}
}

func TestNestedGuardsAndEvents(t *testing.T) {
	var executed atomic.Int32
	ag := newAgent(testTool{name: "write", run: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		executed.Add(1)
		return textResult("written"), nil
	}})
	ag.BeforeToolExecuteContext = func(_ context.Context, call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return call.Name != "write", "permission denied", nil
	}
	var events []core.AgentEvent
	res := ag.CallTool(context.Background(), "script", "codemode", json.RawMessage(`{"code":"text('before'); await tools.write({})"}`), func(ev core.AgentEvent) { events = append(events, ev) })
	if !res.IsError || !strings.Contains(resultText(res), "before") || !strings.Contains(resultText(res), "permission denied") || executed.Load() != 0 {
		t.Fatalf("%+v", res)
	}
	var nestedCall, nestedResult bool
	for _, ev := range events {
		switch e := ev.(type) {
		case core.EvToolCall:
			nestedCall = nestedCall || e.ID == "script/1"
		case core.EvToolResult:
			nestedResult = nestedResult || (e.ID == "script/1" && e.Status == "blocked" && !e.Executed)
		}
	}
	if !nestedCall || !nestedResult {
		t.Fatalf("missing nested lifecycle: %+v", events)
	}
}

func TestScriptFailures(t *testing.T) {
	for _, tc := range []struct{ name, code, want string }{
		{"syntax", "const =", "Script error"},
		{"throw", `text("partial"); throw new Error("broken")`, "broken"},
		{"deadlock", `await new Promise(() => {})`, "cannot settle"},
		{"arguments", `await tools.echo([])`, "JSON object"},
		{"circular", `const x = {}; x.self = x; text(x)`, "circular"},
		{"unknown", `await tools.missing({})`, "Script error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := newAgent(testTool{name: "echo", run: func(context.Context, json.RawMessage) (core.ToolResult, error) { return textResult("ok"), nil }})
			res := runScript(t, ag, tc.code, 1000)
			if !res.IsError || !strings.Contains(resultText(res), tc.want) {
				t.Fatalf("%+v", res)
			}
		})
	}
}

func TestScriptTimeout(t *testing.T) {
	for _, code := range []string{`while (true) {}`, `while (true) await null`} {
		res := runScript(t, newAgent(), code, 20)
		if !res.IsError || res.Status != "timed_out" {
			t.Fatalf("%+v", res)
		}
	}
}

func TestCancellationAndUnawaitedCalls(t *testing.T) {
	for _, await := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "unawaited"}[await], func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			ag := newAgent(testTool{name: "wait", run: func(ctx context.Context, _ json.RawMessage) (core.ToolResult, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return core.ToolResult{}, ctx.Err()
			}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			code := `await tools.wait({})`
			if !await {
				// Wait for the first call to start before completing the script.
				ag.Tools["started"] = testTool{name: "started", run: func(ctx context.Context, _ json.RawMessage) (core.ToolResult, error) {
					select {
					case <-started:
						return textResult("started"), nil
					case <-ctx.Done():
						return core.ToolResult{}, ctx.Err()
					}
				}}
				code = `tools.wait({}); await tools.started({}); return "done"`
			}
			args, _ := json.Marshal(map[string]string{"code": code})
			finished := make(chan core.ToolResult, 1)
			go func() { finished <- ag.CallTool(ctx, "script", "codemode", args, nil) }()
			deadline, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			select {
			case <-started:
			case <-deadline.Done():
				t.Fatal("tool did not start")
			}
			if await {
				cancel()
			}
			select {
			case res := <-finished:
				if await && (!res.IsError || res.Status != "cancelled") {
					t.Fatalf("%+v", res)
				}
				if !await && res.IsError {
					t.Fatalf("%+v", res)
				}
			case <-deadline.Done():
				t.Fatal("script did not finish")
			}
			select {
			case <-stopped:
			case <-deadline.Done():
				t.Fatal("tool was not cancelled")
			}
		})
	}
}

func TestNoHostAPIsAndFreshVM(t *testing.T) {
	ag := newAgent()
	res := runScript(t, ag, `globalThis.saved = 42; return [typeof process, typeof require, typeof fetch, typeof setTimeout]`, 1000)
	if res.IsError || !strings.Contains(resultText(res), `["undefined","undefined","undefined","undefined"]`) {
		t.Fatalf("%+v", res)
	}
	res = runScript(t, ag, `return typeof saved`, 1000)
	if res.IsError || !strings.Contains(resultText(res), "undefined") {
		t.Fatalf("%+v", res)
	}
}

func TestOutputBudgetAndRepeatedCalls(t *testing.T) {
	res := runScript(t, newAgent(), "// @options: {\"max_output_tokens\": 16}\ntext(\"ä\".repeat(100000))", 2000)
	if res.IsError || !strings.Contains(resultText(res), "truncated output") || !strings.Contains(resultText(res), "Full output:") {
		t.Fatalf("%+v", res)
	}
	path := res.Details.(map[string]any)["fullOutputPath"].(string)
	t.Cleanup(func() { _ = os.Remove(path) })
	data, err := os.ReadFile(path)
	if err != nil || string(data) != strings.Repeat("ä", 100000) {
		t.Fatalf("full output lost: size=%d error=%v", len(data), err)
	}
	ag := newAgent(testTool{name: "echo", run: func(context.Context, json.RawMessage) (core.ToolResult, error) { return textResult("ok"), nil }})
	res = runScript(t, ag, `for (let i=0;i<129;i++) await tools.echo({})`, 5000)
	if res.IsError {
		t.Fatalf("%+v", res)
	}
}

func TestCatalogAndGuardRewrite(t *testing.T) {
	ag := newAgent(testTool{name: "custom-tool", run: func(_ context.Context, args json.RawMessage) (core.ToolResult, error) {
		return textResult(string(args)), nil
	}})
	ag.BeforeToolExecuteContext = func(_ context.Context, call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		if call.Name == "custom-tool" {
			return true, "", json.RawMessage(`{"value":"rewritten"}`)
		}
		return true, "", nil
	}
	res := runScript(t, ag, `text(ALL_TOOLS.map(t=>t.name)); return await tools["custom-tool"]({value:"original"})`, 1000)
	if res.IsError || !strings.Contains(resultText(res), "rewritten") || !strings.Contains(resultText(res), `["custom_tool"]`) {
		t.Fatalf("%+v", res)
	}
}
