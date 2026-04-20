package modes

import (
	"github.com/patriceckhart/zot/internal/tui"
)

// telegramDialog is the picker shown when the user runs `/telegram`
// without an argument. Lists the three actions (connect, disconnect,
// status) and routes the choice back to the Interactive via
// telegramAction.
//
// Shape mirrors logoutDialog: a tiny list of rows with the cursor
// moved by arrow keys and enter to pick, esc to cancel.
type telegramDialog struct {
	active bool
	items  []telegramItem
	cursor int
}

type telegramItem struct {
	label  string
	action string // "connect" | "disconnect" | "status"
	hint   string // muted text shown after the label (e.g. "not configured", "active")
}

type telegramAction struct {
	Select bool
	Action string
	Close  bool
}

func newTelegramDialog() *telegramDialog { return &telegramDialog{} }

// Open shows the picker with items describing the current state.
// items is rendered in order; the caller builds it so connect is
// only offered when disconnected, and vice versa.
func (d *telegramDialog) Open(items []telegramItem) bool {
	if len(items) == 0 {
		return false
	}
	d.items = items
	d.cursor = 0
	d.active = true
	return true
}

// Close hides the dialog.
func (d *telegramDialog) Close() { d.active = false }

// Active reports whether the dialog is consuming input.
func (d *telegramDialog) Active() bool { return d != nil && d.active }

// HandleKey advances the selection or resolves the dialog.
func (d *telegramDialog) HandleKey(k tui.Key) telegramAction {
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
		return telegramAction{Close: true}
	case tui.KeyEnter:
		if len(d.items) == 0 {
			d.Close()
			return telegramAction{Close: true}
		}
		it := d.items[d.cursor]
		d.Close()
		return telegramAction{Select: true, Action: it.action}
	}
	return telegramAction{}
}

// Render returns the dialog lines.
func (d *telegramDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "telegram", width))
	lines = append(lines, th.FG256(th.Muted, "pick an action (\u2191/\u2193, enter, esc to cancel):"))
	for i, it := range d.items {
		plain := "  " + it.label
		if it.hint != "" {
			plain += "  " + th.FG256(th.Muted, "("+it.hint+")")
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
