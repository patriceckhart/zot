package codemode

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

func TestOutputLimitPreservesPartialOutputRollsBackAndCancelsCalls(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	ag := newAgent(testTool{name: "wait", run: func(ctx context.Context, _ json.RawMessage) (core.ToolResult, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return core.ToolResult{}, ctx.Err()
	}}, testTool{name: "started", run: func(ctx context.Context, _ json.RawMessage) (core.ToolResult, error) {
		select {
		case <-started:
			return textResult("started"), nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}})
	res := runScript(t, ag, `store("value",1)`, 2000)
	if res.IsError {
		t.Fatalf("initial store failed: %+v", res)
	}
	code := `// @options: {"max_output_tokens":1024}
	tools.wait({})
	await tools.started({})
	store("value",2)
	text("partial output")
	const chunk = "x".repeat(65536)
	try { for (let i = 0; i < 256; i++) text(chunk) } catch (e) { exit() }
	`
	args, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res = ag.CallTool(ctx, "overflow", "codemode", args, nil)
	path := ""
	if details, ok := res.Details.(map[string]any); ok {
		path, _ = details["fullOutputPath"].(string)
	}
	if path != "" {
		t.Cleanup(func() { _ = os.Remove(path) })
	}
	if !res.IsError || res.Status != "" || len(res.State) != 0 || !strings.Contains(resultText(res), "partial output") || !strings.Contains(resultText(res), "script output exceeded the limit") {
		t.Fatalf("output limit did not fail with partial output: %+v", res)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("script returned before cancelling its outstanding tool")
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "partial output\n") || !strings.Contains(string(data), "Script error:\nscript output exceeded the limit") || len(data) > 16777216 {
		t.Fatalf("unexpected bounded spill: bytes=%d error=%v", len(data), err)
	}
	res = runScript(t, ag, `return load("value")`, 2000)
	if res.IsError || !strings.HasSuffix(resultText(res), "1") {
		t.Fatalf("failed script committed state or damaged subsequent execution: %+v", res)
	}
}
