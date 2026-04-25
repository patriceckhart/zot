package modes

import (
	"github.com/patriceckhart/zot/internal/tui"
)

// sessionOpsDialog is the picker shown when the user runs `/session`
// without an argument. Offers the two portable-file operations on
// the current conversation: export (write the in-memory transcript
// plus meta to a .zotsession file) and import (load a .zotsession
// from another machine and swap it in as the active session).
//
// Shape mirrors telegramDialog and logoutDialog: tiny list, arrow
// keys to move, enter to pick, esc to cancel.
type sessionOpsDialog struct {
	active bool
	items  []sessionOpsItem
	cursor int
}

type sessionOpsItem struct {
	label  string
	action string // "export" | "import"
	hint   string
}

type sessionOpsAction struct {
	Select bool
	Action string
	Close  bool
}

func newSessionOpsDialog() *sessionOpsDialog { return &sessionOpsDialog{} }

// Open shows the picker. Items are usually both "export" and
// "import" but the caller can suppress either (e.g. hide export
// when the session is empty).
func (d *sessionOpsDialog) Open(items []sessionOpsItem) bool {
	if len(items) == 0 {
		return false
	}
	d.items = items
	d.cursor = 0
	d.active = true
	return true
}

// Close hides the dialog.
func (d *sessionOpsDialog) Close() { d.active = false }

// Active reports whether the dialog is consuming input.
func (d *sessionOpsDialog) Active() bool { return d != nil && d.active }

// HandleKey advances the selection or resolves the dialog.
func (d *sessionOpsDialog) HandleKey(k tui.Key) sessionOpsAction {
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
		return sessionOpsAction{Close: true}
	case tui.KeyEnter:
		if len(d.items) == 0 {
			d.Close()
			return sessionOpsAction{Close: true}
		}
		it := d.items[d.cursor]
		d.Close()
		return sessionOpsAction{Select: true, Action: it.action}
	}
	return sessionOpsAction{}
}

// Render returns the dialog lines.
func (d *sessionOpsDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "session", width))
	lines = append(lines, th.FG256(th.Muted, "pick an action (\u2191/\u2193, enter, esc to cancel):"))
	for i, it := range d.items {
		text := "  " + it.label
		if it.hint != "" {
			text += "  (" + it.hint + ")"
		}
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(text, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, text))
		}
	}
	lines = append(lines, frameRule(th, width))
	return lines
}
