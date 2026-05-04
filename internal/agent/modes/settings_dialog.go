package modes

import (
	"github.com/patriceckhart/zot/internal/tui"
)

type settingsDialog struct {
	active bool
	items  []settingsItem
	cursor int
}

type settingsItem struct {
	key      string
	label    string
	desc     string
	value    bool
	disabled bool
	hint     string
}

type settingsAction struct {
	Toggle bool
	Key    string
	Value  bool
	Close  bool
}

func newSettingsDialog() *settingsDialog { return &settingsDialog{} }

func (d *settingsDialog) Open(items []settingsItem) bool {
	if len(items) == 0 {
		return false
	}
	d.items = items
	d.cursor = 0
	d.active = true
	return true
}

func (d *settingsDialog) Close()       { d.active = false }
func (d *settingsDialog) Active() bool { return d != nil && d.active }

func (d *settingsDialog) HandleKey(k tui.Key) settingsAction {
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
		return settingsAction{Close: true}
	case tui.KeyEnter:
		return d.toggleCurrent()
	case tui.KeyRune:
		if k.Rune == ' ' {
			return d.toggleCurrent()
		}
	}
	return settingsAction{}
}

func (d *settingsDialog) toggleCurrent() settingsAction {
	if len(d.items) == 0 {
		d.Close()
		return settingsAction{Close: true}
	}
	it := d.items[d.cursor]
	if it.disabled {
		return settingsAction{}
	}
	it.value = !it.value
	d.items[d.cursor] = it
	return settingsAction{Toggle: true, Key: it.key, Value: it.value}
}

func (d *settingsDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "settings", width))
	lines = append(lines, th.FG256(th.Muted, "toggle with enter/space, esc to close:"))
	for i, it := range d.items {
		box := "[ ]"
		if it.value {
			box = "[x]"
		}
		plain := "  " + box + " " + it.label
		if it.hint != "" {
			plain += "  " + th.FG256(th.Muted, "("+it.hint+")")
		}
		if it.disabled {
			lines = append(lines, th.FG256(th.Muted, plain))
		} else if i == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, plain)
		}
		if it.desc != "" {
			lines = append(lines, th.FG256(th.Muted, "      "+it.desc))
		}
	}
	lines = append(lines, frameRule(th, width))
	return lines
}
