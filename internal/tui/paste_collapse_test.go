package tui

import (
	"strings"
	"testing"
)

// TestPasteCollapseInsertsPlaceholder verifies that a multi-line
// paste gets collapsed to the "[paste #N +L lines]" placeholder
// in the editor buffer, leaving the full body behind the scenes.
func TestPasteCollapseInsertsPlaceholder(t *testing.T) {
	e := NewEditor("▌ ")
	body := "line1\nline2\nline3\nline4"
	e.HandleKey(Key{Kind: KeyPaste, Paste: body})

	got := e.Value()
	want := "[pasted text #1 +4 lines]"
	if got != want {
		t.Fatalf("editor Value: want %q, got %q", want, got)
	}
	if e.SubmitValue() != body {
		t.Fatalf("SubmitValue didn't expand placeholder: got %q", e.SubmitValue())
	}
}

// TestPasteCollapseSingleLineFallthrough ensures short pastes are
// NOT collapsed — single-line drag-drop file paths and short
// two-line snippets should appear inline so the user can edit
// them in place.
func TestPasteCollapseSingleLineFallthrough(t *testing.T) {
	e := NewEditor("▌ ")
	e.HandleKey(Key{Kind: KeyPaste, Paste: "hello world"})
	if strings.Contains(e.Value(), "[pasted text #") {
		t.Errorf("single-line paste should not collapse, got %q", e.Value())
	}

	e2 := NewEditor("▌ ")
	e2.HandleKey(Key{Kind: KeyPaste, Paste: "line1\nline2"})
	if strings.Contains(e2.Value(), "[pasted text #") {
		t.Errorf("two-line paste should not collapse, got %q", e2.Value())
	}
}

// TestPasteCollapseSequentialIDs makes sure two separate pastes get
// distinct ids and both expand correctly in SubmitValue.
func TestPasteCollapseSequentialIDs(t *testing.T) {
	e := NewEditor("▌ ")
	a := "aaa\nbbb\nccc"
	b := "xxx\nyyy\nzzz"
	e.HandleKey(Key{Kind: KeyPaste, Paste: a})
	e.Insert(" ")
	e.HandleKey(Key{Kind: KeyPaste, Paste: b})

	visible := e.Value()
	if !strings.Contains(visible, "[pasted text #1 +3 lines]") ||
		!strings.Contains(visible, "[pasted text #2 +3 lines]") {
		t.Fatalf("expected two placeholders in %q", visible)
	}

	full := e.SubmitValue()
	if !strings.Contains(full, a) || !strings.Contains(full, b) {
		t.Fatalf("SubmitValue missing bodies: %q", full)
	}
}

// TestPasteCollapseClearResetsMap verifies that Clear drops the
// stored pastes so stale ids can't leak into a follow-up turn
// (same placeholder number could otherwise be reused to expand to
// the wrong body).
func TestPasteCollapseClearResetsMap(t *testing.T) {
	e := NewEditor("▌ ")
	e.HandleKey(Key{Kind: KeyPaste, Paste: "a\nb\nc"})
	e.Clear()

	// A fresh paste must start at id #1 again.
	e.HandleKey(Key{Kind: KeyPaste, Paste: "d\ne\nf"})
	if !strings.Contains(e.Value(), "[pasted text #1 ") {
		t.Errorf("Clear didn't reset pasteSeq: %q", e.Value())
	}
}
