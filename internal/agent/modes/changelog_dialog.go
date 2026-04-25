package modes

import (
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/internal/tui"
)

// changelogDialog is the one-shot release-notes overlay shown the
// first time a user launches a new zot version. The body is the
// markdown from the GitHub release page, rendered through the same
// pipeline used for assistant messages so code fences + bold +
// links look right.
//
// Any key dismisses; the parent Interactive then persists the
// version-shown marker so the dialog never reappears for that
// version.
type changelogDialog struct {
	active  bool
	version string
	url     string
	body    string
	scroll  int
}

func newChangelogDialog() *changelogDialog { return &changelogDialog{} }

// Open populates and shows the dialog.
func (d *changelogDialog) Open(version, url, body string) {
	d.active = true
	d.version = version
	d.url = url
	d.body = strings.TrimSpace(body)
	d.scroll = 0
}

// Close hides the dialog.
func (d *changelogDialog) Close() { d.active = false }

// Active reports whether the overlay is visible and consuming keys.
func (d *changelogDialog) Active() bool { return d != nil && d.active }

// HandleKey: any key (other than scroll) closes the dialog. Returns
// closed=true when the user dismissed; the parent uses that as the
// signal to persist LastChangelogShown.
func (d *changelogDialog) HandleKey(k tui.Key) (closed bool) {
	if !d.Active() {
		return false
	}
	switch k.Kind {
	case tui.KeyUp:
		if d.scroll > 0 {
			d.scroll--
		}
		return false
	case tui.KeyDown:
		d.scroll++
		return false
	case tui.KeyPageUp:
		d.scroll -= 8
		if d.scroll < 0 {
			d.scroll = 0
		}
		return false
	case tui.KeyPageDown:
		d.scroll += 8
		return false
	}
	d.Close()
	return true
}

// Render returns the dialog lines.
func (d *changelogDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	title := fmt.Sprintf("zot %s \u2014 release notes (any key to dismiss)", d.version)
	out := []string{frameHeaderColor(th, title, width, th.Accent)}
	if d.url != "" {
		out = append(out, "  "+th.FG256(th.Muted, d.url))
		out = append(out, "")
	}

	var bodyLines []string
	for _, l := range strings.Split(d.body, "\n") {
		if strings.HasPrefix(l, "\x00H:") {
			// Heading: render in accent color, bold.
			heading := strings.TrimPrefix(l, "\x00H:")
			bodyLines = append(bodyLines, th.FG256(th.Accent, tui.Bold(heading)))
		} else {
			// Regular line: render through markdown for bullet points etc.
			rendered := tui.RenderMarkdown(l, th, width-4)
			for _, rl := range strings.Split(rendered, "\n") {
				bodyLines = append(bodyLines, rl)
			}
		}
	}

	const maxRows = 18
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
	out = append(out, frameRuleColor(th, width, th.Accent))
	return out
}
