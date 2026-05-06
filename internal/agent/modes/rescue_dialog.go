package modes

import (
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/tui"
)

// rescueDialog offers a quick model swap when the active turn fails
// with a recoverable provider error (auth/rate/temporary). It looks
// and behaves like modelDialog: vertical list of available models,
// up/down to select, enter to retry, esc to cancel. The candidate
// list is built dynamically from the providers the user is currently
// logged in to — no static fallback config.
type rescueDialog struct {
	active   bool
	all      []provider.Model // candidates, sorted
	view     []provider.Model // filtered view (typed query)
	cursor   int
	current  string // currently active model id (excluded from view)
	query    string
	failedAt string // failed provider/model pair, e.g. "kimi/kimi-for-coding"
	reason   string // short human-readable reason ("token expired", "rate limited", ...)
	prompt   string // the user prompt that should be retried on Select
}

type rescueDialogAction struct {
	Select   bool
	Provider string
	Model    string
	Prompt   string
	Close    bool
}

func newRescueDialog() *rescueDialog { return &rescueDialog{} }

// Open shows the dialog. current is the currently active model id so
// it can be excluded from the candidate list. loggedInProviders is
// the set of provider names with usable credentials right now.
func (d *rescueDialog) Open(current string, loggedInProviders []string, failedProvider, failedModel, reason, prompt string) {
	d.active = true
	d.current = current
	d.query = ""
	d.reason = reason
	d.prompt = prompt
	d.failedAt = strings.TrimSpace(failedProvider + "/" + failedModel)

	all := provider.Active()
	provSet := map[string]bool{}
	for _, p := range loggedInProviders {
		provSet[p] = true
	}
	var filtered []provider.Model
	for _, m := range all {
		if !provSet[m.Provider] {
			continue
		}
		// Drop the exact failed pair so users can't retry on the
		// model that just failed.
		if m.Provider == failedProvider && m.ID == failedModel {
			continue
		}
		// Drop the currently-active model id (if it differs from
		// the failed one for some reason). The picker is meant to
		// help the user move *off* the broken pair.
		if m.ID == current {
			continue
		}
		filtered = append(filtered, m)
	}
	d.all = sortedModels(filtered)
	d.refilter()
}

func (d *rescueDialog) Close()       { d.active = false }
func (d *rescueDialog) Active() bool { return d != nil && d.active }

func (d *rescueDialog) refilter() {
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
}

// Render mirrors modelDialog so a rescue prompt feels identical to
// every other picker in the TUI.
func (d *rescueDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	header := "rescue turn"
	if d.failedAt != "" && d.failedAt != "/" {
		header = "rescue turn — " + d.failedAt + " failed"
	}
	lines = append(lines, frameHeader(th, header, width))

	if d.reason != "" {
		lines = append(lines, th.FG256(th.Warning, "  "+d.reason))
	}

	hint := "retry this turn with another model (↑/↓, enter, esc to cancel)"
	if d.query != "" {
		hint = fmt.Sprintf("filter: %s (%d match)", d.query, len(d.view))
		if len(d.view) != 1 {
			hint = fmt.Sprintf("filter: %s (%d matches)", d.query, len(d.view))
		}
	} else {
		hint += " - type to filter"
	}
	lines = append(lines, th.FG256(th.Muted, hint))

	if len(d.view) == 0 {
		if len(d.all) == 0 {
			lines = append(lines, th.FG256(th.Muted, "  no other models available — log in to another provider with /login"))
		} else {
			lines = append(lines, th.FG256(th.Muted, "  no models match "+fmt.Sprintf("%q", d.query)))
		}
		lines = append(lines, frameRule(th, width))
		return lines
	}

	const visible = 12
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
		reason := " "
		if m.Reasoning {
			reason = "✦"
		}
		tag := ""
		switch {
		case m.Speculative:
			tag = "[speculative] "
		case m.Source == "live":
			tag = "[live] "
		}
		plain := fmt.Sprintf("   %-10s %-28s %s  %s%s", m.Provider, m.ID, reason, tag, m.DisplayName)
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

func (d *rescueDialog) HandleKey(k tui.Key) rescueDialogAction {
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
			r := []rune(d.query)
			d.query = string(r[:len(r)-1])
			d.refilter()
		}
	case tui.KeyRune:
		if k.Alt || k.Ctrl {
			break
		}
		if k.Rune >= 0x20 && k.Rune < 0x7f {
			d.query += string(k.Rune)
			d.refilter()
		}
	case tui.KeyEsc:
		d.Close()
		return rescueDialogAction{Close: true}
	case tui.KeyEnter:
		if len(d.view) == 0 {
			d.Close()
			return rescueDialogAction{Close: true}
		}
		m := d.view[d.cursor]
		prompt := d.prompt
		d.Close()
		return rescueDialogAction{Select: true, Provider: m.Provider, Model: m.ID, Prompt: prompt}
	}
	return rescueDialogAction{}
}

// classifyRescueError inspects an agent error and decides whether
// it's a recoverable provider failure that the user should be offered
// a rescue picker for, plus a short human-readable reason. Returns
// (false, "") for errors we should NOT auto-rescue (bad request,
// context length, transcript serialization issues, etc.) so the
// regular red status banner still surfaces them.
func classifyRescueError(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	msg := err.Error()
	low := strings.ToLower(msg)

	// Don't trigger on payload-too-large; that path already has its
	// own auto-compact handling.
	if isPayloadTooLargeError(err) {
		return false, ""
	}

	// Network failures.
	if strings.Contains(low, "timeout") ||
		strings.Contains(low, "deadline exceeded") ||
		strings.Contains(low, "connection refused") ||
		strings.Contains(low, "connection reset") ||
		strings.Contains(low, "no such host") ||
		strings.Contains(low, "tls handshake") ||
		strings.Contains(low, "eof") {
		return true, "network failure: " + shortError(msg)
	}

	switch {
	case containsAny(low, "http 401", " 401:", "invalid_authentication", "token expired", "api key appears to be invalid"):
		return true, "authentication failed: " + shortError(msg)
	case containsAny(low, "http 403", " 403:", "permission denied", "forbidden"):
		return true, "permission denied: " + shortError(msg)
	case containsAny(low, "http 429", " 429:", "rate limit", "rate_limit", "too many requests", "quota"):
		return true, "rate limited: " + shortError(msg)
	case containsAny(low, "http 500", "http 502", "http 503", "http 504", " 500:", " 502:", " 503:", " 504:", "upstream connect error", "service unavailable", "internal server error", "bad gateway", "gateway timeout"):
		return true, "provider unavailable: " + shortError(msg)
	}

	// Anything else (400 bad request, validation errors, etc.) is
	// usually not fixed by switching models; let it surface as-is.
	return false, ""
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// shortError trims a long http-payload error to something readable in
// the rescue dialog header without dropping the most useful prefix.
func shortError(msg string) string {
	msg = strings.TrimSpace(msg)
	const max = 140
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}

// extractFailedProvider tries to pull the failing provider name out
// of an error string ("kimi: http 401: ...", "openai: http 503: ...").
// Returns "" when nothing recognisable is found.
func extractFailedProvider(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.Index(msg, ":"); i > 0 {
		head := strings.TrimSpace(msg[:i])
		switch head {
		case "anthropic", "openai", "openai-codex", "kimi", "ollama":
			return head
		}
	}
	return ""
}
