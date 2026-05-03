// Package tui is a small terminal UI: raw-mode input, differential
// line renderer, multi-line editor, and chat view. No external TUI
// framework; just ANSI escape codes.
package tui

import "strings"

// ANSI 256-color palette used by zot. Defined as numeric codes so we
// can swap themes without changing any render code.
type Theme struct {
	FG           int
	Muted        int
	Accent       int
	User         int // label color for the user role
	UserBubbleBG int // background tint behind user message rows
	UserBubbleFG int // foreground colour for user message rows
	Assistant    int // label color for the zot role
	Tool         int
	ToolOut      int
	Error        int
	Warning      int
	Spinner      int // spinner + funny working line
	SelectionBG  int // background for highlighted rows
	SelectionFG  int // foreground for highlighted rows
}

var Dark = Theme{
	FG:           253,
	Muted:        244,
	Accent:       111, // soft blue
	User:         180, // warm tan (unused now that the speaker label is gone, kept for skin compat)
	UserBubbleBG: 237, // soft mid-dark grey panel behind user rows
	UserBubbleFG: 246, // matches Theme.Muted (status bar text colour)
	Assistant:    117, // bright cyan — the zot label color
	Tool:         114, // green
	ToolOut:      245,
	Error:        203,
	Warning:      214,
	Spinner:      183, // soft purple
	SelectionBG:  24,  // deep blue background
	SelectionFG:  231, // near-white foreground
}

var Light = Theme{
	FG:           236,
	Muted:        244,
	Accent:       33,
	User:         94,
	UserBubbleBG: 254, // very pale grey panel behind user rows on light theme
	UserBubbleFG: 240, // dark grey text, legible on the pale panel
	Assistant:    31,  // deep cyan
	Tool:         28,
	ToolOut:      240,
	Error:        160,
	Warning:      166,
	Spinner:      91,  // purple
	SelectionBG:  153, // light blue
	SelectionFG:  232, // near-black
}

// FG256 wraps s in foreground color c using ANSI 256-color SGR.
func (t Theme) FG256(c int, s string) string {
	return sgrFG(c) + s + reset
}

// BG256 wraps s in background color c using ANSI 256-color SGR.
// Useful when the visible cell needs a coloured fill but the
// underlying character should be a regular space (so mouse-copy
// from the terminal yields whitespace instead of a glyph).
func (t Theme) BG256(c int, s string) string {
	return sgrBG(c) + s + reset
}

// AccentBar returns a 2-cell-wide leader: a coloured half-block
// glyph followed by a plain space gutter. Used as the speaker-label
// prefix in the chat ("▌ you", "▌ zot") and as the editor prompt so
// the bar reads consistently across the UI.
func (t Theme) AccentBar(c int) string {
	return t.FG256(c, "▌ ")
}

// Highlight paints s with the theme's selection colors (foreground +
// background). The caller is responsible for padding s to the desired
// width; styling alone does not extend the background past content.
func (t Theme) Highlight(s string) string {
	return sgrFG(t.SelectionFG) + sgrBG(t.SelectionBG) + s + reset
}

// PadHighlight styles s and extends the selection background to the
// full terminal width so the highlight is a full row, not just a
// rectangle around the text.
func (t Theme) PadHighlight(s string, width int) string {
	visible := visibleWidth(s)
	if visible < width {
		s += strings.Repeat(" ", width-visible)
	}
	return sgrFG(t.SelectionFG) + sgrBG(t.SelectionBG) + s + reset
}

// UserBubble paints a single user message row with the bubble
// background colour, padding to width so the tint extends to the
// full terminal width. Foreground stays in UserBubbleFG so text
// remains legible against the tint.
func (t Theme) UserBubble(s string, width int) string {
	visible := visibleWidth(s)
	if visible < width {
		s += strings.Repeat(" ", width-visible)
	}
	return sgrFG(t.UserBubbleFG) + sgrBG(t.UserBubbleBG) + s + reset
}

// UserBubbleRow renders one user-bubble row prefixed with a coloured
// half-block accent bar ("▌ ") so every line of the bubble has the
// zot-blue gutter at the very left. The bar lives outside the bubble
// tint (chat bg) so the bubble itself sits inside it. Width is the
// outer width including the bar; the bubble content is padded to
// width-2 (the bar + its trailing space).
func (t Theme) UserBubbleRow(content string, width int) string {
	// Bar plus a single space gutter, in the assistant accent colour
	// so it matches the tool-box / app accent and reads as zot's voice
	// marker. Two cells wide.
	bar := t.FG256(t.Assistant, "▌ ")
	bubbleW := width - 2
	if bubbleW < 1 {
		bubbleW = 1
	}
	return bar + t.UserBubble(content, bubbleW)
}

// Bold wraps s in bold SGR.
func Bold(s string) string { return "\x1b[1m" + s + "\x1b[22m" }

// Dim wraps s in dim SGR.
func Dim(s string) string { return "\x1b[2m" + s + "\x1b[22m" }

// Italic wraps s in italic SGR.
func Italic(s string) string { return "\x1b[3m" + s + "\x1b[23m" }

const reset = "\x1b[0m"

func sgrFG(c int) string { return "\x1b[38;5;" + itoa(c) + "m" }
func sgrBG(c int) string { return "\x1b[48;5;" + itoa(c) + "m" }

// small itoa to avoid pulling strconv into this hot path twice.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [4]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
