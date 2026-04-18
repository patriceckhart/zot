package modes

import (
	"strings"

	"github.com/patriceckhart/zot/internal/tui"
)

// frameHeader returns a full-width rule with a small title at the left.
// Matches the thin rule style used for code blocks and tool results so
// every dialog in the TUI looks the same.
//
//	─── title ────────────────────────────────
func frameHeader(th tui.Theme, title string, width int) string {
	label := "── " + title + " "
	if width <= 0 {
		return th.FG256(th.Muted, label)
	}
	padLen := width - len(label)
	if padLen < 0 {
		padLen = 0
	}
	return th.FG256(th.Muted, label+strings.Repeat("─", padLen))
}

// frameRule returns a full-width horizontal rule in the muted color.
func frameRule(th tui.Theme, width int) string {
	if width <= 0 {
		width = 1
	}
	return th.FG256(th.Muted, strings.Repeat("─", width))
}
