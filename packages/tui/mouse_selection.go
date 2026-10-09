package tui

import (
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

type mouseCell struct {
	row, col int
}

type mouseSelection struct {
	frame      []string
	start, end mouseCell
	dragged    bool
	released   bool
}

// BeginMouseSelection snapshots the visible screen. Until selection is cleared,
// DrawLog and DrawHistory defer painting so streaming cannot move selected text.
// Coordinates are zero-based terminal cells.
func (r *Renderer) BeginMouseSelection(row, col int) {
	r.ClearMouseSelection()
	if row < 0 || row >= r.rows || col < 0 || col >= r.cols {
		return
	}
	frame := make([]string, r.rows)
	if r.history != nil {
		copy(frame, r.history.prev)
	} else {
		for i := range frame {
			logical := r.logViewportTop + i
			if logical < len(r.logLines) {
				frame[i] = r.logLines[logical]
			}
		}
	}
	r.mouseSelection = &mouseSelection{frame: frame, start: mouseCell{row, col}, end: mouseCell{row, col}}
}

// DragMouseSelection highlights the range between the initial press and this
// cell. The end cell is exclusive, as with a text cursor selection.
func (r *Renderer) DragMouseSelection(row, col int) {
	s := r.mouseSelection
	if s == nil || s.released {
		return
	}
	row = min(max(row, 0), r.rows-1)
	col = min(max(col, 0), r.cols)
	s.end = mouseCell{row, col}
	s.dragged = s.dragged || s.end != s.start
	if s.dragged {
		r.paintMouseSelection(true)
	}
}

// EndMouseSelection finishes a drag without clearing its highlight or copying.
// A stationary click clears the snapshot and returns no text.
func (r *Renderer) EndMouseSelection(row, col int) (text string, dragged bool) {
	if r.mouseSelection == nil || r.mouseSelection.released {
		return "", false
	}
	r.DragMouseSelection(row, col)
	s := r.mouseSelection
	if !s.dragged {
		r.ClearMouseSelection()
		return "", false
	}
	s.released = true
	text, _ = r.MouseSelectionText()
	return text, true
}

// MouseSelectionText returns the finished selection for an explicit copy action.
// It excludes ANSI controls and trailing row padding.
func (r *Renderer) MouseSelectionText() (string, bool) {
	s := r.mouseSelection
	if s == nil || !s.released || !s.dragged {
		return "", false
	}
	var lines []string
	start, end := s.bounds()
	for row := start.row; row <= end.row; row++ {
		lo, hi := 0, r.cols
		if row == start.row {
			lo = start.col
		}
		if row == end.row {
			hi = end.col
		}
		if containsImageEscape(s.frame[row]) {
			lines = append(lines, "")
			continue
		}
		_, plain := selectionLine(s.frame[row], lo, hi, r.theme)
		lines = append(lines, strings.TrimRight(plain, " \t"))
	}
	return strings.Join(lines, "\n"), true
}

// MouseSelectionActive reports whether a drag or finished selection is retained.
func (r *Renderer) MouseSelectionActive() bool {
	return r.mouseSelection != nil
}

// MouseSelectionDragging reports whether the left button is still held.
func (r *Renderer) MouseSelectionDragging() bool {
	return r.mouseSelection != nil && !r.mouseSelection.released
}

// ClearMouseSelection cancels a drag without copying, for typing, scrolling,
// mode changes, and resize. It restores the snapshot without clearing scrollback.
func (r *Renderer) ClearMouseSelection() {
	if r.mouseSelection != nil && r.mouseSelection.dragged {
		r.paintMouseSelection(false)
	}
	r.mouseSelection = nil
}

func (s *mouseSelection) bounds() (mouseCell, mouseCell) {
	if s.start.row > s.end.row || (s.start.row == s.end.row && s.start.col > s.end.col) {
		return s.end, s.start
	}
	return s.start, s.end
}

func (r *Renderer) paintMouseSelection(highlight bool) {
	s := r.mouseSelection
	start, end := s.bounds()
	var out strings.Builder
	out.WriteString(SeqSynchronizedOn + SeqSaveCursor)
	for row, line := range s.frame {
		if containsImageEscape(line) {
			continue
		}
		if highlight && row >= start.row && row <= end.row {
			lo, hi := 0, r.cols
			if row == start.row {
				lo = start.col
			}
			if row == end.row {
				hi = end.col
			}
			line, _ = selectionLine(line, lo, hi, r.theme)
		}
		out.WriteString(MoveTo(row+1, 1) + reset + SeqClearLine + line + reset)
	}
	out.WriteString(SeqRestoreCursor + SeqSynchronizedOff)
	_, _ = io.WriteString(r.out, out.String())
}

// selectionLine measures cells, not bytes, and keeps styling outside the range.
// Escape payloads (including hyperlinks) never enter the copied plain text.
func selectionLine(line string, lo, hi int, theme Theme) (string, string) {
	var styled, plain, styles, visible strings.Builder
	col := 0
	previousSelected := false
	for i := 0; i < len(line); {
		if line[i] == '\x1b' {
			end := skipEscapeSequence(line, i)
			seq := line[i:end]
			styled.WriteString(seq)
			if strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") {
				if seq == reset || seq == "\x1b[m" {
					styles.Reset()
				} else {
					styles.WriteString(seq)
				}
			}
			i = end
			continue
		}
		ch, size := utf8.DecodeRuneInString(line[i:])
		i += size
		if ch < 0x20 || ch == 0x7f {
			continue
		}
		visible.WriteRune(ch)
		// Measuring the accumulated text keeps combining and joined Unicode
		// sequences attached to the preceding cell.
		width := runewidth.StringWidth(visible.String()) - col
		selected := col < hi && col+width > lo
		if width == 0 {
			selected = previousSelected
		}
		if selected {
			styled.WriteString(theme.SelectionStyle())
			styled.WriteRune(ch)
			styled.WriteString(reset + styles.String())
			plain.WriteRune(ch)
		} else {
			styled.WriteRune(ch)
		}
		previousSelected = selected
		col += width
	}
	return styled.String(), plain.String()
}
