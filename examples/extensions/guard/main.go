// guard — phase 3 example: event subscriptions + tool-call interception.
//
// What it does:
//
//   - Subscribes to turn_start, tool_call, and turn_end events and
//     writes a one-line audit entry per event to its own log file.
//
//   - Intercepts every tool call. If the tool is `bash` and the
//     command matches a danger pattern (rm -rf, sudo, dd of=/, etc.)
//     the call is refused and the model gets a one-line refusal it
//     can react to. Everything else passes through.
//
// Build it:
//
//	cd examples/extensions/guard
//	go build -o guard .
//
// Drop it next to its extension.json under $ZOT_HOME/extensions/guard/
// (or `zot ext install ./guard` from this directory) and the next zot
// session will load it automatically.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/patriceckhart/zot/pkg/zotext"
)

var dangerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+-rf\b`),
	regexp.MustCompile(`(?i)\bsudo\b`),
	regexp.MustCompile(`(?i)\bdd\s+of=/`),
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\b:\s*\(\s*\)\s*\{\s*:\|:`), // fork bomb
	regexp.MustCompile(`(?i)\bchmod\s+-R\s+777\b`),
}

func main() {
	ext := zotext.New("guard", "1.0.0")

	auditPath := filepath.Join(os.TempDir(), "zot-guard-audit.log")
	auditFile, _ := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if auditFile != nil {
		defer auditFile.Close()
	}
	audit := func(format string, args ...any) {
		line := fmt.Sprintf("%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
		if auditFile != nil {
			auditFile.WriteString(line)
		}
		ext.Logf("%s", strings.TrimRight(line, "\n"))
	}

	// Lifecycle observers.
	ext.On("session_start", func(ev zotext.Event) { audit("[guard] session_start") })
	ext.On("turn_start", func(ev zotext.Event) { audit("[guard] turn_start step=%d", ev.Step) })
	ext.On("tool_call", func(ev zotext.Event) {
		audit("[guard] tool_call %s args=%s", ev.ToolName, string(ev.ToolArgs))
	})
	ext.On("turn_end", func(ev zotext.Event) { audit("[guard] turn_end stop=%s err=%s", ev.Stop, ev.Error) })

	// Tool-call gate.
	ext.InterceptToolCall(func(toolName string, args json.RawMessage) (bool, string) {
		if toolName != "bash" {
			return true, "" // only guard bash
		}
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return true, ""
		}
		for _, pat := range dangerPatterns {
			if pat.MatchString(in.Command) {
				audit("[guard] BLOCKED bash: %s (matched %s)", in.Command, pat.String())
				return false, fmt.Sprintf("guard refused: command matches the danger pattern %q. Ask the user before running this.", pat.String())
			}
		}
		return true, ""
	})

	if err := ext.Run(); err != nil {
		ext.Logf("fatal: %v", err)
	}
}
