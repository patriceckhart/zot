package modes

import (
	"fmt"
	"strings"
	"time"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/tui"
)

// sessionDialog is the inline picker shown when the user runs /sessions.
type sessionDialog struct {
	active   bool
	sessions []core.SessionSummary
	cursor   int

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
	Select bool
	Path   string
	Close  bool
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
	d.active = true
}

// Close hides the dialog.
func (d *sessionDialog) Close() { d.active = false }

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
	lines = append(lines, th.FG256(th.Muted, "pick a session to resume (↑/↓, pgup/pgdn, enter, esc to cancel)"))

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
// full-row selection highlight.
func formatSessionRowPlain(s core.SessionSummary, maxWidth int) string {
	when := formatRelative(s.Started)
	summary := strings.TrimSpace(s.FirstUserText)
	if summary == "" {
		summary = "(empty)"
	}
	left := fmt.Sprintf("%-14s  %s/%s  %d msgs  $%.4f  ",
		when, s.Provider, s.Model, s.MessageCount, s.TotalCost)
	room := maxWidth - len(left)
	if room < 10 {
		room = 10
	}
	if len(summary) > room {
		summary = summary[:room-1] + "…"
	}
	summary = strings.ReplaceAll(summary, "\n", " ")
	return left + summary
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
	}
	return sessionDialogAction{}
}
