package extensions

import (
	"context"
	"encoding/json"
	"os"

	"github.com/patriceckhart/zot/internal/extproto"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// stubHooks records every callback so the test can assert on them.
type stubHooks struct {
	mu       sync.Mutex
	notifies []string
	displays []string
}

func (s *stubHooks) Notify(name, level, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifies = append(s.notifies, name+":"+level+":"+message)
}
func (s *stubHooks) Submit(string) {}
func (s *stubHooks) Insert(string) {}
func (s *stubHooks) Display(name, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displays = append(s.displays, name+":"+text)
}
func (s *stubHooks) OpenPanel(string, extproto.PanelSpec)                 {}
func (s *stubHooks) UpdatePanel(string, string, string, []string, string) {}
func (s *stubHooks) ClosePanel(string, string)                            {}

// writeMockExtension creates a minimal extension on disk that uses a
// shell script (or batch file on windows) to drive the protocol. The
// script reads commands from stdin and emits hard-coded responses,
// exercising the manager's spawn/handshake/dispatch path without
// needing the SDK.
func writeMockExtension(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	dir := filepath.Join(root, "mock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Shell script: emit hello, read frames, respond. Reads until
	// stdin closes; tail's -F keeps the pipe alive long enough for
	// the manager to send command_invoked.
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"mock","version":"0.0.1","capabilities":["commands"]}'
printf '%s\n' '{"type":"register_command","name":"ping","description":"ping/pong"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"command_invoked"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
      printf '%s\n' "{\"type\":\"command_response\",\"id\":\"$id\",\"action\":\"display\",\"display\":\"pong\"}"
      ;;
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"name":    "mock",
		"version": "0.0.1",
		"exec":    "./run.sh",
	}
	mfb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), mfb, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSpawnAndInvoke(t *testing.T) {
	tmp := t.TempDir()
	extRoot := filepath.Join(tmp, "extensions")
	if err := os.MkdirAll(extRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMockExtension(t, extRoot)

	hooks := &stubHooks{}
	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", hooks)

	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	// Give the extension a beat to send register_command frames after
	// the hello handshake.
	time.Sleep(150 * time.Millisecond)

	cmds := mgr.Commands()
	if len(cmds) != 1 || cmds[0].Name != "ping" {
		t.Fatalf("expected one command 'ping', got %#v", cmds)
	}
	if !mgr.HasCommand("ping") {
		t.Fatal("HasCommand(\"ping\") = false")
	}

	resp, err := mgr.Invoke(context.Background(), "ping", "", 2*time.Second)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if resp.Action != "display" {
		t.Errorf("expected action=display, got %q", resp.Action)
	}
	if resp.Display != "pong" {
		t.Errorf("expected display=pong, got %q", resp.Display)
	}
}
