package modes

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestSwarmEditorsTransferImageSnapshotsAndPreserveFailures(t *testing.T) {
	for _, followUp := range []bool{false, true} {
		for _, attached := range []bool{false, true} {
			t.Run(fmt.Sprintf("follow-up=%v/attached=%v", followUp, attached), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "image.png")
				original := transferImage(t).Data
				if err := os.WriteFile(path, original, 0o600); err != nil {
					t.Fatal(err)
				}
				d := newSwarmDialog()
				d.cwd = root
				d.transferFiles = attached
				var received []provider.ImageBlock
				fail := true
				capture := func(images []provider.ImageBlock) error {
					if fail {
						return fmt.Errorf("adapter failure")
					}
					received = images
					return os.Remove(path)
				}
				d.spawnWithImages = func(_, _, _ string, images []provider.ImageBlock) error { return capture(images) }
				d.sendWithImages = func(id, _ string, images []provider.ImageBlock) error {
					if id != "target" {
						t.Fatalf("unexpected target %q", id)
					}
					return capture(images)
				}
				d.openSpawnEditor(tui.Theme{})
				ed := d.newTaskEd
				handle := d.handleSpawnKey
				if followUp {
					d.openPromptEditor(tui.Theme{}, "target")
					ed = d.promptEd
					handle = d.handlePromptKey
				}
				trigger := "!@"
				chip := "[content:image.png] "
				if attached {
					trigger = "@"
					chip = "[file:image.png] "
				}
				ed.SetValue(trigger + "image")
				handle(tui.Key{Kind: tui.KeyEnter})
				if ed.Value() != chip {
					t.Fatalf("picker failed: %q", ed.Value())
				}
				_, _, errMsg := handle(tui.Key{Kind: tui.KeyEnter})
				if errMsg == "" || ed.Value() != chip {
					t.Fatal("adapter error lost image input")
				}
				fail = false
				_, _, errMsg = handle(tui.Key{Kind: tui.KeyEnter})
				if errMsg != "" || len(received) != 1 || !bytes.Equal(received[0].Data, original) {
					t.Fatalf("image snapshot missing after file deletion: %s", errMsg)
				}
			})
		}
	}
}
