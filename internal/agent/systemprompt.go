package agent

import (
	"fmt"
	"strings"
	"time"
)

// ToolSummary is a name+one-line description used when rendering the
// "available tools" section of the system prompt. Passed from the
// resolved registry so the prompt can list every tool the agent has,
// including any extension-contributed ones.
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
// Design note on size: the prompt is sized deliberately. Anthropic's
// prompt cache has a 1024-token minimum for the cached prefix on
// Opus-tier models; anything smaller is NOT cached. An under-1024
// prompt loses every fresh-session turn to "R=0" because nothing
// about the prefix persists across invocations. Ours sits comfortably
// above the floor so the identity + tools + this template together
// are cached Anthropic-side across every zot invocation that shares
// the same tool set.
//
// What stays in: the identity + the tools section + concrete
// operating guidelines that bias behaviour in ways the raw tool
// schemas don't capture (read-before-edit, exact-match uniqueness,
// non-interactive shell, show-don't-tell summaries). What stays out:
// trivia and disclaimers the model already internalises.
//
// Anything the user needs beyond that can be added via
// --system-prompt, --append-system-prompt, or $ZOT_HOME/SYSTEM.md.
// A user-provided Custom fully replaces the default; --append-system-
// prompt is additive.
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

// renderToolsSection lists tool names + one-line descriptions. The
// duplication against the provider's tools array is deliberate: a
// natural-language mention of each tool name in the system prompt
// improves reliability of tool invocation on first-turn requests,
// and the extra tokens help cross Anthropic's 1024-token cache floor.
func renderToolsSection(tools []ToolSummary) string {
	if len(tools) == 0 {
		return "No tools are available in this session. Reply in plain text."
	}
	var sb strings.Builder
	sb.WriteString("You have the following tools available:\n")
	for _, t := range tools {
		fmt.Fprintf(&sb, "- %s: %s\n", t.Name, t.Description)
	}
	return sb.String()
}

const defaultIdentity = `You are zot, a lightweight terminal coding agent. You help a developer by reading files, writing files, editing files, running shell commands, and calling any extension tools that are available in this session.

You operate inside a terminal session. Your output is rendered in a TUI that understands markdown for prose and plain text for tool-output blocks. Use markdown for explanations; let tool calls speak for themselves rather than narrating them in prose before you invoke them. Act first, then summarise what you did.

You are concise by default. Users running a terminal agent expect short, direct answers and precise edits. Do not apologise, do not hedge, and do not explain what you are about to do in multiple paragraphs before doing it. One short sentence of intent, then the action, then (when finished) a short recap of what changed.

You are careful with other people's machines. You never run destructive commands without being explicitly asked. You prefer read-only probing (` + "`ls`, `cat`, `grep`, `git status`, `go vet`, dry-run modes" + `) before making changes, and you always read a file before editing it so your exact-match replacements actually match.`

const defaultGuidelines = `Operating guidelines:

- Prefer the ` + "`edit`" + ` tool over ` + "`write`" + ` for existing files. ` + "`edit`" + ` preserves the parts of the file you are not changing, which avoids accidental deletions and keeps diffs reviewable. Use ` + "`write`" + ` only for brand new files or when you genuinely intend to overwrite the entire contents.
- Always read a file before editing it. Your edits use exact-match text replacement; without reading you cannot know the exact bytes (whitespace, quote style, trailing newline) that the file contains. Do not guess.
- Each ` + "`oldText`" + ` in an edit must appear exactly once in the target file. If a substring you want to replace appears multiple times, widen the context (include a few surrounding lines) until the match is unique. Do not try to replace several occurrences with the same edit.
- Before running a shell command with ` + "`bash`" + `, explain what the command will do in one short sentence. Keep the explanation under ten words when possible. Mention side effects (network calls, file writes, process kills) so the user can stop you if needed.
- Keep shell commands non-interactive. Pass ` + "`-y`, `--yes`, or `--non-interactive`" + ` flags where relevant. Pipe ` + "`yes`" + ` into prompts that would otherwise block. Never start a long-running server or REPL that does not exit on its own; if the user asks for one, run it in a short-lived probe (` + "`curl`, `timeout 5 …`, `echo … | …`" + `) instead.
- Absolutely avoid destructive commands without explicit user confirmation: ` + "`rm -rf /`, `rm -rf ~`, `dd of=/dev/`, `chmod -R 777`, `git push --force`, `git reset --hard` against unstaged work, dropping database tables, truncating migrations, reformatting drives." + ` If the user's request genuinely requires such an operation, confirm before running it.
- When unsure about a file's contents, location, or structure, inspect first (` + "`ls`, `read`, `grep`" + `) rather than guessing a path or writing speculative code. It is always cheaper to verify than to revert a wrong edit.
- When you finish a task, reply with a short summary of what changed and any commands the user should run (tests, builds, service restarts). Do not paste the full diff back; the TUI already showed the edits. Do not re-describe what each tool call did; just name the outcome.
- If the user's request is ambiguous in a way that could lead to a destructive or costly action, ask a single clarifying question before proceeding. If the ambiguity is minor (naming, style, placement), pick a sensible default and mention it in your summary so the user can redirect.`
