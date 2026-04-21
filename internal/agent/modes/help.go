package modes

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/patriceckhart/zot/internal/tui"
)

// helpKeyRows is the list of keybindings shown by /help.
var helpKeyRows = [][2]string{
	{"enter", "submit the current input"},
	{"alt+enter", "insert a newline"},
	{"tab", "complete the highlighted slash command"},
	{"esc", "cancel the current turn (while busy) - clear the input (while idle)"},
	{"ctrl+c", "exit (while idle) - cancel the current turn (while busy)"},
	{"ctrl+w", "delete previous word"},
	{"alt+backspace", "delete previous word (same as ctrl+w)"},
	{"ctrl+u / ctrl+k", "delete to start / end of line"},
	{"ctrl+a / ctrl+e", "jump to start / end of line"},
	{"alt+← / alt+→", "jump one word back / forward"},
	{"ctrl+l", "redraw the screen"},
	{"ctrl+o", "expand / collapse long tool results"},
	{"pgup / pgdn", "scroll the chat one page up / down"},
	{"up / down", "scroll by 3 lines (when input is empty) - prompt history (otherwise)"},
}

// renderHelpBlock builds the friendly /help view. Uses the shared
// frameHeader/frameRule helpers so the rules match every other block
// in the tui (tool results, code fences, dialogs) — full terminal width
// in the muted colour.
//
// Slash commands and keybindings share the same label column width so
// every description starts at the same x-position, regardless of which
// section it lives in. The width is computed from the longest label
// across BOTH lists, with a minimum of 14 cells so changes to either
// list don't compress the column visually.
func renderHelpBlock(th tui.Theme, width int) []string {
	if width < 20 {
		width = 20
	}

	// Label column width uses display cells, not byte length, so
	// single-cell multibyte runes (← → - …) don't over-count and leave
	// a raggedy right edge. `len("alt+← / alt+→")` is 17 bytes but
	// only 13 cells; padding off byte length would either overshoot
	// (setting labelWidth too high and wasting space on every row)
	// or undershoot (never padding that row because len >= labelWidth
	// already, leaving its description mis-aligned).
	labelWidth := 14
	for _, c := range slashCatalog {
		if n := runewidth.StringWidth(c.Name); n > labelWidth {
			labelWidth = n
		}
	}
	for _, k := range helpKeyRows {
		if n := runewidth.StringWidth(k[0]); n > labelWidth {
			labelWidth = n
		}
	}

	pad := func(s string) string {
		w := runewidth.StringWidth(s)
		if w >= labelWidth {
			return s
		}
		return s + strings.Repeat(" ", labelWidth-w)
	}

	var out []string
	out = append(out, frameHeader(th, "zot help", width))

	// commands section
	out = append(out, tui.Bold("slash commands:"))
	for _, c := range slashCatalog {
		out = append(out, fmt.Sprintf("  %s  %s",
			th.FG256(th.Accent, pad(c.Name)),
			th.FG256(th.Muted, c.Desc)))
	}

	// keys section
	out = append(out, "", tui.Bold("keys:"))
	for _, k := range helpKeyRows {
		out = append(out, fmt.Sprintf("  %s  %s",
			th.FG256(th.Accent, pad(k[0])),
			th.FG256(th.Muted, k[1])))
	}

	out = append(out, frameRule(th, width), "")
	return out
}
