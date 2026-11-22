package modes

import (
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/tui"
)

// fitAuthRows fits the padded sign-in dialog after the host knows the space
// used by the editor and status. Arrow keys expose every row, including URLs
// too long to fit vertically. Editing the redirect field brings it into view.
func (d *mcpManagerDialog) fitAuthRows(th tui.Theme, width, rows int, lines []string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busy == "" || d.authURL == "" {
		return lines
	}
	var body []string
	for _, line := range lines {
		body = append(body, strings.Split(line, "\n")...)
	}
	rows = max(1, rows)
	if len(body) <= rows {
		d.authScroll, d.authScrollMax, d.authFollowPaste = 0, 0, false
		return body
	}
	// Reserve a compact, always-visible hint. On a one-row viewport, keep
	// the content accessible rather than replacing it with the hint.
	hint := tui.WrapANSILine("up/down scroll", max(1, width))
	hintRows := min(len(hint), rows-1)
	contentRows := rows - hintRows
	d.authScrollMax = len(body) - contentRows
	if d.authFollowPaste && d.pasteRow >= 0 {
		d.authScroll = d.pasteRow - contentRows + 1
	}
	d.authFollowPaste = false
	d.authScroll = min(max(0, d.authScroll), d.authScrollMax)
	start := d.authScroll
	if d.pasteRow >= start && d.pasteRow < start+contentRows {
		d.pasteRow -= start
	} else {
		d.pasteRow = -1
	}
	out := append([]string(nil), body[start:start+contentRows]...)
	if hintRows == 1 {
		hint = tui.WrapANSILine(fmt.Sprintf("up/down scroll (%d-%d/%d)", start+1, start+contentRows, len(body)), max(1, width))
	}
	for _, row := range hint[:hintRows] {
		out = append(out, th.FG256(th.Muted, row))
	}
	return out
}
