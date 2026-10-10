package modes

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestSwarmContentPickerSnapshotsOnSubmission(t *testing.T) {
	for _, followUp := range []bool{false, true} {
		name := "spawn"
		if followUp {
			name = "follow-up"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "notes.txt")
			if err := os.WriteFile(path, []byte("before selection"), 0o600); err != nil {
				t.Fatal(err)
			}
			var received string
			d := newSwarmDialog()
			d.cwd = root
			d.spawn = func(task, _, _ string) error {
				received = task
				return nil
			}
			d.send = func(id, text string) error {
				if id != "target" {
					t.Fatalf("sent to %q", id)
				}
				received = text
				return nil
			}
			d.openSpawnEditor(tui.Theme{})
			ed := d.newTaskEd
			handle := d.handleSpawnKey
			if followUp {
				d.openPromptEditor(tui.Theme{}, "target")
				ed = d.promptEd
				handle = d.handlePromptKey
			}
			ed.SetValue("review !@notes")
			handle(tui.Key{Kind: tui.KeyEnter})
			if ed.Value() != "review [content:notes.txt] " || received != "" {
				t.Fatalf("content picker did not select: %q", ed.Value())
			}
			if err := os.WriteFile(path, []byte("submission snapshot"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, errMsg := handle(tui.Key{Kind: tui.KeyEnter})
			if errMsg != "" || !strings.Contains(received, "submission snapshot") || strings.Contains(received, "before selection") {
				t.Fatalf("content not sent: %q, %s", received, errMsg)
			}
		})
	}
}

func TestSwarmContentErrorsKeepEditor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "image.png"), []byte{0, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	jail := tools.NewSandbox(root)
	jail.Lock()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, followUp := range []bool{false, true} {
		d := newSwarmDialog()
		d.cwd = root
		d.sandbox = jail
		d.spawn = func(string, string, string) error {
			t.Fatal("invalid content spawned")
			return nil
		}
		d.send = func(string, string) error {
			t.Fatal("invalid content sent")
			return nil
		}
		d.openSpawnEditor(tui.Theme{})
		ed := d.newTaskEd
		handle := d.handleSpawnKey
		if followUp {
			d.openPromptEditor(tui.Theme{}, "target")
			ed = d.promptEd
			handle = d.handlePromptKey
		}
		for _, input := range []string{"[content:missing.txt]", "[content:image.png]", "[content:" + rel + "]"} {
			ed.SetValue(input)
			_, _, errMsg := handle(tui.Key{Kind: tui.KeyEnter})
			if !strings.HasPrefix(errMsg, "attachment:") || ed.Value() != input {
				t.Fatalf("failed content not preserved: %q, %s", ed.Value(), errMsg)
			}
		}
	}
}

func TestAttachedSwarmOrdinaryPickerTransfersText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("client-local context"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newSwarmDialog()
	d.cwd = root
	d.transferFiles = true
	d.openSpawnEditor(tui.Theme{})
	text, _, err := d.prepareFilePrompt(d.newTaskEd, "[file:notes.txt]")
	if err != nil || !strings.Contains(text, "client-local context") {
		t.Fatalf("remote Swarm received only a local path: %q, %v", text, err)
	}
}

func TestBtwContentPickerSendsTextAndImages(t *testing.T) {
	for _, image := range []bool{false, true} {
		name := "notes.txt"
		data := []byte("side-chat file context")
		if image {
			name = "image.png"
			data = transferImage(t).Data
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
			client := &preparationCaptureClient{requests: make(chan provider.Request, 1)}
			main := core.NewAgent(client, "test", "", nil)
			d := newBtwDialog()
			d.Open(tui.Theme{}, main, "", "test", root, "", false, false, true, nil)
			t.Cleanup(d.Close)
			d.editor.SetValue("review !@" + name)
			d.HandleKey(tui.Key{Kind: tui.KeyEnter}, func() {})
			if d.editor.Value() != "review [content:"+name+"] " {
				t.Fatal("side-chat content selection failed")
			}
			d.HandleKey(tui.Key{Kind: tui.KeyEnter}, func() {})
			select {
			case req := <-client.requests:
				text, images := lastUserInput(req)
				if image {
					if len(images) != 1 || !bytes.Equal(images[0].Data, data) {
						t.Fatal("side-chat image content missing")
					}
				} else if !strings.Contains(text, string(data)) {
					t.Fatal("side-chat text content missing")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("side-chat prompt not submitted")
			}
			if len(main.Messages()) != 0 {
				t.Fatal("side-chat attachment changed the main transcript")
			}
		})
	}
}

func TestBtwContentErrorsPreserveInputAndLayout(t *testing.T) {
	root := t.TempDir()
	jail := tools.NewSandbox(root)
	jail.Lock()
	d := newBtwDialog()
	d.sandbox = jail
	d.Open(tui.Theme{}, core.NewAgent(nil, "test", "", nil), "", "test", root, "", false, false, true, nil)
	defer d.Close()
	input := "unique editor input [content:missing.txt]"
	d.editor.SetValue(input)
	d.HandleKey(tui.Key{Kind: tui.KeyEnter}, func() {})
	if d.editor.Value() != input || d.Loading() || len(d.turns) != 0 || !strings.Contains(d.inputErr, "attachment:") {
		t.Fatal("side-chat file error lost input or started a turn")
	}
	rows := padDialogFrame(d.Render(tui.Theme{}, 100))
	row, _ := d.CursorPos(100)
	if row < 0 || row >= len(rows) || !strings.Contains(rows[row], "unique editor input") {
		t.Fatalf("attachment error shifted cursor away from editor: row %d", row)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.fileContext = ctx
	d.submit(nil)
	if !strings.Contains(d.inputErr, "canceled") || d.editor.Value() != input {
		t.Fatal("cancelled side-chat file read was admitted")
	}
}
