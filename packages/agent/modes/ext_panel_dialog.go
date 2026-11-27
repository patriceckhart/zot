package modes

import (
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/tui"
)

type extPanelDialog struct {
	active bool
	ext    string
	id     string
	title  string
	lines  []string
	footer string
	width  int // last width reported to the extension
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
	d.width = 0
}

// reportExtPanelSizeLocked reports the same column count used for wrapping.
func (i *Interactive) reportExtPanelSizeLocked(cols, rows int) {
	d := i.extPanel
	if i.cfg.Extensions == nil || !d.Active() || cols <= 0 || cols == d.width {
		return
	}
	d.width = cols
	_ = i.cfg.Extensions.SendPanelResize(d.ext, extproto.PanelResizeFromHost{
		PanelID: d.id, Width: cols, Height: rows,
	})
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
// The host wraps lines even if the extension does not size them itself.
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
		// highlight every resulting row. Wrapping emits the original marker
		// only once, so leave continuation content intact.
		rows := tui.WrapANSILine(plain, width)
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, styleExtPanelSelectedRow(th, row, width))
		}
		return out
	case raw != plain:
		// Extension-provided ANSI: carry active styles into continuation
		// rows and reset each row so extension styling cannot leak.
		// Keep an editor's leading margin on every continuation row.
		content := strings.TrimLeft(raw, " ")
		indent := len(raw) - len(content)
		if indent > 0 && width > indent {
			rows := tui.WrapStyledANSILine(content, width-indent)
			for i := range rows {
				rows[i] = raw[:indent] + rows[i]
			}
			return rows
		}
		return tui.WrapStyledANSILine(raw, width)
	default:
		rows := tui.WrapANSILine(plain, width)
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			// Style each text segment separately so a checkmark's reset
			// cannot discard the body's foreground color.
			parts := strings.Split(row, "✓")
			for i := range parts {
				parts[i] = th.FG256(th.FG, parts[i])
			}
			out = append(out, strings.Join(parts, th.FG256(th.Tool, "✓")))
		}
		return out
	}
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
