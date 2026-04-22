package modes

import (
	"strings"

	"github.com/patriceckhart/zot/internal/tui"
)

type extPanelDialog struct {
	active bool
	ext    string
	id     string
	title  string
	lines  []string
	footer string
}

func newExtPanelDialog() *extPanelDialog { return &extPanelDialog{} }

func (d *extPanelDialog) Active() bool { return d != nil && d.active }

func (d *extPanelDialog) Open(ext, id, title string, lines []string, footer string) {
	d.active = true
	d.ext = ext
	d.id = id
	d.title = title
	d.lines = append([]string(nil), lines...)
	d.footer = footer
}

func (d *extPanelDialog) Update(title string, lines []string, footer string) {
	if !d.active {
		return
	}
	if title != "" {
		d.title = title
	}
	d.lines = append(d.lines[:0], lines...)
	d.footer = footer
}

func (d *extPanelDialog) Close() {
	d.active = false
	d.ext = ""
	d.id = ""
	d.title = ""
	d.lines = nil
	d.footer = ""
}

func (d *extPanelDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	title := d.title
	if title == "" {
		title = d.ext
	}
	out := []string{frameHeader(th, title, width)}
	for _, l := range d.lines {
		out = append(out, l)
	}
	if strings.TrimSpace(d.footer) != "" {
		out = append(out, "")
		out = append(out, th.FG256(th.Muted, d.footer))
	}
	out = append(out, frameRule(th, width))
	return out
}
