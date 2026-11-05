package tui

import "strings"

// wipPhase groups consecutive draft turns, including their assistant and tool
// messages. The start timestamp distinguishes phases without intervening input.
type wipPhase struct {
	active bool
	since  string
}

func (p *wipPhase) transition(active bool, since string) []string {
	var labels []string
	changed := p.since != "" && since != "" && p.since != since
	if p.active && (!active || changed) {
		labels = append(labels, "wip end")
		p.active = false
	}
	if active && !p.active {
		labels = append(labels, "wip start")
		p.active = true
	}
	p.since = since
	return labels
}

func (v *View) appendWIPMarkers(lines []string, labels []string, width int) []string {
	for _, label := range labels {
		prefix := "──── " + label + " "
		prefixWidth := 6 + len(label)
		rule := strings.Repeat("─", max(0, width))
		if width >= prefixWidth {
			rule = prefix + strings.Repeat("─", width-prefixWidth)
		}
		lines = append(lines, v.Theme.FG256(v.Theme.Muted, rule), "")
	}
	return lines
}
