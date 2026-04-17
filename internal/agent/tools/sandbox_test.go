package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxLockedBlocksOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "a.txt")
	os.WriteFile(outsideFile, []byte("secret"), 0o644)

	sb := NewSandbox(root)
	sb.Lock()

	if err := sb.CheckPath(outsideFile); err == nil {
		t.Fatal("expected outside path to be blocked")
	}
	inside := filepath.Join(root, "ok.txt")
	if err := sb.CheckPath(inside); err != nil {
		t.Fatalf("inside path blocked unexpectedly: %v", err)
	}
}

func TestSandboxUnlockedAllows(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sb := NewSandbox(root)
	if err := sb.CheckPath(filepath.Join(outside, "a.txt")); err != nil {
		t.Fatalf("unlocked should allow: %v", err)
	}
}

func TestSandboxCommandBanned(t *testing.T) {
	sb := NewSandbox(t.TempDir())
	sb.Lock()
	cases := []string{
		"sudo apt-get install foo",
		"rm -rf /",
		"cd /etc && ls",
		"cd .. && rm foo",
	}
	for _, c := range cases {
		if err := sb.CheckCommand(c); err == nil {
			t.Fatalf("expected %q to be banned", c)
		}
	}
	// Allowed:
	for _, c := range []string{"ls", "go test ./...", "cd subdir && ls"} {
		if err := sb.CheckCommand(c); err != nil {
			t.Fatalf("expected %q to be allowed: %v", c, err)
		}
	}
}

func TestReadToolRejectsOutsideWhenLocked(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "a.txt")
	os.WriteFile(outsideFile, []byte("x"), 0o644)

	sb := NewSandbox(root)
	sb.Lock()
	tool := &ReadTool{CWD: root, Sandbox: sb}

	_, err := tool.Execute(context.Background(),
		mustJSONRaw(t, map[string]any{"path": outsideFile}), nil)
	if err == nil {
		t.Fatal("expected sandbox error")
	}
}

func mustJSONRaw(t *testing.T, v any) []byte {
	t.Helper()
	return mustJSON(t, v)
}
