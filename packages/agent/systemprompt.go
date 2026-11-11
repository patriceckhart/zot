package agent

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// ToolSummary is a name+one-line description. The prompt uses names to
// select usage guidance, but does not duplicate the provider's tool list.
type ToolSummary struct {
	Name        string
	Description string
	// ConfiguredShell suppresses host-specific default shell guidance.
	ConfiguredShell bool
}

// SystemPromptOpts configures BuildSystemPrompt.
type SystemPromptOpts struct {
	CWD        string
	Tools      []ToolSummary // available tools, used to select built-in guidance
	Custom     string        // when CustomSet, replaces the built-in identity and docs guidance
	CustomSet  bool          // preserves an intentionally empty custom prompt
	Append     []string      // extra text appended at the end
	Now        time.Time
	ZotDocsDir string
	GOOS       string // defaults to runtime.GOOS, overridable for tests
	GOARCH     string // defaults to runtime.GOARCH
}

// BuildSystemPrompt constructs a compact identity, tool-dependent usage
// guidance, and a date/cwd/platform footer. Tool descriptions remain in the
// provider's tool schemas rather than being duplicated here.
//
// Users can replace the identity and built-in guidance with --system-prompt
// or $ZOT_HOME/SYSTEM.md, or add instructions with --append-system-prompt.
func BuildSystemPrompt(o SystemPromptOpts) string {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	date := o.Now.Format("2006-01-02")
	cwd := o.CWD
	if cwd == "" {
		cwd = "."
	}
	available := make(map[string]bool, len(o.Tools))
	configuredShell := false
	for _, tool := range o.Tools {
		available[tool.Name] = true
		if tool.Name == "bash" {
			configuredShell = tool.ConfiguredShell
		}
	}

	var sb strings.Builder

	if o.CustomSet || o.Custom != "" {
		sb.WriteString(o.Custom)
	} else {
		sb.WriteString(defaultIdentity)
		var hints []string
		if available["edit"] {
			hints = append(hints, "For focused changes to an existing file, inspect its current contents and use edit with verbatim oldText taken from that same file. Include only enough context to make each match unambiguous.")
		}
		if available["write"] {
			hints = append(hints, "Use write when creating a file or replacing it wholesale.")
		}
		if available["bash"] && available["edit"] && available["write"] {
			hints = append(hints, "Do not mutate files through bash redirections or commands such as cat, echo, sed, or tee, because those changes appear as opaque shell output instead of a readable edit diff.")
		}
		if len(hints) > 0 {
			sb.WriteString("\n\n")
			sb.WriteString(strings.Join(hints, " "))
		}
		if strings.TrimSpace(o.ZotDocsDir) != "" {
			sb.WriteString("\n\nZot's own docs are installed under ")
			sb.WriteString(o.ZotDocsDir)
			if available["read"] {
				sb.WriteString(". Start with index.md and use the read tool there for zot configuration, tools, RPC, extensions, skills, or built-in behaviour.")
			} else {
				sb.WriteString(". The index.md file is the entry point for zot configuration, tools, RPC, extensions, skills, and built-in behaviour.")
			}
			sb.WriteString(" Repository reference docs take precedence over website-only pages, which may describe newer features.")
		}
	}

	for _, a := range o.Append {
		if strings.TrimSpace(a) == "" {
			continue
		}
		sb.WriteString("\n\n")
		sb.WriteString(a)
	}

	fmt.Fprintf(&sb, "\n\nCurrent date: %s\nCurrent working directory: %s\n", date, cwd)
	sb.WriteString(platformLine(o.GOOS, o.GOARCH, available, configuredShell))
	return sb.String()
}

// platformLine identifies the OS and, when bash is available, its Windows
// shell dialect. Keep this short because it sits in the cached prefix.
func platformLine(goos, goarch string, available map[string]bool, configuredShell bool) string {
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	line := fmt.Sprintf("Platform: %s/%s\n", goos, goarch)
	if goos == "windows" && available["bash"] && !configuredShell {
		line += windowsShellHint
		if available["read"] && available["glob"] {
			line += "For searching, prefer the read and glob tools.\n"
		}
	}
	return line
}

const windowsShellHint = `The bash tool runs cmd.exe /C, not a POSIX shell: use cmd syntax (dir, type, findstr, where, set, %VAR%, backslash paths). Commands such as grep, ls, cat, sed, head, $VAR, and single quotes do not work. Keep commands simple: cmd's quoting breaks on nested double quotes. Use git grep for code and powershell -NoProfile -File <script.ps1> for anything complex.
`

const defaultIdentity = `You are an expert coding assistant operating inside zot, a coding agent harness. The name "zot" stands for "zero-overhead-tool". If the user asks what zot means, answer exactly that.

Your output renders in a TUI that understands markdown for prose and plain text for tool output. Use markdown freely, keep answers concise, and let tool calls speak for themselves rather than narrating them in prose before you invoke them. Act first, then summarise what you did.`
