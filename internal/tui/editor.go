package tui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// Editor is a simple multi-line text editor for the input area.
//
// Users can type, paste, move the cursor, and submit. The editor
// exposes its rendered height and the current cursor row/col for the
// outer renderer. It does NOT draw itself directly; instead, Render()
// returns the visible lines.
type Editor struct {
	// Lines is the current buffer, one entry per line.
	Lines    []string
	CursorR  int // row index into Lines
	CursorC  int // rune index into Lines[CursorR]
	Prompt   string
	MaxWidth int

	// History is a ring of previously submitted buffers (newest last).
	History    []string
	histIdx    int // -1 means "editing current buffer"
	savedDraft string
}

// NewEditor returns an empty editor with the given prompt.
func NewEditor(prompt string) *Editor {
	return &Editor{
		Lines:   []string{""},
		Prompt:  prompt,
		histIdx: -1,
	}
}

// Value returns the buffer as a single string.
func (e *Editor) Value() string { return strings.Join(e.Lines, "\n") }

// SetValue replaces the buffer and places the cursor at the end.
func (e *Editor) SetValue(s string) {
	e.Lines = strings.Split(s, "\n")
	if len(e.Lines) == 0 {
		e.Lines = []string{""}
	}
	e.CursorR = len(e.Lines) - 1
	e.CursorC = runeLen(e.Lines[e.CursorR])
	e.histIdx = -1
}

// Clear resets the buffer.
func (e *Editor) Clear() { e.SetValue("") }

// IsEmpty reports whether the buffer has no visible content.
func (e *Editor) IsEmpty() bool {
	return len(e.Lines) == 1 && e.Lines[0] == ""
}

// HandleKey applies k to the editor. It returns (submit, key).
// If submit is true, the caller should read Value() and call
// PushHistory + Clear.
func (e *Editor) HandleKey(k Key) (submit bool) {
	switch k.Kind {
	case KeyRune:
		if k.Alt && (k.Rune == '\r' || k.Rune == '\n') {
			e.newline()
			return false
		}
		e.insert(string(k.Rune))
	case KeyEnter:
		// Shift+Enter would be a separate key in some terminals; we treat
		// the literal newline byte as Enter. Newline on submit is a decision
		// for the outer UI via slash commands. Here Enter submits.
		return true
	case KeyBackspace:
		if k.Alt {
			// Alt+Backspace (Option+Delete on macOS) — delete previous word.
			e.deleteWord()
		} else {
			e.backspace()
		}
	case KeyDelete:
		e.delete()
	case KeyLeft:
		if k.Alt {
			e.moveWordLeft()
		} else {
			e.moveLeft()
		}
	case KeyRight:
		if k.Alt {
			e.moveWordRight()
		} else {
			e.moveRight()
		}
	case KeyUp:
		if e.CursorR == 0 {
			e.historyPrev()
		} else {
			e.CursorR--
			if e.CursorC > runeLen(e.Lines[e.CursorR]) {
				e.CursorC = runeLen(e.Lines[e.CursorR])
			}
		}
	case KeyDown:
		if e.CursorR == len(e.Lines)-1 {
			e.historyNext()
		} else {
			e.CursorR++
			if e.CursorC > runeLen(e.Lines[e.CursorR]) {
				e.CursorC = runeLen(e.Lines[e.CursorR])
			}
		}
	case KeyHome, KeyCtrlA:
		e.CursorC = 0
	case KeyEnd, KeyCtrlE:
		e.CursorC = runeLen(e.Lines[e.CursorR])
	case KeyCtrlU:
		e.Lines[e.CursorR] = substringAfter(e.Lines[e.CursorR], e.CursorC)
		e.CursorC = 0
	case KeyCtrlK:
		e.Lines[e.CursorR] = substringBefore(e.Lines[e.CursorR], e.CursorC)
	case KeyCtrlW:
		e.deleteWord()
	case KeyPaste:
		e.insert(k.Paste)
	case KeyEsc:
		e.Clear()
	}
	return false
}

func (e *Editor) insert(s string) {
	e.histIdx = -1
	line := e.Lines[e.CursorR]
	pre := substringBefore(line, e.CursorC)
	post := substringAfter(line, e.CursorC)
	// Split pasted text on newlines.
	parts := strings.Split(s, "\n")
	if len(parts) == 1 {
		e.Lines[e.CursorR] = pre + s + post
		e.CursorC += runeLen(s)
		return
	}
	newLines := make([]string, 0, len(e.Lines)+len(parts)-1)
	newLines = append(newLines, e.Lines[:e.CursorR]...)
	newLines = append(newLines, pre+parts[0])
	for i := 1; i < len(parts)-1; i++ {
		newLines = append(newLines, parts[i])
	}
	last := parts[len(parts)-1]
	newLines = append(newLines, last+post)
	newLines = append(newLines, e.Lines[e.CursorR+1:]...)
	e.Lines = newLines
	e.CursorR += len(parts) - 1
	e.CursorC = runeLen(last)
}

func (e *Editor) newline() {
	e.histIdx = -1
	line := e.Lines[e.CursorR]
	pre := substringBefore(line, e.CursorC)
	post := substringAfter(line, e.CursorC)
	e.Lines[e.CursorR] = pre
	e.Lines = append(e.Lines, "")
	copy(e.Lines[e.CursorR+2:], e.Lines[e.CursorR+1:])
	e.Lines[e.CursorR+1] = post
	e.CursorR++
	e.CursorC = 0
}

func (e *Editor) backspace() {
	e.histIdx = -1
	if e.CursorC == 0 {
		if e.CursorR == 0 {
			return
		}
		prev := e.Lines[e.CursorR-1]
		cur := e.Lines[e.CursorR]
		e.Lines = append(e.Lines[:e.CursorR], e.Lines[e.CursorR+1:]...)
		e.CursorR--
		e.CursorC = runeLen(prev)
		e.Lines[e.CursorR] = prev + cur
		return
	}
	line := e.Lines[e.CursorR]
	e.Lines[e.CursorR] = substringBefore(line, e.CursorC-1) + substringAfter(line, e.CursorC)
	e.CursorC--
}

func (e *Editor) delete() {
	e.histIdx = -1
	line := e.Lines[e.CursorR]
	if e.CursorC == runeLen(line) {
		if e.CursorR == len(e.Lines)-1 {
			return
		}
		next := e.Lines[e.CursorR+1]
		e.Lines = append(e.Lines[:e.CursorR+1], e.Lines[e.CursorR+2:]...)
		e.Lines[e.CursorR] = line + next
		return
	}
	e.Lines[e.CursorR] = substringBefore(line, e.CursorC) + substringAfter(line, e.CursorC+1)
}

// moveWordLeft jumps the cursor to the start of the previous word,
// using the same word-separator rules as deleteWord. If already at the
// start of the line, wraps to the end of the previous line.
func (e *Editor) moveWordLeft() {
	if e.CursorC == 0 {
		if e.CursorR > 0 {
			e.CursorR--
			e.CursorC = runeLen(e.Lines[e.CursorR])
		}
		return
	}
	r := []rune(e.Lines[e.CursorR])
	i := e.CursorC
	for i > 0 && isWordSep(r[i-1]) {
		i--
	}
	for i > 0 && !isWordSep(r[i-1]) {
		i--
	}
	e.CursorC = i
}

// moveWordRight jumps the cursor to the start of the next word. If
// already at the end of the line, wraps to the start of the next line.
func (e *Editor) moveWordRight() {
	line := e.Lines[e.CursorR]
	if e.CursorC >= runeLen(line) {
		if e.CursorR < len(e.Lines)-1 {
			e.CursorR++
			e.CursorC = 0
		}
		return
	}
	r := []rune(line)
	i := e.CursorC
	for i < len(r) && !isWordSep(r[i]) {
		i++
	}
	for i < len(r) && isWordSep(r[i]) {
		i++
	}
	e.CursorC = i
}

func (e *Editor) moveLeft() {
	if e.CursorC > 0 {
		e.CursorC--
		return
	}
	if e.CursorR > 0 {
		e.CursorR--
		e.CursorC = runeLen(e.Lines[e.CursorR])
	}
}

func (e *Editor) moveRight() {
	if e.CursorC < runeLen(e.Lines[e.CursorR]) {
		e.CursorC++
		return
	}
	if e.CursorR < len(e.Lines)-1 {
		e.CursorR++
		e.CursorC = 0
	}
}

func (e *Editor) deleteWord() {
	e.histIdx = -1
	line := e.Lines[e.CursorR]
	if e.CursorC == 0 {
		e.backspace()
		return
	}
	r := []rune(line)
	i := e.CursorC
	// Step 1: walk over any trailing whitespace to the left of the cursor.
	for i > 0 && isWordSep(r[i-1]) {
		i--
	}
	// Step 2: walk over the word itself (non-separator runes).
	for i > 0 && !isWordSep(r[i-1]) {
		i--
	}
	e.Lines[e.CursorR] = string(r[:i]) + string(r[e.CursorC:])
	e.CursorC = i
}

// isWordSep reports whether r is a word separator. Anything that isn't
// a letter, digit, or underscore counts as a separator so delete-word
// feels natural on paths, code, and prose.
func isWordSep(r rune) bool {
	switch {
	case r == '_':
		return false
	case r >= '0' && r <= '9':
		return false
	case r >= 'a' && r <= 'z':
		return false
	case r >= 'A' && r <= 'Z':
		return false
	case r >= 0x80:
		// Treat all non-ASCII as word chars so CJK/äöü don't get chopped.
		return false
	}
	return true
}

// ---- history ----

// PushHistory saves s to the history ring.
func (e *Editor) PushHistory(s string) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return
	}
	if n := len(e.History); n > 0 && e.History[n-1] == s {
		return
	}
	e.History = append(e.History, s)
	if len(e.History) > 200 {
		e.History = e.History[len(e.History)-200:]
	}
	e.histIdx = -1
}

func (e *Editor) historyPrev() {
	if len(e.History) == 0 {
		return
	}
	if e.histIdx == -1 {
		e.savedDraft = e.Value()
		e.histIdx = len(e.History) - 1
	} else if e.histIdx > 0 {
		e.histIdx--
	}
	e.SetValue(e.History[e.histIdx])
	// SetValue resets histIdx; restore.
	e.histIdx = e.findInHistory()
}

func (e *Editor) historyNext() {
	if e.histIdx == -1 {
		return
	}
	if e.histIdx == len(e.History)-1 {
		e.histIdx = -1
		e.SetValue(e.savedDraft)
		e.histIdx = -1
		return
	}
	e.histIdx++
	e.SetValue(e.History[e.histIdx])
	e.histIdx = e.findInHistory()
}

func (e *Editor) findInHistory() int {
	v := e.Value()
	for i, h := range e.History {
		if h == v {
			return i
		}
	}
	return -1
}

// ---- rendering ----

// Render returns the editor's visible lines (wrapped to width).
// visualRow/visualCol describe where the cursor lands within the returned lines.
func (e *Editor) Render(width int) (lines []string, visualRow, visualCol int) {
	promptLen := visibleWidth(e.Prompt)
	indent := strings.Repeat(" ", promptLen)

	for r, line := range e.Lines {
		var prefix string
		if r == 0 {
			prefix = e.Prompt
		} else {
			prefix = indent
		}
		wrapped := wrapLine(prefix+line, width, indent)
		if r == e.CursorR {
			// Compute where CursorC lands inside the wrapped rows by
			// walking the wrapped output character-by-character and
			// tracking which wrapped row + column corresponds to the
			// cursor's rune index in `line`. This is the only reliable
			// answer under word-wrap, where a simple (promptLen+col)/width
			// formula overshoots when wrapLine broke on a space.
			targetRunes := e.CursorC
			row, col := locateCursor(wrapped, prefix, line, targetRunes, indent)
			visualRow = len(lines) + row
			visualCol = col
		}
		lines = append(lines, wrapped...)
	}
	return lines, visualRow, visualCol
}

// locateCursor finds the wrapped row + visible column corresponding to
// `targetRunes` rune positions into the logical `line`, given that the
// wrapped output `wrapped` started with `prefix` on its first row and
// uses `cont` as the continuation indent on subsequent rows.
func locateCursor(wrapped []string, prefix, line string, targetRunes int, cont string) (int, int) {
	prefixW := visibleWidth(prefix)
	contW := visibleWidth(cont)
	lineRunes := []rune(line)
	if targetRunes > len(lineRunes) {
		targetRunes = len(lineRunes)
	}

	seenRunes := 0
	for row, w := range wrapped {
		// strip leading prefix / continuation indent before counting runes
		body := w
		var leadW int
		if row == 0 {
			if strings.HasPrefix(body, prefix) {
				body = body[len(prefix):]
			}
			leadW = prefixW
		} else {
			if strings.HasPrefix(body, cont) {
				body = body[len(cont):]
			}
			leadW = contW
		}
		bodyRunes := []rune(body)
		// Could this wrapped row contain the cursor?
		if targetRunes <= seenRunes+len(bodyRunes) {
			// Column inside body.
			inner := targetRunes - seenRunes
			if inner < 0 {
				inner = 0
			}
			col := leadW + runewidth.StringWidth(string(bodyRunes[:inner]))
			return row, col
		}
		seenRunes += len(bodyRunes)
		// Word-wrap may have dropped whitespace at the boundary; skip the
		// corresponding runes in `line` so counts stay aligned.
		for seenRunes < len(lineRunes) && lineRunes[seenRunes] == ' ' {
			seenRunes++
			if seenRunes >= targetRunes {
				// Cursor landed in the skipped whitespace — place it at
				// the start of the next wrapped row.
				nextRow := row + 1
				if nextRow >= len(wrapped) {
					nextRow = row
				}
				return nextRow, contW
			}
		}
	}
	// Fallback: end of the last wrapped row.
	if len(wrapped) == 0 {
		return 0, prefixW
	}
	last := wrapped[len(wrapped)-1]
	return len(wrapped) - 1, visibleWidth(last)
}

// ---- helpers ----

func substringBefore(s string, col int) string {
	r := []rune(s)
	if col > len(r) {
		col = len(r)
	}
	return string(r[:col])
}

func substringAfter(s string, col int) string {
	r := []rune(s)
	if col > len(r) {
		col = len(r)
	}
	return string(r[col:])
}

func runeLen(s string) int { return len([]rune(s)) }

func visualColumn(s string, runeCol int) int {
	r := []rune(s)
	if runeCol > len(r) {
		runeCol = len(r)
	}
	return runewidth.StringWidth(string(r[:runeCol]))
}

func visibleWidth(s string) int {
	return runewidth.StringWidth(stripANSI(s))
}

func stripANSI(s string) string {
	// Minimal ANSI stripper; handles CSI sequences (ESC [ ... final).
	var out []rune
	i := 0
	runes := []rune(s)
	for i < len(runes) {
		if runes[i] == 0x1b && i+1 < len(runes) && runes[i+1] == '[' {
			i += 2
			for i < len(runes) {
				c := runes[i]
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			continue
		}
		out = append(out, runes[i])
		i++
	}
	return string(out)
}

func wrapLine(s string, width int, cont string) []string {
	if width <= 0 {
		return []string{s}
	}

	// Tokenize on spaces while preserving their widths. Runs of spaces
	// stay attached to the preceding word so trailing whitespace doesn't
	// create empty wrapped lines.
	type token struct {
		text  string
		width int
		space bool // true if this token is whitespace only
	}
	var tokens []token
	{
		var buf strings.Builder
		var bufW int
		inSpace := false
		flush := func() {
			if buf.Len() == 0 {
				return
			}
			tokens = append(tokens, token{text: buf.String(), width: bufW, space: inSpace})
			buf.Reset()
			bufW = 0
		}
		for _, r := range s {
			rSpace := r == ' ' || r == '\t'
			if buf.Len() == 0 {
				inSpace = rSpace
			} else if rSpace != inSpace {
				flush()
				inSpace = rSpace
			}
			buf.WriteRune(r)
			bufW += runewidth.RuneWidth(r)
		}
		flush()
	}

	var out []string
	var cur strings.Builder
	curW := 0
	firstLine := true
	contW := visibleWidth(cont)

	newLine := func() {
		out = append(out, cur.String())
		cur.Reset()
		curW = 0
		if !firstLine {
			cur.WriteString(cont)
			curW = contW
		}
		firstLine = false
	}

	for i := 0; i < len(tokens); i++ {
		tk := tokens[i]

		// Drop leading whitespace after a wrap.
		if tk.space && curW == contW && !firstLine {
			continue
		}

		if curW+tk.width <= width {
			cur.WriteString(tk.text)
			curW += tk.width
			continue
		}

		// Token overflows. If current line has content, break first.
		if cur.Len() > 0 && !(firstLine && curW == 0) {
			// Drop trailing whitespace on the line we're about to break.
			trimmed := strings.TrimRight(cur.String(), " \t")
			cur.Reset()
			cur.WriteString(trimmed)
			curW = visibleWidth(trimmed)
			newLine()
			// Re-process this token on the fresh line.
			if tk.space {
				continue
			}
		}

		// Token is longer than the available width on an empty line.
		// Split the token itself rune-by-rune.
		if tk.width > width-curW {
			for _, r := range tk.text {
				rw := runewidth.RuneWidth(r)
				if curW+rw > width {
					newLine()
				}
				cur.WriteRune(r)
				curW += rw
			}
		} else {
			cur.WriteString(tk.text)
			curW += tk.width
		}
	}

	if cur.Len() > 0 || len(out) == 0 {
		out = append(out, cur.String())
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
