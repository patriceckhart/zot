package extensions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInterceptAllThreeEvents exercises tool_call / turn_start /
// assistant_message interception end-to-end via a bash extension that
// blocks `rm -rf`, rewrites any other bash command to `echo GUARDED`,
// suppresses nothing at turn_start, and rewrites SECRET → [redacted]
// in assistant messages.
//
// Validates: block, modified_args, pass-through, and replace_text.
func TestInterceptAllThreeEvents(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}

	extDir := t.TempDir()
	script := `#!/bin/bash
emit() { printf '%s\n' "$1"; }
emit '{"type":"hello","name":"itest","version":"0.1.0","capabilities":["events"]}'
emit '{"type":"subscribe","events":[],"intercept":["tool_call","turn_start","assistant_message"]}'
emit '{"type":"ready"}'
while IFS= read -r line; do
  t=$(printf '%s' "$line" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("type",""))')
  id=$(printf '%s' "$line" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("id",""))')
  ev=$(printf '%s' "$line" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("event",""))')
  if [[ "$t" == "shutdown" ]]; then emit '{"type":"shutdown_ack"}'; exit 0; fi
  if [[ "$t" != "event_intercept" ]]; then continue; fi
  case "$ev" in
    tool_call)
      name=$(printf '%s' "$line" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("tool_name",""))')
      args=$(printf '%s' "$line" | python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin).get("tool_args",{})))')
      cmd=$(printf '%s' "$args" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("command",""))')
      if [[ "$name" == "bash" && "$cmd" == *"rm -rf"* ]]; then
        emit "{\"type\":\"event_intercept_response\",\"id\":\"$id\",\"block\":true,\"reason\":\"refused: rm -rf\"}"
      elif [[ "$name" == "bash" && -n "$cmd" ]]; then
        new=$(python3 -c "import json,sys;print(json.dumps({'command':'echo GUARDED: '+sys.argv[1]}))" "$cmd")
        emit "{\"type\":\"event_intercept_response\",\"id\":\"$id\",\"modified_args\":$new}"
      else
        emit "{\"type\":\"event_intercept_response\",\"id\":\"$id\"}"
      fi ;;
    turn_start)
      emit "{\"type\":\"event_intercept_response\",\"id\":\"$id\"}" ;;
    assistant_message)
      text=$(printf '%s' "$line" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("text",""))')
      if [[ "$text" == *"SECRET"* ]]; then
        new=$(python3 -c "import sys;print(sys.argv[1].replace('SECRET','[redacted]'))" "$text")
        rt=$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$new")
        emit "{\"type\":\"event_intercept_response\",\"id\":\"$id\",\"replace_text\":$rt}"
      else
        emit "{\"type\":\"event_intercept_response\",\"id\":\"$id\"}"
      fi ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "ext.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"itest","version":"0.1.0","exec":"./ext.sh","enabled":true}`
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// ZotHome is unused here; we load the extension explicitly.
	m := New(t.TempDir(), "", "0.0.0-test", "anthropic", "claude-test", nil)
	t.Cleanup(func() { m.Stop(2 * time.Second) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errs := m.LoadExplicit(ctx, []string{extDir}); len(errs) > 0 {
		t.Fatalf("LoadExplicit: %v", errs)
	}
	m.WaitForReady(3 * time.Second)

	// tool_call: rm -rf is blocked
	res := m.InterceptToolCall(ctx, "T1", "bash", json.RawMessage(`{"command":"rm -rf /tmp/foo"}`))
	if !res.Block || !strings.Contains(res.Reason, "refused") {
		t.Errorf("rm -rf: want block+reason, got %+v", res)
	}

	// tool_call: non-dangerous bash gets args rewritten
	res = m.InterceptToolCall(ctx, "T2", "bash", json.RawMessage(`{"command":"ls -la"}`))
	if res.Block {
		t.Errorf("ls -la: want allow, got block %q", res.Reason)
	}
	if len(res.ModifiedArgs) == 0 {
		t.Errorf("ls -la: want modified_args, got nothing")
	} else {
		var obj map[string]string
		if err := json.Unmarshal(res.ModifiedArgs, &obj); err != nil {
			t.Errorf("modified_args: %v", err)
		} else if !strings.HasPrefix(obj["command"], "echo GUARDED") {
			t.Errorf("modified_args command=%q, want echo GUARDED prefix", obj["command"])
		}
	}

	// tool_call: non-bash tool untouched
	res = m.InterceptToolCall(ctx, "T3", "read", json.RawMessage(`{"path":"/etc/hosts"}`))
	if res.Block {
		t.Errorf("read: want allow, got block %q", res.Reason)
	}
	if len(res.ModifiedArgs) != 0 {
		t.Errorf("read: want no modified_args, got %s", string(res.ModifiedArgs))
	}

	// turn_start: always allowed
	if r := m.InterceptTurnStart(ctx, 1); r.Block {
		t.Errorf("turn_start: want allow, got block %q", r.Reason)
	}

	// assistant_message: SECRET gets redacted
	r := m.InterceptAssistantMessage(ctx, "your password is SECRET123")
	if r.Block {
		t.Errorf("msg: want allow, got block %q", r.Reason)
	}
	if !strings.Contains(r.ReplaceText, "[redacted]") {
		t.Errorf("msg: want [redacted] in replacement, got %q", r.ReplaceText)
	}

	// assistant_message: clean text unchanged
	r = m.InterceptAssistantMessage(ctx, "hello world")
	if r.Block {
		t.Errorf("clean: want allow")
	}
	if r.ReplaceText != "" {
		t.Errorf("clean: want no replace, got %q", r.ReplaceText)
	}
}

// TestReloadRespawnsExtensions verifies Reload tears down and brings
// back the same extension, and the onReload callback fires.
func TestReloadRespawnsExtensions(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}

	extDir := t.TempDir()
	script := `#!/bin/bash
emit() { printf '%s\n' "$1"; }
emit '{"type":"hello","name":"rtest","version":"0.1.0","capabilities":["commands"]}'
emit '{"type":"register_command","name":"rtest","description":"test"}'
emit '{"type":"ready"}'
while IFS= read -r line; do
  t=$(printf '%s' "$line" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("type",""))')
  if [[ "$t" == "shutdown" ]]; then emit '{"type":"shutdown_ack"}'; exit 0; fi
done
`
	if err := os.WriteFile(filepath.Join(extDir, "ext.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"rtest","version":"0.1.0","exec":"./ext.sh","enabled":true}`
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(t.TempDir(), "", "0.0.0-test", "anthropic", "claude-test", nil)
	t.Cleanup(func() { m.Stop(2 * time.Second) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errs := m.LoadExplicit(ctx, []string{extDir}); len(errs) > 0 {
		t.Fatalf("LoadExplicit: %v", errs)
	}
	m.WaitForReady(3 * time.Second)
	before := len(m.All())
	if before == 0 {
		t.Fatal("extension didn't load")
	}

	reloadFired := false
	m.SetOnReload(func() { reloadFired = true })

	stats := m.Reload(ctx, 2*time.Second)
	if stats.Stopped != before {
		t.Errorf("Stopped: want %d, got %d", before, stats.Stopped)
	}
	if stats.Loaded != before {
		t.Errorf("Loaded: want %d, got %d", before, stats.Loaded)
	}
	if !reloadFired {
		t.Error("onReload callback didn't fire")
	}

	// Registered command should still be there after reload.
	if !m.HasCommand("rtest") {
		t.Error("rtest command not re-registered after reload")
	}
}
