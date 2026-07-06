package extensions

import (
	"context"
	"encoding/json"
	"os"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubHooks records every callback so the test can assert on them.
type stubHooks struct {
	mu          sync.Mutex
	notifies    []string
	displays    []string
	submits     []string
	submitSlash []string
	clearNotes  []string
	panels      []extproto.PanelSpec
	panelExts   []string
}

func (s *stubHooks) Notify(name, level, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifies = append(s.notifies, name+":"+level+":"+message)
}
func (s *stubHooks) Submit(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submits = append(s.submits, text)
}
func (s *stubHooks) SubmitSlash(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitSlash = append(s.submitSlash, text)
}
func (s *stubHooks) Insert(string) {}
func (s *stubHooks) Display(name, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displays = append(s.displays, name+":"+text)
}
func (s *stubHooks) ClearNotes(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearNotes = append(s.clearNotes, name)
}
func (s *stubHooks) OpenPanel(extName string, spec extproto.PanelSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panelExts = append(s.panelExts, extName)
	s.panels = append(s.panels, spec)
}
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

func TestDiscoverLoadsThemeOnlyExtension(t *testing.T) {
	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "theme-only")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"theme-only","version":"1.0.0","description":"theme only"}`
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	theme := `{"name":"Theme Only","description":"theme from extension","colors":{"dark":{"accent":204}}}`
	if err := os.WriteFile(filepath.Join(extDir, "theme.json"), []byte(theme), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", nil)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(10 * time.Millisecond)

	opts := mgr.ThemeOptions()
	if len(opts) != 1 {
		t.Fatalf("theme options = %d, want 1", len(opts))
	}
	if opts[0].Label != "Theme Only" || opts[0].Path != filepath.Join(extDir, "theme.json") {
		t.Fatalf("unexpected theme option: %#v", opts[0])
	}
	if !strings.Contains(opts[0].Description, "from extension theme-only") {
		t.Fatalf("description missing extension source: %q", opts[0].Description)
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
	found := false
	for _, c := range cmds {
		if c.Name == "ping" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected command 'ping', got %#v", cmds)
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

func TestDiagnosticsReportMalformedFramesAndConflicts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extRoot := filepath.Join(tmp, "extensions")
	writeDiagExtension := func(name, script string) {
		t.Helper()
		dir := filepath.Join(extRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		mfb, _ := json.Marshal(map[string]any{"name": name, "exec": "./run.sh"})
		if err := os.WriteFile(filepath.Join(dir, "extension.json"), mfb, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeDiagExtension("a-first", `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"a-first","version":"0.1","capabilities":["tools"]}'
printf '%s\n' '{"type":"register_tool","name":"shared","description":"first","schema":{"type":"object"}}'
printf '%s\n' '{"type":"register_tool","name":"broken","description":"bad","schema":'
printf '%s\n' '{"type":"ready"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*) exit 0 ;;
  esac
done
`)
	writeDiagExtension("b-second", `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"b-second","version":"0.1","capabilities":["tools"]}'
printf '%s\n' '{"type":"register_tool","name":"shared","description":"second","schema":{"type":"object"}}'
printf '%s\n' '{"type":"ready"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*) exit 0 ;;
  esac
done
`)

	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", &stubHooks{})
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)
	mgr.WaitForReady(2 * time.Second)

	diags := mgr.Diagnostics()
	byName := map[string]ExtensionDiagnostic{}
	for _, d := range diags {
		byName[d.Name] = d
	}

	first := byName["a-first"]
	if len(first.Messages) == 0 || !strings.Contains(strings.Join(first.Messages, "\n"), "malformed json frame") {
		t.Fatalf("expected malformed-frame diagnostic, got %#v", first.Messages)
	}

	var shadowedTool bool
	var conflictMessage bool
	for _, d := range diags {
		if strings.Contains(strings.Join(d.Messages, "\n"), "conflicts with another extension") {
			conflictMessage = true
		}
		for _, tool := range d.Tools {
			if tool.Name == "shared" && !tool.Active {
				shadowedTool = true
			}
		}
	}
	if !shadowedTool {
		t.Fatalf("expected one shared tool registration to be inactive, got %#v", diags)
	}
	if !conflictMessage {
		t.Fatalf("expected conflict diagnostic, got %#v", diags)
	}
}

// TestSpontaneousSubmit verifies that an extension can submit a
// non-empty prompt outside of any command response.
func TestSpontaneousSubmit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "submit-mock")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"submit-mock","version":"0.1","capabilities":["submit"]}'
printf '%s\n' '{"type":"ready"}'
printf '%s\n' '{"type":"submit","text":"  explain this repository briefly  "}'
printf '%s\n' '{"type":"submit","text":"   "}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mfb, _ := json.Marshal(map[string]any{"name": "submit-mock", "exec": "./run.sh"})
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), mfb, 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := &stubHooks{}
	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", hooks)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hooks.mu.Lock()
		n := len(hooks.submits)
		hooks.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if len(hooks.submits) != 1 {
		t.Fatalf("Submit calls = %#v, want one non-empty call", hooks.submits)
	}
	if hooks.submits[0] != "explain this repository briefly" {
		t.Fatalf("Submit text = %q", hooks.submits[0])
	}
}

// TestSpontaneousOpenPanel verifies that an extension sending an
// open_panel frame outside of any command response causes the manager
// to call hooks.OpenPanel with the correct PanelSpec fields.
func TestSpontaneousOpenPanel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock extension uses /bin/sh; skip on windows")
	}

	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "extensions", "panel-mock")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Extension emits hello + ready, then immediately fires a
	// spontaneous open_panel, then waits for shutdown.
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"panel-mock","version":"0.1","capabilities":["panels"]}'
printf '%s\n' '{"type":"ready"}'
printf '%s\n' '{"type":"open_panel","panel":{"id":"test-panel","title":"Hello Panel","lines":["line one","line two"],"footer":"esc close"}}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*)
      printf '%s\n' '{"type":"shutdown_ack"}'
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mfb, _ := json.Marshal(map[string]any{"name": "panel-mock", "exec": "./run.sh"})
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), mfb, 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := &stubHooks{}
	mgr := New(tmp, "", "0.0.0-test", "anthropic", "claude-opus-4-7", hooks)
	if errs := mgr.Discover(context.Background()); len(errs) > 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)

	// Give the extension time to flush its open_panel frame.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hooks.mu.Lock()
		n := len(hooks.panels)
		hooks.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	hooks.mu.Lock()
	defer hooks.mu.Unlock()

	if len(hooks.panels) == 0 {
		t.Fatal("hooks.OpenPanel was never called")
	}
	spec := hooks.panels[0]
	if spec.ID != "test-panel" {
		t.Errorf("panel id: want %q, got %q", "test-panel", spec.ID)
	}
	if spec.Title != "Hello Panel" {
		t.Errorf("panel title: want %q, got %q", "Hello Panel", spec.Title)
	}
	if len(spec.Lines) != 2 || spec.Lines[0] != "line one" || spec.Lines[1] != "line two" {
		t.Errorf("panel lines: want [line one line two], got %v", spec.Lines)
	}
	if spec.Footer != "esc close" {
		t.Errorf("panel footer: want %q, got %q", "esc close", spec.Footer)
	}
	if hooks.panelExts[0] != "panel-mock" {
		t.Errorf("ext name: want %q, got %q", "panel-mock", hooks.panelExts[0])
	}
}
