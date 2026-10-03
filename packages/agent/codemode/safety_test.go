package codemode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestInvalidInput(t *testing.T) {
	for _, input := range []string{`{`, `{}`, `{"code":" "}`, `{"code":1}`, `{"code":"return 1","timeout_ms":0}`, `{"code":"return 1","timeout_ms":2147483648}`, `{"code":"return 1","timeout_ms":1.5}`} {
		res := newAgent().CallTool(context.Background(), "script", "codemode", json.RawMessage(input), nil)
		if !res.IsError {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestStandaloneScriptHasNoSessionCapabilities(t *testing.T) {
	res, err := (&Tool{}).Execute(context.Background(), json.RawMessage(`{"code":"store('temporary',1); return {count:ALL_TOOLS.length,value:load('temporary')}"}`), nil)
	if err != nil || res.IsError || len(res.State) != 0 || !strings.Contains(resultText(res), `"count":0`) || !strings.Contains(resultText(res), `"value":1`) {
		t.Fatalf("standalone result: %+v %v", res, err)
	}
	res, err = (&Tool{}).Execute(context.Background(), json.RawMessage(`{"code":"return load('temporary')"}`), nil)
	if err != nil || res.IsError || strings.Contains(resultText(res), "\n1") {
		t.Fatalf("standalone state leaked: %+v %v", res, err)
	}
}

func TestNestedToolPermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(path, []byte("private text"), 0600); err != nil {
		t.Fatal(err)
	}
	sandbox := tools.NewSandbox(root)
	sandbox.Lock()
	ag := newAgent(&tools.ReadTool{CWD: root, Sandbox: sandbox})
	encoded, _ := json.Marshal(map[string]string{"path": path})
	res := runScript(t, ag, "return await tools.read("+string(encoded)+")", 1000)
	if !res.IsError || !strings.Contains(resultText(res), "jailed") || strings.Contains(resultText(res), "private text") {
		t.Fatalf("%+v", res)
	}
	sandbox.Unlock()
	sandbox.SetPermissions(&tools.PermissionSet{})
	res = runScript(t, ag, "return await tools.read("+string(encoded)+")", 1000)
	if !res.IsError || !strings.Contains(resultText(res), "permission denied") {
		t.Fatalf("%+v", res)
	}
}

func TestImagesAndPartialFailure(t *testing.T) {
	ag := newAgent(testTool{name: "picture", outputSchema: json.RawMessage(`{"type":"object"}`), run: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{StructuredContent: json.RawMessage(`{"content":[{"type":"text","text":"caption"},{"type":"image","mimeType":"image/png","data":"iVBORw0KGgo="}]}`)}, nil
	}})
	res := runScript(t, ag, `const r = await tools.picture({}); for (const b of r.content) { if (b.type === "image") image(b); else text(b.text) } throw new Error("later")`, 1000)
	if !res.IsError || !strings.Contains(resultText(res), "caption") || len(res.Content) != 4 {
		t.Fatalf("%+v", res)
	}
	block, ok := res.Content[2].(provider.ImageBlock)
	if !ok || block.MimeType != "image/png" || string(block.Data) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("image: %+v", block)
	}
	for _, code := range []string{`image({type:"image",mime_type:"image/png",data:"invalid"})`, `image({type:"image",mime_type:"text/plain",data:""})`, `image("https://example.invalid/image.png")`} {
		res = runScript(t, ag, code, 1000)
		if !res.IsError {
			t.Fatalf("accepted %s", code)
		}
	}
}

func TestErrorConversionIsInterruptible(t *testing.T) {
	for _, code := range []string{
		`throw {toString() { while (true) {} }}`,
		`return {toJSON() { while (true) {} }}`,
		`text({toJSON() { while (true) {} }})`,
		`text({toJSON() { throw {toString() { while (true) {} }} }})`,
		`return {toJSON() { throw {toString() { while (true) {} }} }}`,
	} {
		res := runScript(t, newAgent(), code, 20)
		if !res.IsError || res.Status != "timed_out" {
			t.Fatalf("%+v", res)
		}
	}
}

func TestConcurrentCallsAreNotArtificiallyLimited(t *testing.T) {
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	var running, peak atomic.Int32
	ag := newAgent(testTool{name: "wait", run: func(ctx context.Context, _ json.RawMessage) (core.ToolResult, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return textResult("ok"), nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}})
	finished := make(chan core.ToolResult, 1)
	go func() {
		finished <- runScript(t, ag, `return await Promise.all(Array.from({length:16},()=>tools.wait({})))`, 3000)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 16 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("calls did not start")
		}
	}
	close(release)
	select {
	case res := <-finished:
		if res.IsError {
			t.Fatalf("%+v", res)
		}
	case <-ctx.Done():
		t.Fatal("script did not finish")
	}
	if peak.Load() != 16 || running.Load() != 0 {
		t.Fatalf("peak=%d running=%d", peak.Load(), running.Load())
	}
}

func TestAllSettledAndNoRollback(t *testing.T) {
	var writes atomic.Int32
	ag := newAgent(testTool{name: "write", run: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		writes.Add(1)
		return textResult("saved"), nil
	}}, testTool{name: "fail", run: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		r := textResult("failed")
		r.IsError = true
		return r, nil
	}})
	res := runScript(t, ag, `return (await Promise.allSettled([tools.write({}), tools.fail({})])).map(r=>r.status)`, 1000)
	if res.IsError || !strings.Contains(resultText(res), `["fulfilled","rejected"]`) {
		t.Fatalf("%+v", res)
	}
	res = runScript(t, ag, `await tools.write({}); throw new Error("later")`, 1000)
	if !res.IsError || writes.Load() != 2 {
		t.Fatalf("writes=%d result=%+v", writes.Load(), res)
	}
}

func TestNestedActivationsSurviveScriptFailure(t *testing.T) {
	ag := newAgent(testTool{name: "discover", run: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		result := textResult("discovered")
		result.ActivateTools = []string{"deferred"}
		return result, nil
	}})
	res := runScript(t, ag, `await tools.discover({}); throw new Error("later")`, 1000)
	if !res.IsError || len(res.ActivateTools) != 1 || res.ActivateTools[0] != "deferred" {
		t.Fatalf("activation lost: %+v", res)
	}
}

func TestLargeArgumentsAndImageLists(t *testing.T) {
	ag := newAgent(testTool{name: "echo", run: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return textResult("ok"), nil
	}})
	res := runScript(t, ag, `await tools.echo({value: "x".repeat(262144)})`, 2000)
	if res.IsError {
		t.Fatalf("large argument failed: %+v", res)
	}
	res = runScript(t, ag, `for (let i = 0; i < 17; i++) image({type: "image", mimeType: "image/png", data: "iVBORw0KGgo="})`, 2000)
	if res.IsError || len(res.Content) != 18 {
		t.Fatalf("image list failed: %+v", res)
	}
}
