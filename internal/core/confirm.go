package core

import (
	"encoding/json"
	"strings"
	"sync"
)

// ConfirmDecision is the outcome of a confirmation prompt for a
// tool call. It answers two questions: should this call run, and
// should future calls of this shape skip the prompt for the rest
// of the session.
type ConfirmDecision struct {
	Allow bool
	// Reason is shown to the model as the tool error when
	// Allow=false. Examples: "user declined", "user refused: rm -rf
	// looks dangerous".
	Reason string
	// RememberTool, when true, auto-allows every future call of the
	// same tool name for the rest of the session without prompting.
	RememberTool bool
	// RememberAll, when true, auto-allows every future call of any
	// tool for the rest of the session without prompting.
	// Effectively turns yolo back on for this session.
	RememberAll bool
}

// Confirmer asks the user to approve or refuse a single tool call.
// Implementations block until the user responds (or the agent's
// context is cancelled, in which case they should return
// Allow=false with a cancellation reason).
//
// preview is a short one-line summary of the args (the shell
// command, the file path, the URL) that the TUI shows alongside
// the tool name. It is intentionally short: no full tool outputs.
type Confirmer interface {
	Confirm(toolName string, preview string) ConfirmDecision
}

// ConfirmGate wraps a Confirmer with session-scoped memory for the
// "allow, always" decisions. Once the user picks RememberTool on a
// given tool name, the gate short-circuits subsequent calls of that
// name. Once the user picks RememberAll, the gate short-circuits
// everything for the rest of the session.
//
// Safe for concurrent use: the allow-lists are guarded by a mutex
// because the agent can queue tool calls from different goroutines.
type ConfirmGate struct {
	inner Confirmer

	mu          sync.Mutex
	allowAll    bool
	allowedTool map[string]bool
}

// NewConfirmGate returns a gate backed by inner. When inner is nil,
// the gate operates in auto-refuse mode: every tool call is denied
// with a fixed reason. That's what non-interactive modes (-p /
// --json / rpc) use when --no-yolo is on, since they have no way
// to prompt the user.
func NewConfirmGate(inner Confirmer) *ConfirmGate {
	return &ConfirmGate{
		inner:       inner,
		allowedTool: map[string]bool{},
	}
}

// Check is the BeforeToolExecute-style entry point. Returns
// allowed, reason, modifiedArgs. modifiedArgs is always nil: the
// gate never rewrites args; it only allows or denies.
//
// A nil ConfirmGate always allows (treat as yolo mode).
func (g *ConfirmGate) Check(toolName, preview string) (bool, string, json.RawMessage) {
	if g == nil {
		return true, "", nil
	}
	g.mu.Lock()
	if g.allowAll {
		g.mu.Unlock()
		return true, "", nil
	}
	if g.allowedTool[toolName] {
		g.mu.Unlock()
		return true, "", nil
	}
	g.mu.Unlock()

	g.mu.Lock()
	inner := g.inner
	g.mu.Unlock()
	if inner == nil {
		return false, "tool call refused: --no-yolo is active and there is no interactive prompt in this mode; ask the user what to do instead", nil
	}

	decision := inner.Confirm(toolName, preview)

	g.mu.Lock()
	if decision.Allow {
		if decision.RememberAll {
			g.allowAll = true
		}
		if decision.RememberTool {
			g.allowedTool[toolName] = true
		}
	}
	g.mu.Unlock()

	reason := strings.TrimSpace(decision.Reason)
	if !decision.Allow && reason == "" {
		reason = "tool call refused by user"
	}
	return decision.Allow, reason, nil
}

// Reset clears the session memory. Invoked when the user toggles
// yolo mode back on via /yolo or closes the session.
func (g *ConfirmGate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.allowAll = false
	g.allowedTool = map[string]bool{}
	g.mu.Unlock()
}

// AllowAll flips the gate into "always allow" for the rest of the
// session. Used by /yolo on to turn confirmation off at runtime
// without having to restart with the flag flipped.
func (g *ConfirmGate) AllowAll() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.allowAll = true
	g.mu.Unlock()
}

// SetConfirmer replaces the inner Confirmer. Used by the cli to
// hand the Interactive TUI to a gate that was constructed before
// the TUI existed (a construction-order knot we unwind here).
// Safe to call from any goroutine.
func (g *ConfirmGate) SetConfirmer(c Confirmer) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.inner = c
	g.mu.Unlock()
}

// BuildPreview turns a tool call's JSON args into a short one-line
// summary the TUI can show in the confirmation prompt. Prioritises
// obvious human-readable fields (command, path, url) over raw JSON.
// Returns at most maxLen characters.
func BuildPreview(args json.RawMessage, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 120
	}
	if len(args) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return truncatePreview(string(args), maxLen)
	}
	for _, k := range []string{"command", "path", "file_path", "url", "query", "name"} {
		if v, ok := m[k].(string); ok && v != "" {
			return truncatePreview(v, maxLen)
		}
	}
	b, _ := json.Marshal(m)
	return truncatePreview(string(b), maxLen)
}

func truncatePreview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "\u2026"
}
