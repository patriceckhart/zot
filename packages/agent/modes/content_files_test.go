package modes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestContentPickerAndQueuedSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes with spaces.txt")
	if err := os.WriteFile(path, []byte("original content"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := core.NewAgent(nil, "test", "", nil)
	iv := NewInteractive(InteractiveConfig{CWD: root, Agent: ag})
	iv.fileSuggest.SetCWD(root)
	iv.busy = true
	iv.ed.SetValue("!@notes")
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if got := iv.ed.Value(); got != "[content:notes with spaces.txt] " {
		t.Fatalf("picker selection = %q", got)
	}
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if !iv.ed.IsEmpty() {
		t.Fatalf("submission failed: %s", iv.statusErr)
	}
	if err := os.WriteFile(path, []byte("changed content"), 0o600); err != nil {
		t.Fatal(err)
	}
	queued := ag.PendingQueuedMessages()
	if len(queued) != 1 || !strings.Contains(queued[0], "original content") || strings.Contains(queued[0], "changed content") {
		t.Fatalf("queue did not snapshot content: %q", queued)
	}
	text, _, err := iv.prepareEditorPrompt(context.Background(), "read [content:notes with spaces.txt] and [file:missing.txt]")
	if err != nil || !strings.Contains(text, "changed content") || !strings.Contains(text, filepath.Join(root, "missing.txt")) {
		t.Fatalf("mixed content and reference failed: %q, %v", text, err)
	}
	text, _, err = iv.prepareEditorPrompt(context.Background(), "read [file:notes with spaces.txt]")
	if err != nil || strings.Contains(text, "changed content") || text != "read "+path {
		t.Fatalf("ordinary reference changed: %q, %v", text, err)
	}
}

func TestContentSelectionErrorsPreserveInput(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string][]byte{
		"binary": {0, 1},
		"large":  make([]byte, continuous.MaxAttachmentBytes+1),
	} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	iv := NewInteractive(InteractiveConfig{CWD: root, Agent: core.NewAgent(nil, "test", "", nil)})
	iv.busy = true
	for _, input := range []string{
		"[content:missing]", "[content:binary]", "[content:large]", "[content:.]",
		"/study [content:binary]", "!cat [content:binary]", "!@missing",
		strings.Repeat("[content:binary] ", continuous.MaxAttachments+1),
	} {
		t.Run(input[:min(len(input), 40)], func(t *testing.T) {
			iv.ed.SetValue(input)
			iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
			if iv.ed.Value() != input || !strings.Contains(iv.statusErr, "attachment:") {
				t.Fatalf("invalid input cleared or accepted: %q, %q", iv.ed.Value(), iv.statusErr)
			}
		})
	}
	if iv.agent.QueuedMessageCount() != 0 {
		t.Fatal("invalid input was queued")
	}
}

func TestContentSelectionHonorsJailAndCancellation(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	jail := tools.NewSandbox(root)
	jail.Lock()
	iv := NewInteractive(InteractiveConfig{CWD: root, Sandbox: jail})
	rel, err := filepath.Rel(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := iv.prepareEditorPrompt(context.Background(), "[content:"+rel+"]"); err == nil {
		t.Fatal("content escaped jail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := iv.prepareEditorPrompt(ctx, "[content:notes.txt]"); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}

func TestContentImagesAndAttachedSnapshot(t *testing.T) {
	root := t.TempDir()
	img := transferImage(t)
	if err := os.WriteFile(filepath.Join(root, "image.png"), img.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	iv := NewInteractive(InteractiveConfig{CWD: root, Agent: core.NewAgent(nil, "test", "", nil)})
	text, images, err := iv.prepareEditorPrompt(context.Background(), "[content:image.png]")
	if err != nil || len(images) != 1 || !strings.Contains(text, "Attached image.") {
		t.Fatalf("image not included: %q, %v, %v", text, images, err)
	}
	iv.busy = true
	iv.ed.SetValue("[content:image.png]")
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if !iv.ed.IsEmpty() || len(iv.queued) != 1 || len(iv.queued[0].Images) != 1 {
		t.Fatal("local busy image selection was not queued")
	}
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
	if len(iv.queued) != 0 || len(iv.clipboardImages) != 1 {
		t.Fatal("local queued image could not be restored for editing")
	}
	iv.cfg.PromptDriverWithImages = func(context.Context, *core.Agent, string, []provider.ImageBlock, func(core.AgentEvent)) error {
		t.Fatal("busy attached submission must queue")
		return nil
	}
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if len(iv.queued) != 1 || len(iv.queued[0].Images) != 1 {
		t.Fatal("attached image not queued")
	}
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	iv.ed.SetValue("[content:notes.txt]")
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if len(iv.queued) != 2 || !strings.Contains(iv.queued[1].Text, "snapshot") {
		t.Fatal("attached text content not captured before queueing")
	}
}

func TestContentPickerCancelAndDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	iv := NewInteractive(InteractiveConfig{CWD: root})
	iv.fileSuggest.SetCWD(root)
	iv.ed.SetValue("review !@folder")
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if iv.ed.Value() != "review !@folder" || !strings.Contains(iv.statusErr, "requires a file") {
		t.Fatal("directory content selection was accepted")
	}
	iv.fileSuggest.lastMatches = iv.fileSuggest.matches(iv.ed.Value())
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyRight})
	if iv.ed.Value() != "review !@" || iv.fileSuggest.browseRel != "folder" {
		t.Fatal("directory browsing lost content mode")
	}
	// Browse back to a populated directory so Escape acts on the picker.
	iv.fileSuggest.Left()
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	if iv.ed.Value() != "review " {
		t.Fatalf("cancel left trigger: %q", iv.ed.Value())
	}
	if _, ok := ShellEscapeCommand("!@notes"); ok {
		t.Fatal("content trigger dispatched to shell")
	}
	if cmd, ok := ShellEscapeCommand("!echo hello"); !ok || cmd != "echo hello" {
		t.Fatal("ordinary shell escape changed")
	}
}
