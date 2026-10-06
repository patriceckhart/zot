package modes

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

// TestExtensionDisplayActionInvalidates is a regression test for the
// `display` command action. Appending the note must also wake the render
// loop; otherwise the text is invisible until the next keypress triggers an
// unrelated repaint. The `notify` hook and the `insert` action already
// invalidate, and `display` shares their note-rendering path.
func TestExtensionDisplayActionInvalidates(t *testing.T) {
	i := newNotesTestInteractive()

	i.applyExtensionCommandResponse("demo", extproto.CommandResponseFromExt{
		Action:  "display",
		Display: "display note",
	})

	select {
	case <-i.dirty:
	default:
		t.Fatal("display action appended the note but did not invalidate the view")
	}

	if len(i.extNotes) != 1 || !strings.Contains(i.extNotes[0], "display note") {
		t.Fatalf("expected one display note containing the text, got %v", i.extNotes)
	}
}
