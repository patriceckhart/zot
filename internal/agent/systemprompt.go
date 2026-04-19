package agent

import (
	"fmt"
	"strings"
	"time"
)

// ToolSummary is a name+one-line description. Kept for backwards
// compatibility with callers that still pass tool summaries in; the
// built-in system prompt no longer lists tools by name, since the
// tool schemas themselves already reach the model.
type ToolSummary struct {
	Name        string
	Description string
}

// SystemPromptOpts configures BuildSystemPrompt.
type SystemPromptOpts struct {
	CWD    string
	Tools  []ToolSummary
	Custom string   // if set, replaces the default identity/guidelines sections
	Append []string // extra text appended at the end
	Now    time.Time
}

// BuildSystemPrompt constructs the system prompt.
//
// Design note: the prompt is intentionally tiny. Every byte here
// is re-sent on every request (cached after the first, but still
// counts toward cache-write on turn 1 and live context throughout).
// We avoid:
//
//   - Listing the tool names and descriptions (the provider sends
//     the tool schemas separately; duplicating them costs tokens
//     for zero benefit, the model already sees the tools).
//   - Repeating generic coding-assistant advice the frontier models
//     already internalise ("always read before editing", "prefer
//     minimal diffs", "don't apologize"). These were free tokens
//     on older models; they are pure overhead now.
//
// Anything the user explicitly needs can still be added via
// --system-prompt, --append-system-prompt, or $ZOT_HOME/SYSTEM.md.
func BuildSystemPrompt(o SystemPromptOpts) string {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	date := o.Now.Format("2006-01-02")
	cwd := o.CWD
	if cwd == "" {
		cwd = "."
	}

	var sb strings.Builder

	if o.Custom != "" {
		sb.WriteString(o.Custom)
	} else {
		sb.WriteString(defaultIdentity)
	}

	for _, a := range o.Append {
		if strings.TrimSpace(a) == "" {
			continue
		}
		sb.WriteString("\n\n")
		sb.WriteString(a)
	}

	fmt.Fprintf(&sb, "\n\nCurrent date: %s\nCurrent working directory: %s\n", date, cwd)
	return sb.String()
}

const defaultIdentity = `You are zot, a lightweight terminal coding agent. Be concise, act on the user's request directly, and reply with a short summary when done.`
