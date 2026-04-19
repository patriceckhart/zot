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
	lines = append(lines, th.FG256(th.Muted, "pick a session to resume (↑/↓, enter, esc to cancel)"))
	for i, s := range d.sessions {
		plain := "  " + formatSessionRowPlain(s, width-2)
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}
	lines = append(lines, frameRule(th, width))
	return lines
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
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.sessions)-1 {
			d.cursor++
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
