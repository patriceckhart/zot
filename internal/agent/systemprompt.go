package agent

import (
	"fmt"
	"strings"
	"time"
)

// ToolSummary is a name+one-line description, used when rendering the
// "available tools" section of the system prompt.
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
		sb.WriteString("\n\n")
		sb.WriteString(renderToolsSection(o.Tools))
		sb.WriteString("\n")
		sb.WriteString(defaultGuidelines)
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

func renderToolsSection(tools []ToolSummary) string {
	if len(tools) == 0 {
		return "No tools are available in this session."
	}
	var sb strings.Builder
	sb.WriteString("You have the following tools available:\n")
	for _, t := range tools {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name, t.Description)
	}
	return sb.String()
}

const defaultIdentity = `You are zot, a lightweight terminal coding assistant.
You help a developer by reading files, writing files, editing files, and running shell commands.
You are concise. You explain your plan briefly, then act. You do not apologize or hedge.`

const defaultGuidelines = `Operating guidelines:
- Prefer "edit" over "write" for existing files. Always read a file before editing it.
- Before running "bash", explain what the command will do in one short sentence.
- Avoid destructive commands (rm -rf, dropping tables, force-pushing, etc.) unless the user explicitly asks.
- Keep shell commands non-interactive (pass -y / --yes where needed; pipe "yes" if required).
- When unsure about a file's contents or structure, read it first rather than guess.
- When you are done, reply with a short summary of what you changed and any commands the user should run.`
