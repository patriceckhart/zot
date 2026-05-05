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

	// Main-screen flow renderer state. logLines is the full logical
	// buffer (chat + live bottom band) from the previous DrawLog call.
	// logViewportTop/logHardwareRow track where that logical buffer sits
	// in the terminal's visible viewport so we can diff safely, and bail
	// out to clear+replay when the diff would touch rows that are no
	// longer addressable.
	logChat        []string
	logBottom      []string
	logLines       []string
	logViewportTop int
	logHardwareRow int
	logInit        bool
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
		r.logLines = nil
		r.logViewportTop = 0
		r.logHardwareRow = 0
		r.logInit = false
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
	r.logLines = nil
	r.logViewportTop = 0
	r.logHardwareRow = 0
	r.logInit = false
	_, _ = io.WriteString(r.out, SeqDeleteKittyImages+SeqClearScreen+SeqClearScrollback+MoveTo(1, 1))
}

// Invalidate forces a full repaint on the next Draw without clearing the
// whole terminal first. Useful when the cached diff is unreliable but a
// visible full-screen flash would be too distracting.
func (r *Renderer) Invalidate() {
	r.prev = nil
	r.logLines = nil
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

	// Always reserve one real row below the editor/status band. This is
	// renderer-owned (not a best-effort trailing blank in the caller's
	// bottom block), so the logical-buffer diff keeps it visible and cursor
	// placement remains relative to the editor itself.
	const bottomMarginRows = 1
	lines := make([]string, 0, len(chatFrame)+len(bottomFrame)+bottomMarginRows)
	lines = append(lines, chatFrame...)
	lines = append(lines, bottomFrame...)
	for range bottomMarginRows {
		lines = append(lines, "")
	}
	if len(lines) == 0 {
		lines = []string{""}
	}

	cursorTargetRow := -1
	if cursorBottomRow >= 0 && cursorBottomRow < len(bottomFrame) {
		cursorTargetRow = len(chatFrame) + cursorBottomRow
	}

	var w strings.Builder
	w.WriteString(SeqSynchronizedOn)
	w.WriteString(SeqHideCursor)

	writeFull := func(clear bool) {
		if clear {
			w.WriteString(SeqDeleteKittyImages)
			w.WriteString(SeqClearScreen)
			w.WriteString(SeqClearScrollback)
			w.WriteString(MoveTo(1, 1))
		}
		for idx, line := range lines {
			if idx > 0 {
				w.WriteString("\r\n")
			}
			w.WriteString("\x1b[0m")
			w.WriteString(SeqClearLine)
			w.WriteString(line)
		}
		r.logHardwareRow = len(lines) - 1
		r.logViewportTop = len(lines) - r.rows
		if r.logViewportTop < 0 {
			r.logViewportTop = 0
		}
	}

	moveToLogicalRow := func(targetRow int) {
		if targetRow < 0 {
			targetRow = 0
		}
		if targetRow >= len(lines) {
			targetRow = len(lines) - 1
		}
		viewportBottom := r.logViewportTop + r.rows - 1
		if targetRow > viewportBottom {
			currentScreenRow := r.logHardwareRow - r.logViewportTop
			if currentScreenRow < 0 {
				currentScreenRow = 0
			}
			if currentScreenRow >= r.rows {
				currentScreenRow = r.rows - 1
			}
			moveToBottom := r.rows - 1 - currentScreenRow
			if moveToBottom > 0 {
				w.WriteString("\x1b[" + itoa(moveToBottom) + "B")
			}
			scroll := targetRow - viewportBottom
			for s := 0; s < scroll; s++ {
				w.WriteString("\r\n")
			}
			r.logViewportTop += scroll
			r.logHardwareRow = targetRow
			return
		}
		currentScreenRow := r.logHardwareRow - r.logViewportTop
		targetScreenRow := targetRow - r.logViewportTop
		lineDiff := targetScreenRow - currentScreenRow
		if lineDiff > 0 {
			w.WriteString("\x1b[" + itoa(lineDiff) + "B")
		} else if lineDiff < 0 {
			w.WriteString("\x1b[" + itoa(-lineDiff) + "A")
		}
		r.logHardwareRow = targetRow
	}

	positionCursor := func() {
		if cursorTargetRow < 0 || cursorTargetRow >= len(lines) {
			return
		}
		moveToLogicalRow(cursorTargetRow)
		w.WriteString("\r")
		if cursorCol > 0 {
			w.WriteString("\x1b[" + itoa(cursorCol) + "C")
		}
		w.WriteString(SeqShowCursor)
	}

	full := !r.logInit || len(r.logLines) == 0
	if full {
		writeFull(true)
		r.logInit = true
	} else {
		firstChanged := -1
		lastChanged := -1
		maxLines := len(lines)
		if len(r.logLines) > maxLines {
			maxLines = len(r.logLines)
		}
		for idx := 0; idx < maxLines; idx++ {
			oldLine := ""
			if idx < len(r.logLines) {
				oldLine = r.logLines[idx]
			}
			newLine := ""
			if idx < len(lines) {
				newLine = lines[idx]
			}
			if oldLine != newLine {
				if firstChanged == -1 {
					firstChanged = idx
				}
				lastChanged = idx
			}
		}

		if firstChanged == -1 {
			// No content changes; the final cursor positioning below may still
			// move the hardware cursor if the editor cursor changed.
		} else if len(lines) < len(r.logLines) || firstChanged < r.logViewportTop {
			// Shrinks and changes above the visible viewport cannot be safely
			// patched in terminal scrollback. Replay the current logical buffer
			// instead of risking stale duplicated rows.
			writeFull(true)
		} else {
			prevViewportTop := r.logViewportTop
			viewportTop := prevViewportTop
			hardwareRow := r.logHardwareRow
			prevViewportBottom := prevViewportTop + r.rows - 1
			appendStart := len(lines) > len(r.logLines) && firstChanged == len(r.logLines) && firstChanged > 0
			moveTarget := firstChanged
			if appendStart {
				moveTarget = firstChanged - 1
			}

			if moveTarget > prevViewportBottom {
				currentScreenRow := hardwareRow - prevViewportTop
				if currentScreenRow < 0 {
					currentScreenRow = 0
				}
				if currentScreenRow >= r.rows {
					currentScreenRow = r.rows - 1
				}
				moveToBottom := r.rows - 1 - currentScreenRow
				if moveToBottom > 0 {
					w.WriteString("\x1b[" + itoa(moveToBottom) + "B")
				}
				scroll := moveTarget - prevViewportBottom
				for s := 0; s < scroll; s++ {
					w.WriteString("\r\n")
				}
				prevViewportTop += scroll
				viewportTop += scroll
				hardwareRow = moveTarget
			}

			currentScreenRow := hardwareRow - prevViewportTop
			targetScreenRow := moveTarget - viewportTop
			lineDiff := targetScreenRow - currentScreenRow
			if lineDiff > 0 {
				w.WriteString("\x1b[" + itoa(lineDiff) + "B")
			} else if lineDiff < 0 {
				w.WriteString("\x1b[" + itoa(-lineDiff) + "A")
			}
			if appendStart {
				w.WriteString("\r\n")
			} else {
				w.WriteString("\r")
			}

			renderEnd := lastChanged
			if renderEnd >= len(lines) {
				renderEnd = len(lines) - 1
			}
			for idx := firstChanged; idx <= renderEnd; idx++ {
				if idx > firstChanged {
					w.WriteString("\r\n")
				}
				w.WriteString("\x1b[0m")
				w.WriteString(SeqClearLine)
				w.WriteString(lines[idx])
			}
			r.logHardwareRow = renderEnd
			r.logViewportTop = viewportTop
			if minTop := r.logHardwareRow - r.rows + 1; minTop > r.logViewportTop {
				r.logViewportTop = minTop
			}
			if r.logViewportTop < 0 {
				r.logViewportTop = 0
			}
		}
	}

	// Re-anchor the terminal viewport at the true end of the logical
	// buffer on every frame. During busy turns most redraws only mutate
	// spinner/live rows above the editor; without touching the final margin
	// row, the terminal can visually lose the bottom padding even though it
	// still exists in r.logLines.
	if len(lines) > 0 {
		moveToLogicalRow(len(lines) - 1)
		w.WriteString("\r")
		w.WriteString("\x1b[0m")
		w.WriteString(SeqClearLine)
	}
	positionCursor()
	w.WriteString(SeqSynchronizedOff)
	_, _ = io.WriteString(r.out, w.String())

	r.logChat = append(r.logChat[:0], chatFrame...)
	r.logBottom = append(r.logBottom[:0], bottomFrame...)
	r.logLines = append(r.logLines[:0], lines...)
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
