package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/internal/provider"
)

// TestSessionExportImportRoundTrip writes a few messages to a live
// session, exports it, imports the export under a different cwd,
// and verifies OpenSession on the imported file yields the same
// message payloads.
func TestSessionExportImportRoundTrip(t *testing.T) {
	root := t.TempDir()
	originalCWD := "/path/to/project"
	sess, err := NewSession(root, originalCWD, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello from the exporter"}},
	})
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "hi — reply from the assistant"}},
	})
	_ = sess.Close()

	// Export to a directory — helper should build a name inside it.
	exportDir := t.TempDir()
	exportPath, err := ExportSession(sess.Path, exportDir)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if !strings.HasSuffix(exportPath, PortableExt) {
		t.Errorf("exported path should end in %s, got %q", PortableExt, exportPath)
	}
	if _, err := os.Stat(exportPath); err != nil {
		t.Fatalf("exported file doesn't exist: %v", err)
	}

	// Import into a different root + cwd.
	root2 := t.TempDir()
	cwd2 := "/some/other/project"
	importedPath, err := ImportSession(exportPath, root2, cwd2, "0.0.0-test")
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if filepath.Dir(importedPath) != SessionsDir(root2, cwd2) {
		t.Errorf("imported file should land in SessionsDir, got %q", importedPath)
	}

	// Reopen and verify message round-trip.
	imported, msgs, err := OpenSession(importedPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer imported.Close()
	if imported.Meta.CWD != cwd2 {
		t.Errorf("meta cwd: want %q, got %q", cwd2, imported.Meta.CWD)
	}
	if imported.Meta.ID == sess.ID {
		t.Errorf("imported session kept the original id %q; must be rotated", sess.ID)
	}
	if imported.Meta.Model != "claude-opus-4-7" {
		t.Errorf("model not preserved: %q", imported.Meta.Model)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	// Text should round-trip.
	if extractText(msgs[0]) != "hello from the exporter" {
		t.Errorf("msg 0 mismatch: %q", extractText(msgs[0]))
	}
	if extractText(msgs[1]) != "hi — reply from the assistant" {
		t.Errorf("msg 1 mismatch: %q", extractText(msgs[1]))
	}
}

// TestExportToFilePath writes to an explicit file path (no
// directory guessing) and checks the .zotsession extension is
// appended when missing.
func TestExportToFilePath(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root, "/cwd", "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "x"}},
	})
	_ = sess.Close()

	// No extension — should add .zotsession.
	dst := filepath.Join(t.TempDir(), "mysession")
	out, err := ExportSession(sess.Path, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, PortableExt) {
		t.Errorf("want .zotsession suffix on %q", out)
	}
}

// TestExportStripsCWDFromMeta verifies the exported meta no longer
// carries the source user's cwd (not useful to the recipient).
func TestExportSessionHandlesHugeJSONLRows(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root, "/cwd", "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "first prompt names export"}},
	})
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("x", 22*1024*1024)}},
	})
	_ = sess.Close()

	exportDir := t.TempDir()
	out, err := ExportSession(sess.Path, exportDir)
	if err != nil {
		t.Fatalf("ExportSession with huge row: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("exported file missing: %v", err)
	}
	opened, msgs, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatalf("OpenSession with huge row: %v", err)
	}
	_ = opened.Close()
	if len(msgs) != 2 {
		t.Fatalf("OpenSession messages=%d, want 2", len(msgs))
	}
	// Ensure the exported file is still readable JSONL and contains the
	// huge assistant message row, not just the meta/header.
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), strings.Repeat("x", 1024)) {
		t.Fatalf("export does not appear to contain huge assistant row")
	}
	for n, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("exported line %d is invalid json: %v", n+1, err)
		}
	}
}

func TestExportStripsCWDFromMeta(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root, "/original/cwd", "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "x"}},
	})
	_ = sess.Close()

	out, err := ExportSession(sess.Path, filepath.Join(t.TempDir(), "x"+PortableExt))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "/original/cwd") {
		t.Errorf("exported file leaks the source cwd: %s", string(b))
	}
}

// TestBranchSessionCopiesPrefix writes several messages to a
// session, branches at message index 2 (first user + first
// assistant), and verifies the new session has exactly those two
// messages with parent + fork_point meta set.
func TestBranchSessionCopiesPrefix(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "first"}},
	})
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "first reply"}},
	})
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "second"}},
	})
	_ = parent.Close()

	// Branch at the first user+assistant pair (upToMessageIdx=2).
	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 2)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, msgs, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	defer branch.Close()

	if len(msgs) != 2 {
		t.Errorf("want 2 copied messages, got %d", len(msgs))
	}
	if branch.Meta.Parent != parent.Meta.ID {
		t.Errorf("parent id: want %q, got %q", parent.Meta.ID, branch.Meta.Parent)
	}
	if branch.Meta.ForkPoint != 2 {
		t.Errorf("fork_point: want 2, got %d", branch.Meta.ForkPoint)
	}
	if branch.Meta.ID == parent.Meta.ID {
		t.Errorf("branch kept parent id; must rotate")
	}
}

// TestBuildSessionTree verifies parent/child edges are rebuilt
// from meta + sibling-scan.
func TestBuildSessionTree(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, _ := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "x"}},
	})
	_ = parent.Close()

	childA, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 1)
	if err != nil {
		t.Fatal(err)
	}
	childB, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = childA
	_ = childB

	tree := BuildSessionTree(root, cwd)
	if len(tree) != 1 {
		t.Fatalf("want 1 root, got %d", len(tree))
	}
	rootNode := tree[0]
	if rootNode.Meta.ID != parent.Meta.ID {
		t.Errorf("root should be parent %q, got %q", parent.Meta.ID, rootNode.Meta.ID)
	}
	if len(rootNode.Children) != 2 {
		t.Errorf("want 2 children, got %d", len(rootNode.Children))
	}
}
