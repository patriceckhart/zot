package tui

import (
	"fmt"
	"strings"
	"testing"
)

// makeLines returns a body with n lines of short text — useful for
// exercising the line-count trigger without also tripping the
// character-count one.
func makeLines(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("line %d", i+1)
	}
	return strings.Join(parts, "\n")
}

// TestPasteCollapseLineTrigger verifies that a paste with more than
// ten lines gets collapsed to the "+L lines" placeholder shape.
// The full body is preserved and expanded back by SubmitValue.
func TestPasteCollapseLineTrigger(t *testing.T) {
	e := NewEditor("▌ ")
	body := makeLines(11)
	e.HandleKey(Key{Kind: KeyPaste, Paste: body})

	got := e.Value()
	want := "[pasted text #1 +11 lines]"
	if got != want {
		t.Fatalf("editor Value: want %q, got %q", want, got)
	}
	if e.SubmitValue() != body {
		t.Fatalf("SubmitValue didn't expand placeholder: got %q", e.SubmitValue())
	}
}

// TestPasteCollapseCharTrigger verifies that a paste with more than
// 1000 characters but few enough lines collapses to the "C chars"
// placeholder shape (long single-line / near-single-line dumps).
func TestPasteCollapseCharTrigger(t *testing.T) {
	e := NewEditor("▌ ")
	body := strings.Repeat("a", 1500) // 1 line, 1500 chars
	e.HandleKey(Key{Kind: KeyPaste, Paste: body})

	got := e.Value()
	want := "[pasted text #1 1500 chars]"
	if got != want {
		t.Fatalf("editor Value: want %q, got %q", want, got)
	}
	if e.SubmitValue() != body {
		t.Fatalf("SubmitValue didn't expand placeholder: got %q", e.SubmitValue())
	}
}

// TestPasteCollapseLinePrecedence verifies that when a paste trips
// both thresholds, the line-count marker wins. "12 lines, 4000
// chars" should read as "+12 lines", not "4000 chars".
func TestPasteCollapseLinePrecedence(t *testing.T) {
	e := NewEditor("▌ ")
	// 12 lines, each 400 chars = 12 lines, ~4800 chars.
	parts := make([]string, 12)
	for i := range parts {
		parts[i] = strings.Repeat("x", 400)
	}
	body := strings.Join(parts, "\n")
	e.HandleKey(Key{Kind: KeyPaste, Paste: body})

	if !strings.HasPrefix(e.Value(), "[pasted text #1 +12 lines") {
		t.Errorf("want line-trigger placeholder, got %q", e.Value())
	}
}

// TestPasteCollapseFallthrough ensures short pastes (under both
// thresholds) are NOT collapsed — 1-10 lines with < 1000 chars
// should appear inline so the user can edit them in place.
func TestPasteCollapseFallthrough(t *testing.T) {
	cases := map[string]string{
		"single line":          "hello world",
		"two lines":            "line1\nline2",
		"ten lines (at limit)": makeLines(10),
		"exactly 1000 chars":   strings.Repeat("a", 1000),
		"multiline under caps": "aaa\nbbb\nccc\nddd\neee",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := NewEditor("▌ ")
			e.HandleKey(Key{Kind: KeyPaste, Paste: body})
			if strings.Contains(e.Value(), "[pasted text #") {
				t.Errorf("%s should NOT collapse, got %q", name, e.Value())
			}
		})
	}
}

// TestPasteCollapseSequentialIDs makes sure two separate large
// pastes get distinct ids and both expand correctly in
// SubmitValue, even when they use different placeholder shapes.
func TestPasteCollapseSequentialIDs(t *testing.T) {
	e := NewEditor("▌ ")
	a := makeLines(12)             // line trigger
	b := strings.Repeat("y", 1500) // char trigger, single line
	e.HandleKey(Key{Kind: KeyPaste, Paste: a})
	e.Insert(" ")
	e.HandleKey(Key{Kind: KeyPaste, Paste: b})

	visible := e.Value()
	if !strings.Contains(visible, "[pasted text #1 +12 lines]") ||
		!strings.Contains(visible, "[pasted text #2 1500 chars]") {
		t.Fatalf("expected mixed-shape placeholders in %q", visible)
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
	e.HandleKey(Key{Kind: KeyPaste, Paste: makeLines(11)})
	e.Clear()

	// A fresh paste must start at id #1 again.
	e.HandleKey(Key{Kind: KeyPaste, Paste: makeLines(11)})
	if !strings.Contains(e.Value(), "[pasted text #1 ") {
		t.Errorf("Clear didn't reset pasteSeq: %q", e.Value())
	}
}
