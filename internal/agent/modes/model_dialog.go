package modes

import (
	"fmt"

	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/tui"
)

// modelDialog is an inline picker for choosing the active model.
// It lists all models known to the provider package (baked-in catalog
// + any live entries discovered via /v1/models) and lets the user pick
// one with arrow keys + enter.
type modelDialog struct {
	active  bool
	models  []provider.Model
	cursor  int
	current string // currently selected model id (highlighted)
}

// modelDialogAction is returned by HandleKey.
type modelDialogAction struct {
	Select   bool
	Provider string
	Model    string
	Close    bool
}

func newModelDialog() *modelDialog {
	return &modelDialog{}
}

// Open shows the dialog. current is the currently active model id so
// it can be pre-selected.
func (d *modelDialog) Open(current string) {
	d.active = true
	d.models = provider.Active()
	d.current = current
	d.cursor = 0
	for i, m := range d.models {
		if m.ID == current {
			d.cursor = i
			break
		}
	}
}

// Close hides the dialog.
func (d *modelDialog) Close() { d.active = false }

// Active reports whether the dialog is visible and consumes input.
func (d *modelDialog) Active() bool { return d != nil && d.active }

// Render returns the dialog lines.
func (d *modelDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "model", width))
	lines = append(lines, th.FG256(th.Muted, "pick a model (↑/↓, enter, esc to cancel)"))

	// Simple scroll window so very tall catalogs still fit in a short tui.
	const visible = 14
	start := 0
	end := len(d.models)
	if end > visible {
		start = d.cursor - visible/2
		if start < 0 {
			start = 0
		}
		if start+visible > end {
			start = end - visible
		}
		end = start + visible
	}

	for i := start; i < end; i++ {
		m := d.models[i]
		prov := m.Provider
		id := m.ID
		reason := " "
		if m.Reasoning {
			reason = "✦"
		}
		name := m.DisplayName
		tag := ""
		switch {
		case m.Speculative:
			tag = "[speculative] "
		case m.Source == "live":
			tag = "[live] "
		}
		curMark := "  "
		if m.ID == d.current {
			curMark = "● "
		}
		plain := fmt.Sprintf(" %s%-10s %-28s %s  %s%s", curMark, prov, id, reason, tag, name)
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}

	if start > 0 {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("   … %d more above", start)))
	}
	if end < len(d.models) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("   … %d more below", len(d.models)-end)))
	}

	lines = append(lines, frameRule(th, width))
	return lines
}

// HandleKey advances the dialog and returns an action to apply, if any.
func (d *modelDialog) HandleKey(k tui.Key) modelDialogAction {
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.models)-1 {
			d.cursor++
		}
	case tui.KeyEsc:
		d.Close()
		return modelDialogAction{Close: true}
	case tui.KeyEnter:
		if len(d.models) == 0 {
			d.Close()
			return modelDialogAction{Close: true}
		}
		m := d.models[d.cursor]
		d.Close()
		return modelDialogAction{Select: true, Provider: m.Provider, Model: m.ID}
	}
	return modelDialogAction{}
}
