package tui

import (
	"io"
	"strings"

	"github.com/mattn/go-runewidth"
)

// runewidthRune reports the number of cells a rune occupies, pinned
// here so the renderer does not depend on the editor's helper.
func runewidthRune(r rune) int { return runewidth.RuneWidth(r) }

// Renderer maintains a previous frame and writes only the lines that
// changed on each Draw(). Callers pass a full target frame (slice of
// styled lines, already wrapped to width).
type Renderer struct {
	out  io.Writer
	prev []string
	rows int // terminal rows
	cols int // terminal cols

	// Cursor position after last draw (for placing input cursor).
	cursorRow int
	cursorCol int

	// hideCursor when true prevents ShowCursor from being emitted.
	hideCursor bool

	// prevHadImage tracks whether the previous frame contained an
	// inline-image escape so we can force a full clear+repaint whenever
	// the image set changes. Only matters when inline images are
	// enabled via ZOT_INLINE_IMAGES; defaults to false.
	prevHadImage bool
}

// NewRenderer returns a renderer that writes to out.
func NewRenderer(out io.Writer) *Renderer {
	return &Renderer{out: out}
}

// Resize tells the renderer the current terminal size.
func (r *Renderer) Resize(cols, rows int) {
	if cols != r.cols || rows != r.rows {
		r.cols = cols
		r.rows = rows
		r.prev = nil
	}
}

// Clear forces a full repaint on the next Draw and clears the screen.
func (r *Renderer) Clear() {
	r.prev = nil
	_, _ = io.WriteString(r.out, SeqClearScreen)
}

// Draw updates the terminal so that the visible frame ends with the
// given lines (bottom-aligned). cursorRow/cursorCol are offsets within
// the lines slice indicating where to place the terminal cursor; use
// -1 to hide it.
// containsImageEscape reports whether the line carries an inline-image
// escape we must repaint rather than diff against the previous frame.
func containsImageEscape(s string) bool {
	return strings.Contains(s, "\x1b]1337;File=") || strings.Contains(s, "\x1b_G")
}

// truncateToWidth clips s so its on-screen width doesn't exceed cols
// cells, preserving ANSI CSI escape sequences (which don't consume
// cells). Lines carrying an inline-image escape are returned as-is
// since we can't measure their painted size.
func truncateToWidth(s string, cols int) string {
	if cols <= 0 || containsImageEscape(s) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	seen := 0
	runes := []rune(s)
	for i := 0; i < len(runes); {
		r := runes[i]
		// CSI escape sequence (ESC [ ... final): zero-width.
		if r == 0x1b && i+1 < len(runes) && runes[i+1] == '[' {
			out.WriteRune(r)
			out.WriteRune(runes[i+1])
			i += 2
			for i < len(runes) {
				c := runes[i]
				out.WriteRune(c)
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			continue
		}
		rw := runewidthRune(r)
		if seen+rw > cols {
			break
		}
		out.WriteRune(r)
		seen += rw
		i++
	}
	return out.String()
}

func (r *Renderer) Draw(lines []string, cursorRow, cursorCol int) {
	if r.cols == 0 || r.rows == 0 {
		return
	}
	// Bottom-align: only the last r.rows lines are visible.
	visible := lines
	if len(visible) > r.rows {
		visible = visible[len(visible)-r.rows:]
		cursorRow -= len(lines) - len(visible)
	}
	// Pad to r.rows with empty lines at the top. Every line is also
	// hard-truncated to cols so the terminal never soft-wraps our output
	// (which would push the status bar out of its row).
	frame := make([]string, r.rows)
	top := r.rows - len(visible)
	for i := 0; i < top; i++ {
		frame[i] = ""
	}
	for i, line := range visible {
		frame[top+i] = truncateToWidth(line, r.cols)
	}

	var w strings.Builder
	w.WriteString(SeqSynchronizedOn)
	w.WriteString(SeqHideCursor)

	// When inline images are in play we always full-repaint (clear
	// screen first, then rewrite every row). Terminals manage image
	// pixels in a layer we cannot diff against, so the per-line cache
	// is unreliable. Inline images are opt-in via ZOT_INLINE_IMAGES;
	// the common code path below is the fast cached diff.
	curHasImage := false
	for _, l := range frame {
		if containsImageEscape(l) {
			curHasImage = true
			break
		}
	}
	forceAll := curHasImage || r.prevHadImage
	if forceAll {
		w.WriteString(SeqClearScreen)
	}

	full := r.prev == nil || len(r.prev) != r.rows
	for i := 0; i < r.rows; i++ {
		if full || forceAll || r.prev[i] != frame[i] {
			w.WriteString(MoveTo(i+1, 1))
			w.WriteString(SeqClearLine)
			w.WriteString(frame[i])
		}
	}

	if cursorRow >= 0 {
		absRow := top + cursorRow + 1
		absCol := cursorCol + 1
		if absRow >= 1 && absRow <= r.rows {
			w.WriteString(MoveTo(absRow, absCol))
			w.WriteString(SeqShowCursor)
		}
	}
	w.WriteString(SeqSynchronizedOff)

	_, _ = io.WriteString(r.out, w.String())

	r.prev = frame
	r.prevHadImage = curHasImage
	r.cursorRow = cursorRow
	r.cursorCol = cursorCol
}
