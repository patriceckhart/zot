package modes

import (
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/tui"
)

type extPanelDialog struct {
	active bool
	ext    string
	id     string
	title  string
	lines  []string
	footer string
}

func newExtPanelDialog() *extPanelDialog { return &extPanelDialog{} }

func (d *extPanelDialog) Active() bool { return d != nil && d.active }

func (d *extPanelDialog) Open(ext, id, title string, lines []string, footer string) {
	d.active = true
	d.ext = ext
	d.id = id
	d.title = title
	d.lines = append([]string(nil), lines...)
	d.footer = footer
}

func (d *extPanelDialog) Update(title string, lines []string, footer string) {
	if !d.active {
		return
	}
	if title != "" {
		d.title = title
	}
	d.lines = append(d.lines[:0], lines...)
	d.footer = footer
}

func (d *extPanelDialog) Close() {
	d.active = false
	d.ext = ""
	d.id = ""
	d.title = ""
	d.lines = nil
	d.footer = ""
}

func (d *extPanelDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	title := d.title
	if title == "" {
		title = d.ext
	}
	out := []string{frameHeaderColor(th, title, width, th.Accent)}
	for _, l := range d.lines {
		out = append(out, renderExtPanelLine(th, l, width)...)
	}
	if strings.TrimSpace(d.footer) != "" {
		out = append(out, "")
		out = append(out, renderExtPanelFooter(th, d.footer, width)...)
	}
	out = append(out, frameRuleColor(th, width, th.Accent))
	return out
}

// renderExtPanelLine renders one extension-supplied panel line, folding it
// across as many rows as needed so long content wraps instead of being
// clipped by the renderer's hard truncation (see tui.truncateToWidth).
//
// Extensions never receive the panel width, so the host owns wrapping;
// otherwise any line wider than the terminal is silently cut off.
func renderExtPanelLine(th tui.Theme, raw string, width int) []string {
	plain := stripANSIBytes(raw)
	trimmed := strings.TrimLeft(plain, " ")
	// Selection markers, in order of precedence:
	//   "▸ " / "● "  visible glyph the user sees
	//   "\u200b"      invisible zero-width-space sentinel for
	//                 extensions that want the row highlight
	//                 without rendering an arrow
	selected := strings.HasPrefix(trimmed, "▸ ") ||
		strings.HasPrefix(trimmed, "● ") ||
		strings.HasPrefix(trimmed, "\u200b")

	switch {
	case selected:
		// Selection styling replaces any extension ANSI (matching the
		// historical single-row behavior), so wrap the stripped text and
		// highlight every resulting row. The arrow marker belongs only
		// on the first row so continuation rows read as body text.
		rows := tui.WrapANSILine(plain, width)
		out := make([]string, 0, len(rows))
		for i, row := range rows {
			if i > 0 {
				row = stripExtPanelSelectionMarker(row)
			}
			out = append(out, styleExtPanelSelectedRow(th, row, width))
		}
		return out
	case raw != plain:
		// Extension-provided ANSI: wrap while preserving the escapes and
		// leave the styling to the extension.
		rows := tui.WrapANSILine(raw, width)
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row+"\x1b[0m")
		}
		return out
	default:
		rows := tui.WrapANSILine(plain, width)
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			styled := th.FG256(th.Muted, row)
			styled = strings.ReplaceAll(styled, "✓", th.FG256(th.Tool, "✓"))
			out = append(out, styled)
		}
		return out
	}
}

// stripExtPanelSelectionMarker removes a leading selection glyph from a
// wrapped continuation row. Wrapping happens on spaces so the marker normally
// lands on the first row; this only matters when the first row is narrower
// than the marker itself.
func stripExtPanelSelectionMarker(row string) string {
	for _, m := range []string{"▸ ", "● "} {
		if strings.HasPrefix(row, m) {
			return row[len(m):]
		}
	}
	return strings.TrimPrefix(row, "\u200b")
}

// styleExtPanelSelectedRow paints one row of a selected panel line with the
// theme's selection colors and pads it to the full panel width so the
// highlight spans the row even after wrapping.
func styleExtPanelSelectedRow(th tui.Theme, row string, width int) string {
	if visible := runewidth.StringWidth(row); visible < width {
		row += strings.Repeat(" ", width-visible)
	}
	base := th.SelectionStyle()
	green := th.SelectionStyleFG(th.Tool)
	return base + strings.ReplaceAll(row, "✓", green+"✓"+base) + "\x1b[0m"
}

// renderExtPanelFooter wraps the footer hint to the panel width so a long
// "press ... to ..." line is not clipped on narrow terminals.
func renderExtPanelFooter(th tui.Theme, footer string, width int) []string {
	rows := tui.WrapANSILine(footer, width)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, th.FG256(th.Muted, row))
	}
	return out
}
