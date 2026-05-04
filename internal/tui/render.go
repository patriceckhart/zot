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

	// Main-screen flow renderer state. logChat is the full chat buffer
	// already emitted into terminal scrollback. logBottom is the
	// editable/status block currently drawn after the chat.
	logChat    []string
	logBottom  []string
	logInit    bool
	logCursorR int
}

// NewRenderer returns a renderer that writes to out.
func NewRenderer(out io.Writer) *Renderer {
	return &Renderer{out: out}
}

// Resize tells the renderer the current terminal size.
//
// On a real size change we also issue a clear-screen so the next Draw
// starts from a blank slate. Without the clear, characters from the
// old (wider) layout linger past the new right edge and rows from
// before the new bottom hang around as garbage.
func (r *Renderer) Resize(cols, rows int) {
	if cols != r.cols || rows != r.rows {
		r.cols = cols
		r.rows = rows
		r.prev = nil
		r.logChat = nil
		r.logBottom = nil
		r.logInit = false
		r.logCursorR = 0
		if r.out != nil {
			_, _ = io.WriteString(r.out, SeqClearScreen)
		}
	}
}

// Clear forces a full repaint on the next Draw and clears the screen
// plus scrollback. In main-screen flow mode this is required whenever
// already-emitted transcript layout changes (for example ctrl+o
// expand/collapse), because terminal scrollback cannot be edited
// reliably once printed.
func (r *Renderer) Clear() {
	r.prev = nil
	r.logChat = nil
	r.logBottom = nil
	r.logInit = false
	r.logCursorR = 0
	_, _ = io.WriteString(r.out, SeqDeleteKittyImages+SeqClearScreen+SeqClearScrollback+MoveTo(1, 1))
}

// Invalidate forces a full repaint on the next Draw without clearing the
// whole terminal first. Useful when the cached diff is unreliable but a
// visible full-screen flash would be too distracting.
func (r *Renderer) Invalidate() {
	r.prev = nil
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
//
// Fast path: a byte-length <= cols is a conservative upper bound
// guaranteeing the cell width is also <= cols, so we skip all the
// rune-width math. That covers the vast majority of lines in a
// transcript (narrow terminals wrap early; wide ones leave headroom).
func truncateToWidth(s string, cols int) string {
	if cols <= 0 || containsImageEscape(s) {
		return s
	}
	if len(s) <= cols {
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
			// Flush any trailing ANSI escapes (resets, erase-to-EOL)
			// so background colors and cleanup sequences survive.
			for i < len(runes) {
				if runes[i] == 0x1b && i+1 < len(runes) && runes[i+1] == '[' {
					out.WriteRune(runes[i])
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
				} else {
					break
				}
			}
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
	curHasKittyImage := false
	for _, l := range frame {
		if containsImageEscape(l) {
			curHasImage = true
			if strings.Contains(l, "\x1b_G") {
				curHasKittyImage = true
			}
		}
	}
	forceAll := curHasImage || r.prevHadImage
	if forceAll {
		w.WriteString(SeqClearScreen)
		if curHasKittyImage {
			// Delete previously placed kitty images once per frame,
			// before rewriting all rows. Doing this inside each image
			// escape makes only the last image in the frame survive.
			w.WriteString("\x1b_Ga=d\x1b\\")
		}
	}

	// Detect selection highlights: if the current OR previous frame
	// has selection-background rows, force full repaint. VS Code's
	// terminal doesn't reliably clear background colors on row
	// overwrites, leaving ghost highlights behind.
	hasSelection := false
	for _, l := range frame {
		if strings.Contains(l, "\x1b[48;5;") {
			hasSelection = true
			break
		}
	}
	if !hasSelection && r.prev != nil {
		for _, l := range r.prev {
			if strings.Contains(l, "\x1b[48;5;") {
				hasSelection = true
				break
			}
		}
	}

	full := r.prev == nil || len(r.prev) != r.rows
	for i := 0; i < r.rows; i++ {
		if full || forceAll || hasSelection || r.prev[i] != frame[i] {
			w.WriteString(MoveTo(i+1, 1))
			w.WriteString("\x1b[0m") // reset all attributes first
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

// DrawLog renders zot in the terminal's main screen as normal terminal
// flow rather than a fixed full-screen frame. Chat lines are emitted once
// into the host terminal scrollback; the current bottom block (dialogs,
// slash popup, status, editor) is erased and redrawn in place at the end.
//
// cursorBottomRow/cursorCol are offsets into bottom, not the full frame.
func (r *Renderer) DrawLog(chat, bottom []string, cursorBottomRow, cursorCol int) {
	if r.cols == 0 || r.rows == 0 {
		return
	}
	if len(bottom) == 0 {
		bottom = []string{""}
	}
	chatFrame := make([]string, len(chat))
	for i, line := range chat {
		chatFrame[i] = truncateToWidth(line, r.cols)
	}
	bottomFrame := make([]string, len(bottom))
	for i, line := range bottom {
		bottomFrame[i] = truncateToWidth(line, r.cols)
	}

	var w strings.Builder
	w.WriteString(SeqSynchronizedOn)
	w.WriteString(SeqHideCursor)

	if !r.logInit {
		// First paint: start at top-left and print the transcript followed
		// immediately by the live input/status block. No bottom padding,
		// no footer layout.
		w.WriteString(SeqClearScreen)
		w.WriteString(SeqClearScrollback)
		w.WriteString(MoveTo(1, 1))
		for _, line := range chatFrame {
			w.WriteString("\x1b[0m")
			w.WriteString(SeqClearLine)
			w.WriteString(line)
			w.WriteString("\r\n")
		}
		writeBlock(&w, bottomFrame)
		r.logInit = true
	} else {
		// Move from the currently exposed cursor back to the top of the
		// live bottom block, then erase the old block. We track the row
		// inside the bottom block where we left the cursor last Draw.
		w.WriteString("\r")
		if r.logCursorR > 0 {
			w.WriteString("\x1b[" + itoa(r.logCursorR) + "A")
		}
		eraseRows(&w, len(r.logBottom))

		prefix := len(r.logChat) <= len(chatFrame)
		if prefix {
			for i := range r.logChat {
				if r.logChat[i] != chatFrame[i] {
					prefix = false
					break
				}
			}
		}
		if prefix {
			// Append only genuinely new chat rows. They become real terminal
			// scrollback, and inline image escapes are emitted once here — not
			// on every keystroke.
			for _, line := range chatFrame[len(r.logChat):] {
				w.WriteString("\x1b[0m")
				w.WriteString(SeqClearLine)
				w.WriteString(line)
				w.WriteString("\r\n")
			}
		} else {
			// Transcript changed in place (streaming text grows within the
			// last assistant block, markdown finalises/reflows, ctrl+o
			// expand/collapse, /clear, session load). In main-screen flow
			// mode old chat lives in terminal scrollback; trying to move up
			// and rewrite it is unreliable, especially once images or native
			// scrolling are involved. Use the safe path: clear visible screen
			// + scrollback, then replay the current transcript once.
			w.WriteString(SeqDeleteKittyImages)
			w.WriteString(SeqClearScreen)
			w.WriteString(SeqClearScrollback)
			w.WriteString(MoveTo(1, 1))
			for _, line := range chatFrame {
				w.WriteString("\x1b[0m")
				w.WriteString(SeqClearLine)
				w.WriteString(line)
				w.WriteString("\r\n")
			}
		}
		writeBlock(&w, bottomFrame)
	}

	// writeBlock leaves the cursor on the last bottom row. Move to the
	// requested cursor position inside that block.
	if cursorBottomRow >= 0 && cursorBottomRow < len(bottomFrame) {
		up := (len(bottomFrame) - 1) - cursorBottomRow
		if up > 0 {
			w.WriteString("\x1b[" + itoa(up) + "A")
		}
		w.WriteString("\r")
		if cursorCol > 0 {
			w.WriteString("\x1b[" + itoa(cursorCol) + "C")
		}
		w.WriteString(SeqShowCursor)
		r.logCursorR = cursorBottomRow
	} else {
		r.logCursorR = len(bottomFrame) - 1
	}

	w.WriteString(SeqSynchronizedOff)
	_, _ = io.WriteString(r.out, w.String())

	r.logChat = append(r.logChat[:0], chatFrame...)
	r.logBottom = append(r.logBottom[:0], bottomFrame...)
	r.cursorRow = cursorBottomRow
	r.cursorCol = cursorCol
}

func writeBlock(w *strings.Builder, lines []string) {
	for i, line := range lines {
		w.WriteString("\x1b[0m")
		w.WriteString(SeqClearLine)
		w.WriteString(line)
		if i < len(lines)-1 {
			w.WriteString("\r\n")
		}
	}
}

func eraseRows(w *strings.Builder, n int) {
	if n <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		w.WriteString("\x1b[0m")
		w.WriteString(SeqClearLine)
		if i < n-1 {
			w.WriteString("\r\n")
		}
	}
	if n > 1 {
		w.WriteString("\x1b[" + itoa(n-1) + "A")
	}
	w.WriteString("\r")
}

func tailTruncated(lines []string, maxRows, cols int) []string {
	if maxRows <= 0 {
		return nil
	}
	if len(lines) > maxRows {
		lines = lines[len(lines)-maxRows:]
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = truncateToWidth(line, cols)
	}
	return out
}
