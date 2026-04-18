package modes

import (
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/internal/tui"
)

// helpKeyRows is the list of keybindings shown by /help.
var helpKeyRows = [][2]string{
	{"enter", "submit the current input"},
	{"alt+enter", "insert a newline"},
	{"tab", "complete the highlighted slash command"},
	{"esc", "cancel the current turn (while busy) · clear the input (while idle)"},
	{"ctrl+c", "exit (while idle) · cancel the current turn (while busy)"},
	{"ctrl+w", "delete previous word"},
	{"alt+backspace", "delete previous word (same as ctrl+w)"},
	{"ctrl+u / ctrl+k", "delete to start / end of line"},
	{"ctrl+a / ctrl+e", "jump to start / end of line"},
	{"alt+← / alt+→", "jump one word back / forward"},
	{"ctrl+l", "redraw the screen"},
	{"ctrl+o", "expand / collapse long tool results"},
	{"pgup / pgdn", "scroll the chat one page up / down"},
	{"up / down", "scroll by 3 lines (when input is empty) · prompt history (otherwise)"},
}

// renderHelpBlock builds the friendly /help view. Uses the shared
// frameHeader/frameRule helpers so the rules match every other block
// in the tui (tool results, code fences, dialogs) — full terminal width
// in the muted colour.
func renderHelpBlock(th tui.Theme, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	out = append(out, frameHeader(th, "zot help", width))

	// commands section
	out = append(out, tui.Bold("slash commands:"))
	for _, c := range slashCatalog {
		name := c.Name
		if len(name) < 10 {
			name = name + strings.Repeat(" ", 10-len(name))
		}
		out = append(out, fmt.Sprintf("  %s  %s",
			th.FG256(th.Accent, name),
			th.FG256(th.Muted, c.Desc)))
	}

	// keys section
	out = append(out, "", tui.Bold("keys:"))
	for _, k := range helpKeyRows {
		name := k[0]
		if len(name) < 14 {
			name = name + strings.Repeat(" ", 14-len(name))
		}
		out = append(out, fmt.Sprintf("  %s  %s",
			th.FG256(th.Accent, name),
			th.FG256(th.Muted, k[1])))
	}

	out = append(out, frameRule(th, width), "")
	return out
}
