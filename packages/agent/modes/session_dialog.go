package modes

import (
	"fmt"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

// sessionDialog is the inline picker shown when the user runs /sessions.
type sessionDialog struct {
	active   bool
	sessions []core.SessionSummary
	cursor   int
	renaming bool
	rename   string
	deleting bool

	// MaxRows is the maximum number of session rows the dialog
	// will render in a single frame. Set by the host right before
	// Render based on the available chat space; if 0, the dialog
	// falls back to rendering every row (original behaviour).
	// When the list is longer than MaxRows the dialog scrolls so
	// the cursor stays visible and tags the first/last visible
	// entry with a muted "↑ N more" / "↓ N more" row so the user
	// knows there's offscreen content.
	MaxRows int

	// viewTop is the index of the first session currently drawn.
	// Adjusted to follow the cursor on up/down moves.
	viewTop int
}

// sessionDialogAction is returned by HandleKey.
type sessionDialogAction struct {
	Select  bool
	Delete  bool
	Path    string
	Close   bool
	Renamed bool
}

func newSessionDialog() *sessionDialog { return &sessionDialog{} }

// Open populates the dialog from root + cwd and shows it. Empty
// sessions (zero messages) are filtered out so the currently-running
// session, a freshly-opened one that hasn't received a prompt yet,
// and any stale empties that haven't been pruned yet all stay out
// of the picker. Resuming an empty session is a no-op anyway.
func (d *sessionDialog) Open(root, cwd string) {
	all := core.DescribeSessions(root, cwd)
	filtered := make([]core.SessionSummary, 0, len(all))
	for _, s := range all {
		if s.MessageCount == 0 {
			continue
		}
		filtered = append(filtered, s)
	}
	d.sessions = filtered
	d.cursor = 0
	d.viewTop = 0
	d.renaming = false
	d.rename = ""
	d.deleting = false
	d.active = true
}

// CursorPos returns the row/col for the terminal cursor when in
// rename mode. Returns -1, -1 otherwise.
func (d *sessionDialog) CursorPos() (row, col int) {
	if !d.renaming {
		return -1, -1
	}
	// Row: frameHeader + padDialogFrame blank + rename hint = row 3 (0-indexed).
	// Col: "  ▌ " prefix + text length.
	return 3, 4 + len([]rune(d.rename))
}

// Close hides the dialog and abandons any pending edit or deletion.
func (d *sessionDialog) Close() {
	d.active = false
	d.renaming = false
	d.rename = ""
	d.deleting = false
}

// Active reports whether the dialog is visible and consumes input.
func (d *sessionDialog) Active() bool { return d != nil && d.active }

// Render returns the dialog lines.
func (d *sessionDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "sessions", width))
	if len(d.sessions) == 0 {
		lines = append(lines, th.FG256(th.Muted, "no previous sessions for this directory"))
		lines = append(lines, th.FG256(th.Muted, "press esc to close"))
		lines = append(lines, frameRule(th, width))
		return lines
	}
	if d.renaming {
		lines = append(lines, th.FG256(th.Muted, "rename session (enter save, esc cancel):"))
		text := d.rename
		if text == "" {
			text = th.FG256(th.Muted, "session name")
		} else {
			text = th.FG256(th.FG, text)
		}
		lines = append(lines, "  "+th.FG256(th.Accent, "▌ ")+text)
		lines = append(lines, frameRule(th, width))
		return lines
	}
	if d.deleting {
		s := d.sessions[d.cursor]
		lines = append(lines, th.FG256(th.Muted, "permanently delete this session?"))
		lines = append(lines, "  "+formatSessionRowPlain(s, width-2))
		lines = append(lines, th.FG256(th.Muted, "press y to delete, enter/esc to cancel"))
		lines = append(lines, frameRule(th, width))
		return lines
	}
	lines = append(lines, th.FG256(th.Muted, "pick a session (↑/↓, pgup/pgdn, enter resume, r rename, d delete, esc cancel)"))

	// Viewport: windowed slice of d.sessions around d.cursor so a
	// list taller than the terminal still scrolls. Caller sets
	// MaxRows to the number of rows available for session entries
	// (i.e. excluding the header, hint, chrome). When it's zero or
	// bigger than the list, we draw everything.
	total := len(d.sessions)
	window := d.MaxRows
	if window <= 0 || window >= total {
		window = total
	}
	d.viewTop = clampViewTop(d.viewTop, d.cursor, window, total)
	viewBot := d.viewTop + window
	if viewBot > total {
		viewBot = total
	}

	// Top indicator: how many rows are above the viewport.
	if d.viewTop > 0 {
		hidden := d.viewTop
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  ↑ %d more above", hidden)))
	}
	for i := d.viewTop; i < viewBot; i++ {
		s := d.sessions[i]
		plain := "  " + formatSessionRowPlain(s, width-2)
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}
	// Bottom indicator: how many rows are below the viewport.
	if viewBot < total {
		hidden := total - viewBot
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  ↓ %d more below", hidden)))
	}
	lines = append(lines, frameRule(th, width))
	return lines
}

// clampViewTop returns a viewTop that keeps cursor visible in a
// window of the given size over a list of `total` rows. Leaves one
// row of padding above/below where possible so moving the cursor
// doesn't land right on the top/bottom edge — easier to see what
// direction you're moving.
func clampViewTop(viewTop, cursor, window, total int) int {
	if window <= 0 || total <= 0 {
		return 0
	}
	if window >= total {
		return 0
	}
	pad := 2
	if window < 6 {
		pad = 0
	}
	if cursor < viewTop+pad {
		viewTop = cursor - pad
	}
	if cursor >= viewTop+window-pad {
		viewTop = cursor - window + pad + 1
	}
	if viewTop < 0 {
		viewTop = 0
	}
	if viewTop+window > total {
		viewTop = total - window
	}
	return viewTop
}

// formatSessionRowPlain returns the session row body without any ANSI
// styling so the caller can wrap it in either a plain mute color or a
// full-row selection highlight. The returned string is guaranteed to
// fit within maxWidth visible characters so the terminal never soft-
// wraps it into the next row.
func formatSessionRowPlain(s core.SessionSummary, maxWidth int) string {
	when := formatRelative(s.Started)
	summary := strings.TrimSpace(s.Title)
	if summary == "" {
		summary = strings.TrimSpace(s.FirstUserText)
	}
	if summary == "" {
		summary = "(empty)"
	}
	summary = strings.ReplaceAll(summary, "\n", " ")
	if s.Alias != "" {
		summary = "[" + s.Alias + "] " + summary
	}
	left := fmt.Sprintf("%-14s  %s/%s  %d msgs  $%.4f  ",
		when, s.Provider, s.Model, s.MessageCount, s.TotalCost)
	room := maxWidth - len([]rune(left))
	if room < 4 {
		room = 4
	}
	runes := []rune(summary)
	if len(runes) > room {
		summary = string(runes[:room-3]) + "..."
	}
	row := left + summary
	// Hard clamp: ensure the full row never exceeds maxWidth.
	rowRunes := []rune(row)
	if len(rowRunes) > maxWidth {
		if maxWidth <= 3 {
			row = strings.Repeat(".", maxWidth)
		} else {
			row = string(rowRunes[:maxWidth-3]) + "..."
		}
	}
	return row
}

func formatRelative(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// HandleKey advances the dialog and returns an action to apply, if any.
func (d *sessionDialog) HandleKey(k tui.Key) sessionDialogAction {
	if d.deleting {
		switch k.Kind {
		case tui.KeyRune:
			if (k.Rune == 'y' || k.Rune == 'Y') && d.cursor < len(d.sessions) {
				path := d.sessions[d.cursor].Path
				d.deleting = false
				return sessionDialogAction{Delete: true, Path: path}
			}
			if k.Rune == 'n' || k.Rune == 'N' {
				d.deleting = false
			}
		case tui.KeyEnter, tui.KeyEsc:
			d.deleting = false
		}
		return sessionDialogAction{}
	}

	// Rename mode: type the new name.
	if d.renaming {
		switch k.Kind {
		case tui.KeyEnter:
			title := strings.TrimSpace(d.rename)
			if title != "" && d.cursor < len(d.sessions) {
				path := d.sessions[d.cursor].Path
				_ = core.RenameSession(path, title)
				d.sessions[d.cursor].Title = title
			}
			d.renaming = false
			d.rename = ""
			return sessionDialogAction{Renamed: true}
		case tui.KeyEsc:
			d.renaming = false
			d.rename = ""
			return sessionDialogAction{}
		case tui.KeyBackspace:
			if len(d.rename) > 0 {
				r := []rune(d.rename)
				d.rename = string(r[:len(r)-1])
			}
			return sessionDialogAction{}
		case tui.KeyPaste:
			d.rename += k.Paste
			return sessionDialogAction{}
		case tui.KeyRune:
			if k.Rune != 0 {
				d.rename += string(k.Rune)
			}
			return sessionDialogAction{}
		}
		return sessionDialogAction{}
	}

	page := d.MaxRows
	if page <= 0 {
		page = 10
	}
	if page > 1 {
		page--
	}
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.sessions)-1 {
			d.cursor++
		}
	case tui.KeyPageUp:
		d.cursor -= page
		if d.cursor < 0 {
			d.cursor = 0
		}
	case tui.KeyPageDown:
		d.cursor += page
		if d.cursor >= len(d.sessions) {
			d.cursor = len(d.sessions) - 1
			if d.cursor < 0 {
				d.cursor = 0
			}
		}
	case tui.KeyHome:
		d.cursor = 0
	case tui.KeyEnd:
		if len(d.sessions) > 0 {
			d.cursor = len(d.sessions) - 1
		}
	case tui.KeyEsc:
		d.Close()
		return sessionDialogAction{Close: true}
	case tui.KeyEnter:
		if len(d.sessions) == 0 {
			d.Close()
			return sessionDialogAction{Close: true}
		}
		s := d.sessions[d.cursor]
		d.Close()
		return sessionDialogAction{Select: true, Path: s.Path}
	case tui.KeyRune:
		if k.Rune == 'r' && len(d.sessions) > 0 {
			s := d.sessions[d.cursor]
			d.renaming = true
			if s.Title != "" {
				d.rename = s.Title
			} else {
				d.rename = ""
			}
			return sessionDialogAction{}
		}
		if k.Rune == 'd' && len(d.sessions) > 0 {
			d.deleting = true
		}
	}
	return sessionDialogAction{}
}

// Remove deletes path from the visible rows after the host has removed the
// corresponding file. The next row stays selected when possible.
func (d *sessionDialog) Remove(path string) {
	for idx := range d.sessions {
		if d.sessions[idx].Path != path {
			continue
		}
		d.sessions = append(d.sessions[:idx], d.sessions[idx+1:]...)
		if d.cursor >= len(d.sessions) {
			d.cursor = len(d.sessions) - 1
		}
		if d.cursor < 0 {
			d.cursor = 0
		}
		d.viewTop = clampViewTop(d.viewTop, d.cursor, d.MaxRows, len(d.sessions))
		return
	}
}
