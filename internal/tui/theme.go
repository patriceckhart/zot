// Package tui is a small terminal UI: raw-mode input, differential
// line renderer, multi-line editor, and chat view. No external TUI
// framework; just ANSI escape codes.
package tui

import "strings"

// ANSI 256-color palette used by zot. Defined as numeric codes so we
// can swap themes without changing any render code.
type Theme struct {
	FG          int
	Muted       int
	Accent      int
	User        int // label color for the user role
	Assistant   int // label color for the zot role
	Tool        int
	ToolOut     int
	Error       int
	Warning     int
	Spinner     int // spinner + funny working line
	SelectionBG int // background for highlighted rows
	SelectionFG int // foreground for highlighted rows
}

var Dark = Theme{
	FG:          253,
	Muted:       244,
	Accent:      111, // soft blue
	User:        180, // warm tan
	Assistant:   117, // bright cyan — the zot label color
	Tool:        114, // green
	ToolOut:     245,
	Error:       203,
	Warning:     214,
	Spinner:     183, // soft purple
	SelectionBG: 24,  // deep blue background
	SelectionFG: 231, // near-white foreground
}

var Light = Theme{
	FG:          236,
	Muted:       244,
	Accent:      33,
	User:        94,
	Assistant:   31, // deep cyan
	Tool:        28,
	ToolOut:     240,
	Error:       160,
	Warning:     166,
	Spinner:     91,  // purple
	SelectionBG: 153, // light blue
	SelectionFG: 232, // near-black
}

// FG256 wraps s in foreground color c using ANSI 256-color SGR.
func (t Theme) FG256(c int, s string) string {
	return sgrFG(c) + s + reset
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
	// Emit the background color AFTER the reset via a trailing
	// erase-to-end-of-line so the highlight extends to the terminal
	// edge even if the cell count is slightly off.
	return sgrFG(t.SelectionFG) + sgrBG(t.SelectionBG) + s + "\x1b[K" + reset
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
