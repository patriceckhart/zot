package extensions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeMockToolExtension(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock uses /bin/sh; skip on windows")
	}
	dir := filepath.Join(root, "tool-mock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Shell-script extension: hello, register one tool, ready, then
	// loop on stdin echoing back tool_call args as tool_result text.
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"tool-mock","version":"0.1","capabilities":["tools"]}'
printf '%s\n' '{"type":"register_tool","name":"echo","description":"echo back the args","schema":{"type":"object","properties":{"msg":{"type":"string"}}}}'
printf '%s\n' '{"type":"ready"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"tool_call"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
      printf '%s\n' "{\"type\":\"tool_result\",\"id\":\"$id\",\"content\":[{\"type\":\"text\",\"text\":\"echoed\"}]}"
      ;;
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mfb, _ := json.Marshal(map[string]any{"name": "tool-mock", "exec": "./run.sh"})
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), mfb, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManagerToolRegistrationAndInvoke(t *testing.T) {
	tmp := t.TempDir()
	writeMockToolExtension(t, filepath.Join(tmp, "extensions"))

	mgr := New(tmp, "", "0.0.0", "anthropic", "opus", &stubHooks{})
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errs: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	// Wait for ready (the script sends it right after register_tool).
	mgr.WaitForReady(time.Second)

	tools := mgr.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("expected one tool 'echo', got %#v", tools)
	}
	if !mgr.HasTool("echo") {
		t.Fatal("HasTool(\"echo\") = false")
	}

	resp, err := mgr.InvokeTool(context.Background(), "echo", json.RawMessage(`{"msg":"hi"}`), 2*time.Second)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if resp.IsError {
		t.Errorf("expected success, got is_error=true")
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "echoed" {
		t.Errorf("unexpected content: %#v", resp.Content)
	}
}
