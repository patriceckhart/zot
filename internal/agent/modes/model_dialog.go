package modes

import (
	"fmt"
	"sort"
	"strings"

	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/tui"
)

// modelDialog is an inline picker for choosing the active model.
// It lists all models known to the provider package (baked-in catalog
// + any live entries discovered via /v1/models) sorted by provider
// then model id, and lets the user pick one with arrow keys + enter.
// Typing characters narrows the list via a fuzzy substring match that
// ignores punctuation (e.g. "opus46" matches "claude-opus-4-6").
type modelDialog struct {
	active  bool
	all     []provider.Model // full catalog, sorted
	view    []provider.Model // filtered view shown to the user
	cursor  int
	current string // currently selected model id (highlighted)
	query   string // live filter text typed by the user
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
	d.all = sortedModels(provider.Active())
	d.current = current
	d.query = ""
	d.refilter()
}

// Close hides the dialog.
func (d *modelDialog) Close() { d.active = false }

// Active reports whether the dialog is visible and consumes input.
func (d *modelDialog) Active() bool { return d != nil && d.active }

// refilter rebuilds view from all according to query, and snaps the
// cursor to either the current model (if visible) or the first row.
func (d *modelDialog) refilter() {
	needle := normalizeModelQuery(d.query)
	if needle == "" {
		d.view = append([]provider.Model(nil), d.all...)
	} else {
		out := make([]provider.Model, 0, len(d.all))
		for _, m := range d.all {
			if strings.Contains(normalizeModelQuery(m.Provider+" "+m.ID+" "+m.DisplayName), needle) {
				out = append(out, m)
			}
		}
		d.view = out
	}
	d.cursor = 0
	for i, m := range d.view {
		if m.ID == d.current {
			d.cursor = i
			break
		}
	}
}

// sortedModels returns a fresh slice sorted by provider, then model id.
func sortedModels(in []provider.Model) []provider.Model {
	out := append([]provider.Model(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// normalizeModelQuery lowercases and strips punctuation so fuzzy
// substring matching works on both the query and haystacks. "opus46"
// and "opus-4-6" both become "opus46".
func normalizeModelQuery(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			sb.WriteRune(r + ('a' - 'A'))
		}
	}
	return sb.String()
}

// Render returns the dialog lines.
func (d *modelDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "model", width))

	hint := "pick a model (↑/↓, enter, esc to cancel)"
	if d.query != "" {
		hint = fmt.Sprintf("filter: %s (%d match)", d.query, len(d.view))
		if len(d.view) != 1 {
			hint = fmt.Sprintf("filter: %s (%d matches)", d.query, len(d.view))
		}
	} else {
		hint += " · type to filter"
	}
	lines = append(lines, th.FG256(th.Muted, hint))

	if len(d.view) == 0 {
		lines = append(lines, th.FG256(th.Muted, "  no models match "+fmt.Sprintf("%q", d.query)))
		lines = append(lines, frameRule(th, width))
		return lines
	}

	// Scroll window so very tall catalogs still fit in a short tui.
	const visible = 14
	start := 0
	end := len(d.view)
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
		m := d.view[i]
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
	if end < len(d.view) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("   … %d more below", len(d.view)-end)))
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
		if d.cursor < len(d.view)-1 {
			d.cursor++
		}
	case tui.KeyBackspace:
		if len(d.query) > 0 {
			// Drop one rune from the query.
			r := []rune(d.query)
			d.query = string(r[:len(r)-1])
			d.refilter()
		}
	case tui.KeyRune:
		if k.Alt || k.Ctrl {
			break
		}
		// Only printable ASCII is useful for narrowing.
		if k.Rune >= 0x20 && k.Rune < 0x7f {
			d.query += string(k.Rune)
			d.refilter()
		}
	case tui.KeyEsc:
		d.Close()
		return modelDialogAction{Close: true}
	case tui.KeyEnter:
		if len(d.view) == 0 {
			d.Close()
			return modelDialogAction{Close: true}
		}
		m := d.view[d.cursor]
		d.Close()
		return modelDialogAction{Select: true, Provider: m.Provider, Model: m.ID}
	}
	return modelDialogAction{}
}
