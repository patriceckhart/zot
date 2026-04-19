package modes

import (
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/internal/skills"
	"github.com/patriceckhart/zot/internal/tui"
)

// skillsDialog lists every discovered skill and lets the user view
// the body of one inline. View is read-only — the model loads skills
// itself via the `skill` tool. This dialog is for inspection.
type skillsDialog struct {
	active  bool
	skills  []*skills.Skill
	cursor  int
	viewing *skills.Skill // when non-nil, render the body instead of the list
	scroll  int           // body view scroll offset (in lines)
}

func newSkillsDialog() *skillsDialog { return &skillsDialog{} }

// Open populates and shows the dialog with the given snapshot.
func (d *skillsDialog) Open(s []*skills.Skill) {
	d.active = true
	d.skills = s
	d.cursor = 0
	d.viewing = nil
	d.scroll = 0
}

// Close hides the dialog.
func (d *skillsDialog) Close() { d.active = false }

// Active reports whether the dialog is visible.
func (d *skillsDialog) Active() bool { return d != nil && d.active }

// HandleKey advances the dialog state.
func (d *skillsDialog) HandleKey(k tui.Key) (closed bool) {
	if !d.Active() {
		return false
	}

	if d.viewing != nil {
		// Body view keys.
		switch k.Kind {
		case tui.KeyEsc, tui.KeyEnter:
			d.viewing = nil
			d.scroll = 0
		case tui.KeyUp:
			if d.scroll > 0 {
				d.scroll--
			}
		case tui.KeyDown:
			d.scroll++
		case tui.KeyPageUp:
			d.scroll -= 8
			if d.scroll < 0 {
				d.scroll = 0
			}
		case tui.KeyPageDown:
			d.scroll += 8
		}
		return false
	}

	// List view keys.
	switch k.Kind {
	case tui.KeyEsc:
		d.Close()
		return true
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.skills)-1 {
			d.cursor++
		}
	case tui.KeyEnter:
		if len(d.skills) > 0 {
			d.viewing = d.skills[d.cursor]
			d.scroll = 0
		}
	}
	return false
}

// Render draws the picker or the body view.
func (d *skillsDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}

	if d.viewing != nil {
		return d.renderBody(th, width)
	}

	out := []string{frameHeader(th, "skills (enter to view, esc to close)", width)}
	if len(d.skills) == 0 {
		out = append(out, "  "+th.FG256(th.Muted, "no skills found"))
		out = append(out, "  "+th.FG256(th.Muted, "see docs/skills.md to add some"))
		out = append(out, frameRule(th, width))
		return out
	}

	const maxRows = 12
	start, end := visibleWindow(d.cursor, len(d.skills), maxRows)
	if start > 0 {
		out = append(out, "  "+th.FG256(th.Muted, fmt.Sprintf("\u2191 %d more above", start)))
	}
	for i := start; i < end; i++ {
		s := d.skills[i]
		row := formatSkillRow(s, width-2)
		if i == d.cursor {
			out = append(out, th.PadHighlight("  "+row, width))
		} else {
			out = append(out, "  "+th.FG256(th.Muted, row))
		}
	}
	if end < len(d.skills) {
		out = append(out, "  "+th.FG256(th.Muted, fmt.Sprintf("\u2193 %d more below", len(d.skills)-end)))
	}
	out = append(out, frameRule(th, width))
	return out
}

func (d *skillsDialog) renderBody(th tui.Theme, width int) []string {
	s := d.viewing
	out := []string{
		frameHeader(th, "skill: "+s.Name+"  (esc / enter to go back)", width),
		"  " + th.FG256(th.Muted, s.Description),
		"  " + th.FG256(th.Muted, "source: "+s.Source+"  ("+s.Path+")"),
		"",
	}

	rendered := tui.RenderMarkdown(s.Body, th, width-4)
	bodyLines := strings.Split(rendered, "\n")

	const maxRows = 16
	if d.scroll > len(bodyLines)-1 {
		d.scroll = len(bodyLines) - 1
	}
	if d.scroll < 0 {
		d.scroll = 0
	}
	end := d.scroll + maxRows
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	for _, line := range bodyLines[d.scroll:end] {
		out = append(out, "    "+line)
	}
	if end < len(bodyLines) {
		out = append(out, "  "+th.FG256(th.Muted, fmt.Sprintf("\u2193 %d more lines (down/pgdn)", len(bodyLines)-end)))
	}
	out = append(out, frameRule(th, width))
	return out
}

func formatSkillRow(s *skills.Skill, maxWidth int) string {
	left := fmt.Sprintf("%-20s  ", truncateLineSafe(s.Name, 20))
	src := "  " + truncateLineSafe(s.Source, 16)
	room := maxWidth - len(left) - len(src)
	if room < 10 {
		room = 10
	}
	desc := s.Description
	if len(desc) > room {
		desc = desc[:room-1] + "\u2026"
	}
	return left + desc + src
}

// truncateLineSafe limits s to n runes (not bytes) so multibyte
// names + sources don't blow past the column.
func truncateLineSafe(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "\u2026"
	}
	return string(r[:n-1]) + "\u2026"
}

// visibleWindow centers cursor in a window of size n within total
// items. Returns [start, end) bounds.
func visibleWindow(cursor, total, n int) (start, end int) {
	if total <= n {
		return 0, total
	}
	start = cursor - n/2
	if start < 0 {
		start = 0
	}
	end = start + n
	if end > total {
		end = total
		start = end - n
	}
	return
}
