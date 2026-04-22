package tui

import (
	"fmt"
	"net/url"
	"regexp"
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

	// lastRenderWidth is the column count passed to the most recent
	// Render() call. Up/Down key handling needs this to walk the
	// same visual layout the user sees: a logical line that wraps to
	// two rows should respond to Up by moving to the previous visual
	// row, not do nothing because CursorR is already 0.
	lastRenderWidth int

	// pastes stores the full content of every multi-line paste,
	// keyed by the id embedded in the visible placeholder token.
	// Pasted text is collapsed to "[paste #N +L lines]" in the
	// editor so a 500-line drop doesn't explode the input area;
	// SubmitValue() expands placeholders back to their real bodies
	// right before the prompt goes to the agent. The map is reset
	// on Clear() so stale pastes never leak into a follow-up turn.
	pastes   map[int]string
	pasteSeq int
}

// NewEditor returns an empty editor with the given prompt.
func NewEditor(prompt string) *Editor {
	return &Editor{
		Lines:  []string{""},
		Prompt: prompt,
	}
}

// Value returns the buffer as a single string, WITHOUT expanding
// paste placeholders. Used for anything that should reflect what's
// visible on screen (history, slash-command detection, editor
// state). For the string that actually goes to the agent, use
// SubmitValue(), which expands each [paste #N +L lines] token
// back into the full pasted body.
func (e *Editor) Value() string { return strings.Join(e.Lines, "\n") }

// SubmitValue returns the buffer with every paste placeholder
// expanded to its stored body. Call once at submit time; the
// expansion is lossless (placeholders are only injected in
// HandleKey for KeyPaste with multi-line content).
//
// Expansion is non-destructive: the internal paste map isn't
// touched. Clear() is what resets both the placeholder text and
// the map, and the caller already calls Clear() right after
// reading SubmitValue() as part of the submit flow.
func (e *Editor) SubmitValue() string {
	raw := e.Value()
	if len(e.pastes) == 0 || !strings.Contains(raw, "[pasted text #") {
		return raw
	}
	return expandPastePlaceholders(raw, e.pastes)
}

// SetValue replaces the buffer and places the cursor at the end.
// Also drops any stored pastes because the placeholders they back
// are now gone from the visible text.
func (e *Editor) SetValue(s string) {
	e.Lines = strings.Split(s, "\n")
	if len(e.Lines) == 0 {
		e.Lines = []string{""}
	}
	e.CursorR = len(e.Lines) - 1
	e.CursorC = runeLen(e.Lines[e.CursorR])
	e.pastes = nil
	e.pasteSeq = 0
}

// Clear resets the buffer.
func (e *Editor) Clear() { e.SetValue("") }

// IsEmpty reports whether the buffer has no visible content.
func (e *Editor) IsEmpty() bool {
	return len(e.Lines) == 1 && e.Lines[0] == ""
}

// HandleKey applies k to the editor. It returns submit=true when
// the user pressed enter and there is content to send; the caller
// should read SubmitValue() and then Clear().
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
		// Visual-row navigation. When a single logical line wraps
		// to several visual rows, Up needs to climb one visual
		// row — which may mean moving within the same
		// e.Lines[CursorR] back toward an earlier rune index,
		// not jumping to CursorR-1. Buffer-line navigation is
		// subsumed: a visual row above may also live in the
		// previous logical line when a short line precedes a
		// wrapped one.
		e.moveCursorVisual(-1)
	case KeyDown:
		e.moveCursorVisual(+1)
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
		// Large multi-line pastes are collapsed to a short
		// placeholder token so the editor doesn't balloon to
		// hundreds of rows. The full body is stashed in e.pastes,
		// keyed by a monotonically increasing id, and swapped
		// back in at submit time via SubmitValue. Threshold:
		// two or more newlines triggers collapse; one-liners and
		// drag-dropped file paths fall through to the original
		// insert path (including file-path quoting).
		if pasteShouldCollapse(k.Paste) {
			if e.pastes == nil {
				e.pastes = map[int]string{}
			}
			e.pasteSeq++
			id := e.pasteSeq
			e.pastes[id] = k.Paste
			e.insert(formatPastePlaceholder(id, k.Paste))
		} else {
			// macOS Terminal / iTerm / Ghostty deliver drag-dropped
			// files as bracketed-paste text. Detect that pattern
			// and wrap the path(s) in single quotes so the agent
			// sees them as one argument.
			e.insert(quotePastedFilePaths(k.Paste))
		}
	case KeyEsc:
		e.Clear()
	}
	return false
}

// quotePastedFilePaths returns paste with any drag-dropped file
// paths wrapped in single quotes. Heuristics:
//
//   - one line only; multi-line text is returned unchanged so we
//     never touch a real paste of code.
//   - whitespace-separated tokens are inspected one by one. A token
//     that looks like a filesystem path (absolute, ~/relative, or
//     file:// URL) is normalised and re-quoted; everything else is
//     left exactly as the user dropped it.
//   - terminal-style backslash escapes ("foo\ bar.png") are folded
//     back to literal characters before quoting, since wrapping in
//     single quotes makes them unnecessary.
//   - file:// URLs are URL-decoded and stripped of the scheme so
//     the agent receives a plain filesystem path.
//   - if a path contains a literal single quote, it's escaped using
//     the standard '\” splice.
//
// Pastes that don't contain any path-shaped tokens are returned
// unchanged.
func quotePastedFilePaths(paste string) string {
	if paste == "" || strings.ContainsRune(paste, '\n') {
		return paste
	}
	trimmed := strings.TrimSpace(paste)
	if trimmed == "" {
		return paste
	}

	// Split on runs of whitespace, preserving the original separators
	// so multi-file drops keep their spacing on rebuild. Backslash
	// before a space is treated as part of the preceding token, since
	// macOS Terminal escapes spaces in dropped paths that way.
	tokens := splitPreservingSeparators(paste)
	changed := false
	for i, tk := range tokens {
		if tk.isSpace {
			continue
		}
		if p, ok := normalisePathToken(tk.text); ok {
			tokens[i].text = singleQuote(p)
			changed = true
		}
	}
	if !changed {
		return paste
	}
	var out strings.Builder
	out.Grow(len(paste) + 8)
	for _, tk := range tokens {
		out.WriteString(tk.text)
	}
	return out.String()
}

type pasteToken struct {
	text    string
	isSpace bool
}

// splitPreservingSeparators splits s into runs of whitespace and
// runs of non-whitespace, keeping the separator runs intact so a
// rebuild via concatenation reproduces the original string exactly.
// A backslash immediately followed by a space ("\ ") is treated as
// part of the preceding non-whitespace run, since that's how macOS
// Terminal escapes spaces in drag-dropped file paths.
func splitPreservingSeparators(s string) []pasteToken {
	runes := []rune(s)
	var out []pasteToken
	var buf strings.Builder
	inSpace := false
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		out = append(out, pasteToken{text: buf.String(), isSpace: inSpace})
		buf.Reset()
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		isEscapedSpace := r == '\\' && i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\t')
		rSpace := !isEscapedSpace && (r == ' ' || r == '\t')
		if buf.Len() == 0 {
			inSpace = rSpace
		} else if rSpace != inSpace {
			flush()
			inSpace = rSpace
		}
		if isEscapedSpace {
			// Skip the backslash; emit the literal space as part of
			// this non-whitespace token. normalisePathToken's
			// unescapeBackslashes is now redundant for this path but
			// stays in case some other source escapes other chars.
			buf.WriteRune(runes[i+1])
			i++
			continue
		}
		buf.WriteRune(r)
	}
	flush()
	return out
}

// normalisePathToken decides whether tk is a drag-dropped path and,
// if so, returns the cleaned-up path string.
func normalisePathToken(tk string) (string, bool) {
	// Strip pre-existing surrounding quotes; we'll re-quote consistently.
	if n := len(tk); n >= 2 {
		if (tk[0] == '\'' && tk[n-1] == '\'') || (tk[0] == '"' && tk[n-1] == '"') {
			tk = tk[1 : n-1]
		}
	}

	// file:// URL form: decode and strip the scheme.
	if strings.HasPrefix(tk, "file://") {
		decoded, err := url.PathUnescape(strings.TrimPrefix(tk, "file://"))
		if err != nil {
			return "", false
		}
		if decoded == "" || decoded[0] != '/' {
			return "", false
		}
		return decoded, true
	}

	// Looks-like-a-path heuristic: starts with /, ~, ~/. Must contain
	// at least one path separator after the prefix (otherwise a bare
	// "/" or "~" gets quoted, which is never what the user meant).
	if !strings.HasPrefix(tk, "/") && !strings.HasPrefix(tk, "~") {
		return "", false
	}
	if !strings.ContainsAny(tk[1:], "/.") {
		return "", false
	}
	unescaped := unescapeBackslashes(tk)
	if strings.ContainsAny(unescaped, "|;&$`<>") {
		return "", false
	}
	return unescaped, true
}

// unescapeBackslashes turns "foo\ bar" into "foo bar". Terminal
// drag-and-drop on macOS uses backslash escaping by default; quoting
// makes that unnecessary.
func unescapeBackslashes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	prev := false
	for _, r := range s {
		if prev {
			out.WriteRune(r)
			prev = false
			continue
		}
		if r == '\\' {
			prev = true
			continue
		}
		out.WriteRune(r)
	}
	if prev {
		out.WriteRune('\\')
	}
	return out.String()
}

// singleQuote wraps s in single quotes, escaping any embedded single
// quote using the splice idiom: 'foo'\”bar' for foo'bar.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Insert places s into the editor at the cursor, splitting on
// newlines so multi-line pastes preserve their structure.
func (e *Editor) Insert(s string) { e.insert(s) }

func (e *Editor) insert(s string) {
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

// moveCursorVisual moves the cursor one visual row in direction
// dir (-1 = up, +1 = down) through the wrapped layout the user
// sees on screen. Handles both multi-line logical inputs and the
// case where a single long line wraps across several visual
// rows.
//
// Algorithm: rebuild the same wrapped layout Render produces,
// tagging each visual row with (logicalRow, runeOffsetStart,
// runeOffsetEnd, leadingWidth). Find the row the cursor sits on,
// then pick (row+dir) and map the cursor's current visual column
// (minus the target row's leading indent) to a rune index inside
// that row's slice of its logical line. No-op at the top/bottom
// edges of the whole buffer.
func (e *Editor) moveCursorVisual(dir int) {
	width := e.lastRenderWidth
	if width <= 0 {
		// Fall back to logical-line navigation if Render hasn't
		// been called yet (shouldn't happen in practice; the
		// host always renders once before accepting input).
		e.moveCursorLogical(dir)
		return
	}

	type vrow struct {
		logical    int    // e.Lines index
		runeStart  int    // rune offset into e.Lines[logical]
		runeEnd    int    // exclusive
		leadWidth  int    // width of prompt / cont indent on this row
		leadPrefix string // the prefix used (prompt on row 0, indent on cont)
	}

	promptLen := visibleWidth(e.Prompt)
	indent := strings.Repeat(" ", promptLen)

	var rows []vrow
	curVRow, curVCol := 0, 0
	for r, line := range e.Lines {
		prefix := indent
		if r == 0 {
			prefix = e.Prompt
		}
		wrapped := wrapLine(prefix+line, width, indent)
		lineRunes := []rune(line)
		seen := 0
		for wi, w := range wrapped {
			var leadW int
			var leadP string
			body := w
			if wi == 0 {
				if strings.HasPrefix(body, prefix) {
					body = body[len(prefix):]
				}
				leadW = promptLen
				leadP = prefix
			} else {
				if strings.HasPrefix(body, indent) {
					body = body[len(indent):]
				}
				leadW = promptLen
				leadP = indent
			}
			bodyRunes := []rune(body)
			start := seen
			end := seen + len(bodyRunes)
			rows = append(rows, vrow{
				logical: r, runeStart: start, runeEnd: end,
				leadWidth: leadW, leadPrefix: leadP,
			})
			// Record where the cursor currently sits.
			if r == e.CursorR && e.CursorC >= start && e.CursorC <= end {
				curVRow = len(rows) - 1
				inner := e.CursorC - start
				if inner < 0 {
					inner = 0
				}
				if inner > len(bodyRunes) {
					inner = len(bodyRunes)
				}
				curVCol = leadW + runewidth.StringWidth(string(bodyRunes[:inner]))
			}
			seen = end
			// Word-wrap often drops a single space at the boundary.
			for seen < len(lineRunes) && lineRunes[seen] == ' ' {
				seen++
			}
		}
	}

	target := curVRow + dir
	if target < 0 || target >= len(rows) {
		return
	}
	tr := rows[target]
	line := e.Lines[tr.logical]
	lineRunes := []rune(line)
	bodyRunes := lineRunes[tr.runeStart:tr.runeEnd]

	// Find the rune offset inside bodyRunes whose visible column
	// most closely matches curVCol after accounting for leadWidth.
	want := curVCol - tr.leadWidth
	if want < 0 {
		want = 0
	}
	best := 0
	bestW := 0
	for i := 1; i <= len(bodyRunes); i++ {
		w := runewidth.StringWidth(string(bodyRunes[:i]))
		if w > want {
			break
		}
		best = i
		bestW = w
		if w == want {
			break
		}
	}
	_ = bestW
	e.CursorR = tr.logical
	e.CursorC = tr.runeStart + best
}

// moveCursorLogical is the pre-visual-navigation fallback used
// when Render hasn't told us the terminal width yet. Walks the
// e.Lines array directly.
func (e *Editor) moveCursorLogical(dir int) {
	switch {
	case dir < 0 && e.CursorR > 0:
		e.CursorR--
	case dir > 0 && e.CursorR < len(e.Lines)-1:
		e.CursorR++
	default:
		return
	}
	if e.CursorC > runeLen(e.Lines[e.CursorR]) {
		e.CursorC = runeLen(e.Lines[e.CursorR])
	}
}

func (e *Editor) deleteWord() {
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

// ---- rendering ----

// Render returns the editor's visible lines (wrapped to width).
// visualRow/visualCol describe where the cursor lands within the returned lines.
func (e *Editor) Render(width int) (lines []string, visualRow, visualCol int) {
	e.lastRenderWidth = width
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

// wrapANSILine folds a string that may contain ANSI CSI escapes so that
// the visible width of each line stays within limit. Breaks happen on
// spaces when possible, falling back to mid-token splits for very long
// unbroken runs. Escape sequences are preserved verbatim and never
// counted toward the visible column.
func wrapANSILine(s string, limit int) []string {
	if limit <= 0 {
		return []string{s}
	}
	if visibleWidth(s) <= limit {
		return []string{s}
	}
	runes := []rune(s)
	var out []string
	var line strings.Builder
	var word strings.Builder
	lineW := 0
	wordW := 0

	flushLine := func() {
		out = append(out, line.String())
		line.Reset()
		lineW = 0
	}
	flushWord := func() {
		if word.Len() == 0 {
			return
		}
		if lineW+wordW > limit && lineW > 0 {
			flushLine()
		}
		// If the word alone is bigger than the limit, split it by runes.
		if wordW > limit {
			wr := []rune(word.String())
			for i := 0; i < len(wr); {
				r := wr[i]
				if r == 0x1b && i+1 < len(wr) && wr[i+1] == '[' {
					line.WriteRune(r)
					line.WriteRune(wr[i+1])
					i += 2
					for i < len(wr) {
						c := wr[i]
						line.WriteRune(c)
						i++
						if c >= 0x40 && c <= 0x7e {
							break
						}
					}
					continue
				}
				rw := runewidth.RuneWidth(r)
				if lineW+rw > limit && lineW > 0 {
					flushLine()
				}
				line.WriteRune(r)
				lineW += rw
				i++
			}
			word.Reset()
			wordW = 0
			return
		}
		line.WriteString(word.String())
		lineW += wordW
		word.Reset()
		wordW = 0
	}

	for i := 0; i < len(runes); {
		r := runes[i]
		if r == 0x1b && i+1 < len(runes) && runes[i+1] == '[' {
			word.WriteRune(r)
			word.WriteRune(runes[i+1])
			i += 2
			for i < len(runes) {
				c := runes[i]
				word.WriteRune(c)
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			continue
		}
		if r == ' ' || r == '\t' {
			flushWord()
			rw := runewidth.RuneWidth(r)
			if lineW+rw > limit && lineW > 0 {
				flushLine()
				i++
				continue
			}
			line.WriteRune(r)
			lineW += rw
			i++
			continue
		}
		word.WriteRune(r)
		wordW += runewidth.RuneWidth(r)
		i++
	}
	flushWord()
	if line.Len() > 0 {
		flushLine()
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
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
		// After flushing, everything we append next is a CONTINUATION
		// row, which must start with the cont indent so the editor's
		// visual cursor alignment stays consistent. The old code only
		// wrote cont when !firstLine BEFORE toggling firstLine, which
		// meant the very first wrap never got its indent. That caused
		// the terminal cursor to land in the wrong column after a
		// multi-line paste.
		firstLine = false
		cur.WriteString(cont)
		curW = contW
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

		// Token overflows.
		//
		// If the token on its own is wider than what fits on a
		// continuation line (width - contW), it'll need to be split
		// rune-by-rune no matter what. In that case, don't break
		// first: let the rune-split start right here at the current
		// column and wrap naturally. Breaking first would strand the
		// prompt (on firstLine) or the cont indent (on later lines)
		// on a row by itself, which is the drag-drop-long-path bug.
		if tk.width > width-contW {
			// Rune-by-rune starting from the current column. newLine()
			// inside handles wrapping and re-indents.
			for _, r := range tk.text {
				rw := runewidth.RuneWidth(r)
				if curW+rw > width {
					newLine()
				}
				cur.WriteRune(r)
				curW += rw
			}
			continue
		}

		// Token fits on a fresh continuation line; break first, then
		// emit it whole.
		if cur.Len() > 0 && !(firstLine && curW == 0) {
			trimmed := strings.TrimRight(cur.String(), " \t")
			cur.Reset()
			cur.WriteString(trimmed)
			curW = visibleWidth(trimmed)
			newLine()
			if tk.space {
				continue
			}
		}
		cur.WriteString(tk.text)
		curW += tk.width
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

// pasteCollapseLineThreshold and pasteCollapseCharThreshold govern
// when a bracketed paste gets collapsed to a [pasted text #N …]
// placeholder instead of being inserted inline. Either trigger
// alone is enough — a 500-line log dump and a 1200-character
// one-line log entry both bloat the editor in ways the user
// doesn't want to scroll through while composing a prompt.
const (
	pasteCollapseLineThreshold = 10
	pasteCollapseCharThreshold = 1000
)

// pasteShouldCollapse reports whether a pasted chunk is big enough
// to deserve a placeholder token instead of being inserted verbatim.
// Collapse on > 10 lines OR > 1000 characters, whichever fires
// first.
func pasteShouldCollapse(s string) bool {
	return countLines(s) > pasteCollapseLineThreshold || len(s) > pasteCollapseCharThreshold
}

// formatPastePlaceholder builds the visible marker for a
// collapsed paste. Two shapes:
//
//	[pasted text #N +L lines]  — used when the line count is the
//	                             trigger (multi-line dumps)
//	[pasted text #N C chars]   — used when the character count is
//	                             the trigger (long single-line
//	                             or near-single-line pastes)
//
// Line-triggered takes precedence so a 12-line 4000-char paste
// reads as "+12 lines", not "4000 chars".
func formatPastePlaceholder(id int, body string) string {
	if countLines(body) > pasteCollapseLineThreshold {
		return fmt.Sprintf("[pasted text #%d +%d lines]", id, countLines(body))
	}
	return fmt.Sprintf("[pasted text #%d %d chars]", id, len(body))
}

// countLines returns the number of visual lines in s. A trailing
// newline is not counted as an extra empty line so
// "foo\nbar\n" reads as 2 lines (what the user expects in the
// "+N lines" summary) instead of 3.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// pastePlaceholderRE matches both placeholder shapes the editor
// emits for a collapsed paste:
//
//	[pasted text #N +L lines]   — multi-line trigger
//	[pasted text #N C chars]    — long-char trigger
//
// Capture group 1 is the numeric id used to look up the full
// body in e.pastes; the rest of the token is free-form and gets
// discarded during expansion.
var pastePlaceholderRE = regexp.MustCompile(`\[pasted text #(\d+) (?:\+\d+ lines?|\d+ chars?)\]`)

// expandPastePlaceholders returns raw with every paste token
// swapped for the body stored under its id in pastes. Tokens
// whose id isn't in the map are left as-is (user deleted the
// corresponding entry somehow, or the id is spurious user text
// that happens to match the shape).
func expandPastePlaceholders(raw string, pastes map[int]string) string {
	return pastePlaceholderRE.ReplaceAllStringFunc(raw, func(match string) string {
		groups := pastePlaceholderRE.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		var id int
		if _, err := fmt.Sscanf(groups[1], "%d", &id); err != nil {
			return match
		}
		if body, ok := pastes[id]; ok {
			return body
		}
		return match
	})
}
