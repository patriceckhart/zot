package modes

import (
	"context"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

// rescueDialog offers a retry or model swap after a recoverable provider
// failure. Cursor zero retries the existing client. Remaining rows select
// models from providers with usable credentials.
type rescueDialog struct {
	catalog  func() []provider.Model
	active   bool
	all      []provider.Model // candidates, sorted
	view     []provider.Model // filtered view (typed query)
	cursor   int              // zero is retry, model rows are indexed from one
	current  string           // currently active model id (excluded from view)
	query    string
	failedAt string // failed provider/model pair, e.g. "kimi/kimi-for-coding"
	reason   string // short human-readable reason ("token expired", "rate limited", ...)
	prompt   string // the user prompt that should be retried on Select
}

type rescueDialogAction struct {
	Retry    bool
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
	if d.catalog != nil {
		all = d.catalog()
	}
	provSet := map[string]bool{}
	for _, p := range loggedInProviders {
		provSet[p] = true
	}
	var filtered []provider.Model
	for _, m := range all {
		if !provSet[m.Provider] {
			continue
		}
		// The failed pair has its own retry action, not a model-swap row.
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
	if needle != "" && len(d.view) > 0 {
		d.cursor = 1
	}
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

	hint := "retry or choose another model (↑/↓, enter, esc to cancel)"
	if d.query != "" {
		hint = fmt.Sprintf("filter: %s (%d match)", d.query, len(d.view))
		if len(d.view) != 1 {
			hint = fmt.Sprintf("filter: %s (%d matches)", d.query, len(d.view))
		}
	} else {
		hint += " - type to filter"
	}
	lines = append(lines, th.FG256(th.Muted, hint))

	retry := "   Retry current model (keep credentials and endpoint)"
	if d.cursor == 0 {
		lines = append(lines, th.PadHighlight(retry, width))
	} else {
		lines = append(lines, th.FG256(th.Muted, retry))
	}

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
		start = d.cursor - 1 - visible/2
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
		if i+1 == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}
	if start > 0 {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("   ... %d more above", start)))
	}
	if end < len(d.view) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("   ... %d more below", len(d.view)-end)))
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
		if d.cursor < len(d.view) {
			d.cursor++
		}
	case tui.KeyBackspace:
		if len(d.query) > 0 {
			r := []rune(d.query)
			d.query = string(r[:len(r)-1])
			d.refilter()
		}
	case tui.KeyPaste:
		d.query += singleLinePaste(k.Paste)
		d.refilter()
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
		if d.cursor == 0 {
			d.Close()
			return rescueDialogAction{Retry: true}
		}
		if d.cursor > len(d.view) {
			d.Close()
			return rescueDialogAction{Close: true}
		}
		m := d.view[d.cursor-1]
		prompt := d.prompt
		d.Close()
		return rescueDialogAction{Select: true, Provider: m.Provider, Model: m.ID, Prompt: prompt}
	}
	return rescueDialogAction{}
}

// retryRescueTurn retains the active client, including launch-time auth and
// endpoint overrides. The original input and completed tool results are already
// in the transcript, so continuing must not resubmit the saved prompt or images.
func (i *Interactive) retryRescueTurn() {
	i.mu.Lock()
	i.pendingRescuePrompt = ""
	i.pendingRescueImages = nil
	i.mu.Unlock()
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	i.startTurnRun(parent, "", nil, false, nil, false, true)
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

	// Don't trigger on context overflow; that path already has its own
	// auto-compact handling.
	if isContextOverflowError(err) {
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
	return msg[:max] + "..."
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
		case "anthropic", "openai", "openai-codex", "kimi", "google", "ollama":
			return head
		}
	}
	return ""
}
