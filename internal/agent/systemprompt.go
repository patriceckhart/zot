package agent

import (
	"fmt"
	"strings"
	"time"
)

// ToolSummary is a name+one-line description. Kept as part of the
// public opts type for backwards compatibility with callers that
// still pass tool summaries in; the default prompt no longer lists
// them because the provider already advertises tools in the request
// body's tools[] array, so listing them again in prose is pure
// duplication.
type ToolSummary struct {
	Name        string
	Description string
}

// SystemPromptOpts configures BuildSystemPrompt.
type SystemPromptOpts struct {
	CWD    string
	Tools  []ToolSummary
	Custom string   // if set, replaces the default identity entirely
	Append []string // extra text appended at the end
	Now    time.Time
}

// BuildSystemPrompt constructs the system prompt.
//
// Design note: kept intentionally small. Every byte here is part of
// the cached prefix on every request, so bloat is cumulatively
// expensive. We ship only:
//
//   - A one-paragraph identity (who zot is, what the name means,
//     what the TUI expects for output format).
//   - The date + cwd footer so the model has current-context.
//
// Everything else (tool listing, operating guidelines, "don't run
// sudo", "prefer edit over write", etc.) is left out because the
// current-generation frontier models already internalise it, and
// the tool schemas sent alongside the request carry each tool's
// own description.
//
// Users who want extra biasing can use --system-prompt (replace),
// --append-system-prompt (additive, repeatable), or drop a
// SYSTEM.md in $ZOT_HOME that overrides the default identity.
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

const defaultIdentity = `You are an expert coding assistant operating inside zot, a coding agent harness. The name "zot" stands for "zero-overhead-tool"; if the user asks what zot means, answer exactly that.

Your output renders in a TUI that understands markdown for prose and plain text for tool output. Use markdown freely, keep answers concise, and let tool calls speak for themselves rather than narrating them in prose before you invoke them. Act first, then summarise what you did.`
