package modes

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/tui"
)

// confirmRequest is one pending confirmation. The agent goroutine
// writes it onto the interactive's queue, the TUI renders the
// dialog, and the user's answer goes back through `resp`.
type confirmRequest struct {
	toolName string
	preview  string
	resp     chan core.ConfirmDecision
}

// confirmDialog is the inline prompt shown before every tool call
// when --no-yolo is on. It presents four options and routes the
// answer back through the pending request's response channel.
type confirmDialog struct {
	mu      sync.Mutex
	pending []*confirmRequest
	cursor  int
	// activeSince timestamps when the current request started
	// showing. Used only for debug/future UX (e.g. flashing the
	// dialog border after N seconds of no answer).
	activeSince time.Time
}

func newConfirmDialog() *confirmDialog { return &confirmDialog{} }

// Enqueue adds a pending request. Multiple requests can queue up
// if the agent produces tool calls in parallel (via extensions);
// the TUI shows them one at a time, in order.
func (d *confirmDialog) Enqueue(req *confirmRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = append(d.pending, req)
	if len(d.pending) == 1 {
		d.cursor = 0
		d.activeSince = time.Now()
	}
}

// Active reports whether the dialog is consuming input (there's at
// least one pending request).
func (d *confirmDialog) Active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending) > 0
}

// CancelAll refuses every pending request (used when the user
// cancels the active turn or closes the session while the dialog
// is open). Safe to call when idle.
func (d *confirmDialog) CancelAll(reason string) {
	d.mu.Lock()
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	for _, req := range pending {
		select {
		case req.resp <- core.ConfirmDecision{Allow: false, Reason: reason}:
		default:
		}
	}
}

// AllowAllPending approves every pending request and drains the
// queue. Used by /yolo so any confirmation dialog already on screen
// resolves immediately as "yes".
func (d *confirmDialog) AllowAllPending() {
	d.mu.Lock()
	pending := d.pending
	d.pending = nil
	d.mu.Unlock()
	for _, req := range pending {
		select {
		case req.resp <- core.ConfirmDecision{Allow: true, RememberAll: true}:
		default:
		}
	}
}

// confirmOptions lists the four responses in vertical order. Keyed
// by index so HandleKey can send the right decision.
var confirmOptions = []struct {
	label    string
	decision core.ConfirmDecision
}{
	{
		label:    "yes  (run this call)",
		decision: core.ConfirmDecision{Allow: true},
	},
	{
		label:    "yes, always this tool  (skip prompts for this tool for the rest of the session)",
		decision: core.ConfirmDecision{Allow: true, RememberTool: true},
	},
	{
		label:    "yes, always  (skip all prompts for the rest of the session)",
		decision: core.ConfirmDecision{Allow: true, RememberAll: true},
	},
	{
		label:    "no  (refuse and let the model try something else)",
		decision: core.ConfirmDecision{Allow: false, Reason: "user declined"},
	},
}

// HandleKey advances the selection or resolves the dialog. Returns
// true when a decision was sent back to the agent (the dialog may
// remain Active if there are more queued requests behind it).
func (d *confirmDialog) HandleKey(k tui.Key) bool {
	d.mu.Lock()
	if len(d.pending) == 0 {
		d.mu.Unlock()
		return false
	}
	req := d.pending[0]

	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
		d.mu.Unlock()
		return false
	case tui.KeyDown:
		if d.cursor < len(confirmOptions)-1 {
			d.cursor++
		}
		d.mu.Unlock()
		return false
	case tui.KeyEsc, tui.KeyCtrlC:
		// Treat esc/ctrl+c as "no, refuse this tool call". The
		// active turn can separately be cancelled by the outer esc
		// handler, but for the dialog itself, we must always answer
		// so the agent goroutine unblocks.
		d.pending = d.pending[1:]
		if len(d.pending) > 0 {
			d.cursor = 0
			d.activeSince = time.Now()
		}
		d.mu.Unlock()
		req.resp <- core.ConfirmDecision{Allow: false, Reason: "user cancelled"}
		return true
	case tui.KeyEnter:
		selected := confirmOptions[d.cursor].decision
		d.pending = d.pending[1:]
		if len(d.pending) > 0 {
			d.cursor = 0
			d.activeSince = time.Now()
		}
		d.mu.Unlock()
		req.resp <- selected
		return true
	}

	// Numeric shortcuts 1..4
	if k.Kind == tui.KeyRune && k.Rune >= '1' && k.Rune <= '4' {
		idx := int(k.Rune - '1')
		if idx >= 0 && idx < len(confirmOptions) {
			selected := confirmOptions[idx].decision
			d.pending = d.pending[1:]
			if len(d.pending) > 0 {
				d.cursor = 0
				d.activeSince = time.Now()
			}
			d.mu.Unlock()
			req.resp <- selected
			return true
		}
	}

	d.mu.Unlock()
	return false
}

// Render draws the dialog for the head-of-queue request.
func (d *confirmDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) == 0 {
		return nil
	}
	req := d.pending[0]
	cursor := d.cursor
	queued := len(d.pending) - 1

	var lines []string
	title := "confirm tool call"
	if queued > 0 {
		title = fmt.Sprintf("confirm tool call  (%d more queued)", queued)
	}
	lines = append(lines, frameHeader(th, title, width))

	toolLine := "  " + th.FG256(th.Tool, req.toolName)
	if req.preview != "" {
		toolLine += "  " + th.FG256(th.Muted, req.preview)
	}
	lines = append(lines, toolLine)
	lines = append(lines, "")
	lines = append(lines, th.FG256(th.Muted, "choose (\u2191/\u2193 or 1-4, enter to pick, esc to refuse):"))

	for i, opt := range confirmOptions {
		label := opt.label
		prefix := fmt.Sprintf("  %d. ", i+1)
		plain := prefix + label
		// Truncate the tail if the line would exceed width; keeps
		// the option numbers always visible.
		if visibleLen(plain) > width-2 {
			plain = plain[:width-3] + "\u2026"
		}
		if i == cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}
	lines = append(lines, frameRule(th, width))
	return lines
}

// visibleLen is a cheap char-count; ANSI codes aren't in the raw
// text at this point so len() is fine.
func visibleLen(s string) int { return strings.Count(s, "") - 1 }
