package modes

import (
	"github.com/patriceckhart/zot/internal/tui"
)

// logoutDialog is the picker shown when the user runs `/logout`
// without an argument. Lists each provider the user is currently
// logged into (apikey or oauth), plus an "all" entry and a cancel.
// Up/down to move, enter to pick, esc to cancel.
type logoutDialog struct {
	active bool
	items  []logoutItem
	cursor int
}

// logoutItem is one row in the picker. target is what gets passed
// to doLogout ("anthropic", "openai", or "all"); method is the
// short tag shown in muted text next to the provider ("apikey" or
// "oauth"), empty for the "all" row.
type logoutItem struct {
	label  string
	target string
	method string
}

// logoutAction is the outcome of a key press.
type logoutAction struct {
	Select bool
	Target string // one of: "anthropic", "openai", "all"
	Close  bool
}

func newLogoutDialog() *logoutDialog { return &logoutDialog{} }

// Open populates the picker with whichever providers are currently
// logged in. Returns false if nothing to log out of; the caller
// should fall back to showing a status message instead of opening
// an empty dialog.
func (d *logoutDialog) Open(items []logoutItem) bool {
	if len(items) == 0 {
		return false
	}
	d.items = items
	d.cursor = 0
	d.active = true
	return true
}

// Close hides the dialog.
func (d *logoutDialog) Close() { d.active = false }

// Active reports whether the dialog is consuming input.
func (d *logoutDialog) Active() bool { return d != nil && d.active }

// HandleKey advances the selection or resolves the dialog.
func (d *logoutDialog) HandleKey(k tui.Key) logoutAction {
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.items)-1 {
			d.cursor++
		}
	case tui.KeyEsc:
		d.Close()
		return logoutAction{Close: true}
	case tui.KeyEnter:
		if len(d.items) == 0 {
			d.Close()
			return logoutAction{Close: true}
		}
		it := d.items[d.cursor]
		d.Close()
		return logoutAction{Select: true, Target: it.target}
	}
	return logoutAction{}
}

// Render returns the dialog lines.
func (d *logoutDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "logout", width))
	lines = append(lines, th.FG256(th.Muted, "choose what to log out of (\u2191/\u2193, enter, esc to cancel):"))
	for i, it := range d.items {
		plain := "  " + it.label
		if it.method != "" {
			plain += "  " + th.FG256(th.Muted, "("+it.method+")")
		}
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}
	lines = append(lines, frameRule(th, width))
	return lines
}
