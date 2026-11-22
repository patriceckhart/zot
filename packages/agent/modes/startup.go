package modes

import (
	"fmt"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/tui"
)

// finishStartup ends automatic classification of legacy notifications. Explicit
// startup notifications can still arrive later without interrupting the chat.
func (i *Interactive) finishStartup() {
	i.mu.Lock()
	i.startupComplete = true
	i.mu.Unlock()
}

func (i *Interactive) showStartupChangelog(cl ChangelogPayload) {
	i.mu.Lock()
	i.startupChangelog = cl
	i.mu.Unlock()
	i.invalidate()
	if i.cfg.OnChangelogDismiss != nil {
		i.cfg.OnChangelogDismiss()
	}
}

// startupBlockLocked builds scrolling display state, not model context or
// persisted messages. The caller holds mu, just like buildChatLocked.
func (i *Interactive) startupBlockLocked(cols int) []string {
	showVersion := !i.welcomeStart.IsZero() && time.Since(i.welcomeStart) < welcomeVersionDuration
	out := welcomeBanner(i.cfg.Theme, i.cfg.Version, showVersion)
	out = append(out, renderUpdateBanner(i.cfg.Theme, i.updateInfo, cols)...)
	cl := i.startupChangelog
	if cl.Body != "" {
		title := fmt.Sprintf("zot %s release notes", cl.Version)
		out = append(out, frameHeaderColor(i.cfg.Theme, title, cols, i.cfg.Theme.Accent), "")
		if cl.URL != "" {
			out = append(out, wrapDialogTextRows(i.cfg.Theme.FG256(i.cfg.Theme.Muted, cl.URL), cols)...)
		}
		out = append(out, renderStartupChangelogBody(cl.Body, i.cfg.Theme, cols)...)
		out = append(out, "", frameRuleColor(i.cfg.Theme, cols, i.cfg.Theme.Accent))
		out = append(out, "")
	}
	out = append(out, i.startupNotes...)
	if len(i.startupNotes) > 0 {
		out = append(out, "")
	}
	return out
}

func renderStartupChangelogBody(body string, th tui.Theme, cols int) []string {
	// Release aggregation uses internal heading markers. Render markdown in
	// whole sections so code fences and lists retain their structure.
	var out []string
	var section []string
	flush := func() {
		if len(section) > 0 {
			out = append(out, renderDialogMarkdownRows(strings.Join(section, "\n"), th, cols)...)
			section = nil
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if heading, ok := strings.CutPrefix(line, "\x00H:"); ok {
			flush()
			out = append(out, wrapDialogTextRows(th.FG256(th.Accent, tui.Bold(heading)), cols)...)
		} else {
			section = append(section, line)
		}
	}
	flush()
	return out
}
