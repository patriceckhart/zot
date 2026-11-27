package modes

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/tui"
)

// settingsTestItems builds a list long enough to overflow a short
// terminal. Every entry carries a description that wraps, so an entry
// costs more than one row and the window has to account for it.
func settingsTestItems(n int) []settingsItem {
	items := make([]settingsItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, settingsItem{
			key:   fmt.Sprintf("item_%02d", i),
			label: fmt.Sprintf("entry %02d", i),
			desc:  "description text that wraps onto several rows at the widths these tests render so every entry costs more than a single line",
		})
	}
	return items
}

func renderedPlain(lines []string) string { return stripANSIBytes(strings.Join(lines, "\n")) }

func TestSettingsDialogWindowsListToMaxRows(t *testing.T) {
	d := newSettingsDialog()
	d.Open(settingsTestItems(20))
	d.MaxRows = 12

	lines := d.Render(tui.Dark, 80)
	if len(lines) > d.MaxRows {
		t.Fatalf("rendered %d rows, want <= %d", len(lines), d.MaxRows)
	}
	text := renderedPlain(lines)
	for _, want := range []string{"entry 00", "entry 01", "more below"} {
		if !strings.Contains(text, want) {
			t.Fatalf("top of window missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "entry 19") {
		t.Fatalf("window rendered the far tail of the list:\n%s", text)
	}
}

func TestSettingsDialogWindowFollowsCursorToBothEnds(t *testing.T) {
	d := newSettingsDialog()
	d.Open(settingsTestItems(20))
	d.MaxRows = 10

	for i := 1; i < 20; i++ {
		d.HandleKey(tui.Key{Kind: tui.KeyDown})
		lines := d.Render(tui.Dark, 80)
		if len(lines) > d.MaxRows {
			t.Fatalf("cursor at %d: rendered %d rows, want <= %d", i, len(lines), d.MaxRows)
		}
		if text := renderedPlain(lines); !strings.Contains(text, fmt.Sprintf("entry %02d", i)) {
			t.Fatalf("cursor entry %02d scrolled out of view:\n%s", i, text)
		}
	}
	top := d.Render(tui.Dark, 80)
	if text := renderedPlain(top); !strings.Contains(text, "more above") {
		t.Fatalf("bottom of list missing the above marker:\n%s", text)
	}

	for i := 18; i >= 0; i-- {
		d.HandleKey(tui.Key{Kind: tui.KeyUp})
		lines := d.Render(tui.Dark, 80)
		if text := renderedPlain(lines); !strings.Contains(text, fmt.Sprintf("entry %02d", i)) {
			t.Fatalf("cursor entry %02d scrolled out of view on the way back:\n%s", i, text)
		}
	}
	if text := renderedPlain(d.Render(tui.Dark, 80)); !strings.Contains(text, "entry 00") {
		t.Fatalf("first entry not visible after scrolling back up:\n%s", text)
	}
}

func TestSettingsDialogOptionsWindowFollowsCursor(t *testing.T) {
	item := settingsItem{key: "theme", label: "color theme"}
	for i := 0; i < 30; i++ {
		item.options = append(item.options, settingsOption{
			value: fmt.Sprintf("opt_%02d", i),
			label: fmt.Sprintf("option %02d", i),
			desc:  "option description that also wraps onto several rows at the widths these tests render",
		})
	}
	d := newSettingsDialog()
	d.OpenDirectOption(item)
	d.MaxRows = 14

	for i := 1; i < 30; i++ {
		d.HandleKey(tui.Key{Kind: tui.KeyDown})
		lines := d.Render(tui.Dark, 80)
		if len(lines) > d.MaxRows {
			t.Fatalf("option cursor at %d: rendered %d rows, want <= %d", i, len(lines), d.MaxRows)
		}
		if text := renderedPlain(lines); !strings.Contains(text, fmt.Sprintf("option %02d", i)) {
			t.Fatalf("option %02d scrolled out of view:\n%s", i, text)
		}
	}
	last := d.Render(tui.Dark, 80)
	if text := renderedPlain(last); !strings.Contains(text, "more above") || !strings.Contains(text, "option 29") {
		t.Fatalf("bottom of option list not windowed on the cursor:\n%s", text)
	}
}

func TestSettingsDialogShowsRunningVersionInFooter(t *testing.T) {
	const version = "1.2.3 (abcdef0, 2026-11-27)"
	i := NewInteractive(InteractiveConfig{Version: version})
	i.openSettingsDialog()
	d := i.settingsDialog
	d.MaxRows = 6

	lines := d.Render(tui.Dark, 80)
	if len(lines) != d.MaxRows {
		t.Fatalf("rendered %d rows, want %d", len(lines), d.MaxRows)
	}
	if got := stripANSIBytes(lines[len(lines)-3]); strings.TrimSpace(got) != "" {
		t.Fatalf("expected a blank line before the footer, got %q", got)
	}
	if got := stripANSIBytes(lines[len(lines)-2]); got != "zot "+version {
		t.Fatalf("footer = %q, want %q", got, "zot "+version)
	}
	if text := renderedPlain(lines); !strings.Contains(text, "render images when supported") {
		t.Fatalf("first settings entry missing:\n%s", text)
	}

	d.MaxRows = 4
	lines = d.Render(tui.Dark, 80)
	if text := renderedPlain(lines); len(lines) > d.MaxRows || strings.Contains(text, version) || !strings.Contains(text, "render images when supported") {
		t.Fatalf("expected the selected setting to take priority over the footer:\n%s", text)
	}
}

func TestSettingsDialogKeepsCursorRowOnTinyTerminal(t *testing.T) {
	d := newSettingsDialog()
	d.Open(settingsTestItems(20))
	// Four rows is the dialog's floor: header, hint, one row of list,
	// rule. The cursor entry must still be the row that survives.
	d.MaxRows = 4
	for i := 0; i < 20; i++ {
		lines := d.Render(tui.Dark, 80)
		if len(lines) > d.MaxRows {
			t.Fatalf("cursor at %d: rendered %d rows, want <= %d", i, len(lines), d.MaxRows)
		}
		if text := renderedPlain(lines); !strings.Contains(text, fmt.Sprintf("entry %02d", i)) {
			t.Fatalf("cursor entry %02d missing at the row floor:\n%s", i, text)
		}
		d.HandleKey(tui.Key{Kind: tui.KeyDown})
	}
}

func TestSettingsOptionPickerFitsShortTerminal(t *testing.T) {
	for _, rows := range []int{12, 14, 16, 17, 24} {
		t.Run(fmt.Sprintf("rows=%d", rows), func(t *testing.T) {
			term := &shortTestTerminal{cols: 80, rows: rows}
			i := NewInteractive(InteractiveConfig{Terminal: term})
			i.settingsDialog.OpenDirectOption(settingsItem{
				label: "reasoning level", desc: "reasoning depth for reasoning-capable models",
				options: []settingsOption{{label: "off"}, {label: "low"}},
			})
			i.redraw()

			lines := i.settingsDialog.Render(i.cfg.Theme, 80)
			if len(lines) > i.settingsDialog.MaxRows {
				t.Fatalf("option picker rendered %d rows, cap %d", len(lines), i.settingsDialog.MaxRows)
			}
			text := renderedPlain(lines)
			if !strings.Contains(text, "off") {
				t.Fatalf("selected option missing from window:\n%s", text)
			}
			if i.settingsDialog.MaxRows >= 5 && !strings.Contains(text, "reasoning depth") {
				t.Fatalf("description missing when it fits:\n%s", text)
			}
			written := strings.Count(term.String(), "\r\n") + 1
			band := written - len(i.cachedChatLocked(80))
			if band > rows-1 {
				t.Fatalf("bottom band is %d rows for a %d-row terminal", band, rows)
			}
		})
	}
}

func TestSettingsDialogRendersEverythingWithoutMaxRows(t *testing.T) {
	d := newSettingsDialog()
	d.Open(settingsTestItems(20))

	text := renderedPlain(d.Render(tui.Dark, 80))
	for _, want := range []string{"entry 00", "entry 19"} {
		if !strings.Contains(text, want) {
			t.Fatalf("unbounded render missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "more above") || strings.Contains(text, "more below") {
		t.Fatalf("unbounded render drew window markers:\n%s", text)
	}
}

// shortTestTerminal is a Terminal with a configurable size so the
// layout can be exercised at heights a real window would report.
type shortTestTerminal struct {
	bytes.Buffer
	cols, rows int
}

func (t *shortTestTerminal) Size() (int, int)       { return t.cols, t.rows }
func (t *shortTestTerminal) OnResize(func())        {}
func (t *shortTestTerminal) SetNonblock(bool) error { return nil }
func (t *shortTestTerminal) ReadByte() (byte, error) {
	return 0, io.EOF
}
func (t *shortTestTerminal) EnterRaw() (func() error, error) {
	return func() error { return nil }, nil
}
func (t *shortTestTerminal) PeekByteTimeout(time.Duration) (byte, bool, error) {
	return 0, false, io.EOF
}

func TestSettingsDialogIsCappedToShortTerminal(t *testing.T) {
	for _, rows := range []int{12, 14, 16, 24, 40, 60} {
		t.Run(fmt.Sprintf("rows=%d", rows), func(t *testing.T) {
			term := &shortTestTerminal{cols: 80, rows: rows}
			i := NewInteractive(InteractiveConfig{Terminal: term})
			i.openSettingsDialog()
			if !i.settingsDialog.Active() {
				t.Fatal("settings dialog did not open")
			}
			i.redraw()

			if i.settingsDialog.MaxRows <= 0 || i.settingsDialog.MaxRows >= term.rows {
				t.Fatalf("dialog cap = %d rows for a %d-row terminal", i.settingsDialog.MaxRows, term.rows)
			}
			lines := i.settingsDialog.Render(i.cfg.Theme, 80)
			if len(lines) > i.settingsDialog.MaxRows {
				t.Fatalf("dialog rendered %d rows, cap %d", len(lines), i.settingsDialog.MaxRows)
			}
			text := renderedPlain(lines)
			if !strings.Contains(text, "render images when supported") {
				t.Fatalf("first settings entry missing from the window:\n%s", text)
			}

			// The band the dialog draws into must fit above the
			// terminal's bottom edge: written lines are the chat, the
			// bottom band, and the renderer's trailing margin row. A
			// taller band scrolls the top of the dialog out of reach
			// again.
			written := strings.Count(term.String(), "\r\n") + 1
			band := written - len(i.cachedChatLocked(80))
			if band > term.rows-1 {
				t.Fatalf("bottom band is %d rows for a %d-row terminal; dialog would be clipped", band, term.rows)
			}
		})
	}
}
