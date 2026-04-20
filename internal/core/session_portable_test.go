package core

import (
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
