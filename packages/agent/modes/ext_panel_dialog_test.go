package modes

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

// TestExtPanelDialogWrapsLongLines is a regression test for the extension
// panel text-wrapping bug. Extensions never receive the terminal width, so
// the host owns wrapping. Before this fix each panel line was emitted whole
// and the renderer's hard truncation (tui.truncateToWidth) silently cut off
// everything past the right edge.
func TestExtPanelDialogWrapsLongLines(t *testing.T) {
	const width = 30
	long := "alpha bravo charlie delta echo foxtrot golf hotel india juliet"
	d := newExtPanelDialog()
	d.Open("demo", "main", "Demo", []string{long}, "esc close")

	rows := d.Render(tui.Theme{}, width)
	assertRowsFitWidth(t, rows, width)

	plain := stripANSIBytes(strings.Join(rows, "\n"))
	for _, word := range strings.Fields(long) {
		if !strings.Contains(plain, word) {
			t.Fatalf("wrapped panel lost word %q:\n%s", word, plain)
		}
	}
	// A single unwrapped line renders as header + one body row + blank +
	// footer + rule (5 rows). Wrapping must add body rows.
	if len(rows) <= 5 {
		t.Fatalf("long line was not wrapped; got %d rows:\n%s", len(rows), plain)
	}
}

// TestExtPanelDialogWrapsSelectedLine checks that a selected row keeps its
// highlight across the wrapped continuation rows and that the visible marker
// is not repeated on every row.
func TestExtPanelDialogWrapsSelectedLine(t *testing.T) {
	const width = 24
	d := newExtPanelDialog()
	d.Open("demo", "main", "Demo", []string{"▸ alpha bravo charlie delta echo"}, "")

	rows := d.Render(tui.Theme{}, width)
	assertRowsFitWidth(t, rows, width)

	base := tui.Theme{}.SelectionStyle()
	var selected []string
	for _, r := range rows {
		if strings.HasPrefix(r, base) {
			selected = append(selected, r)
		}
	}
	if len(selected) < 2 {
		t.Fatalf("selected long line should wrap to >=2 highlighted rows, got %d:\n%s",
			len(selected), strings.Join(rows, "\n"))
	}
	markers := 0
	for _, r := range selected {
		if strings.Contains(stripANSIBytes(r), "▸") {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("selection marker should appear once, got %d:\n%s",
			markers, strings.Join(rows, "\n"))
	}
	plain := stripANSIBytes(strings.Join(rows, "\n"))
	for _, word := range []string{"alpha", "echo"} {
		if !strings.Contains(plain, word) {
			t.Fatalf("wrapped selected line lost %q:\n%s", word, plain)
		}
	}
}

// TestExtPanelDialogKeepsShortLinesOnOneRow guards the common case: a line
// that already fits must not be split or reflowed.
func TestExtPanelDialogKeepsShortLinesOnOneRow(t *testing.T) {
	d := newExtPanelDialog()
	d.Open("demo", "main", "Demo", []string{"hello world"}, "esc close")

	rows := d.Render(tui.Theme{}, 60)
	var body []string
	for _, r := range rows {
		if strings.Contains(stripANSIBytes(r), "hello world") {
			body = append(body, r)
		}
	}
	if len(body) != 1 {
		t.Fatalf("short line should render on exactly one row, got %d:\n%s",
			len(body), strings.Join(rows, "\n"))
	}
}

// TestExtPanelDialogWrapsFooter checks the footer hint wraps like the body
// instead of being clipped.
func TestExtPanelDialogWrapsFooter(t *testing.T) {
	const width = 20
	d := newExtPanelDialog()
	d.Open("demo", "main", "Demo", nil, "press a to add, x to complete, esc to close")

	rows := d.Render(tui.Theme{}, width)
	assertRowsFitWidth(t, rows, width)

	plain := stripANSIBytes(strings.Join(rows, "\n"))
	for _, word := range []string{"press", "complete", "close"} {
		if !strings.Contains(plain, word) {
			t.Fatalf("wrapped footer lost %q:\n%s", word, plain)
		}
	}
}

// TestExtPanelDialogWrapsStyledLinePreservingANSI checks that a line carrying
// extension-provided ANSI escapes is wrapped without dropping content or
// leaking escape sequences into the visible width.
func TestExtPanelDialogWrapsStyledLinePreservingANSI(t *testing.T) {
	const width = 16
	styled := "\x1b[31mred text that is quite long indeed\x1b[0m"
	d := newExtPanelDialog()
	d.Open("demo", "main", "Demo", []string{styled}, "")

	rows := d.Render(tui.Theme{}, width)
	assertRowsFitWidth(t, rows, width)

	plain := stripANSIBytes(strings.Join(rows, "\n"))
	for _, word := range []string{"red", "indeed"} {
		if !strings.Contains(plain, word) {
			t.Fatalf("wrapped styled line lost %q:\n%s", word, plain)
		}
	}
}
