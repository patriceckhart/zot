package modes

import (
	"strings"

	"github.com/patriceckhart/zot/internal/tui"
)

// slashCommand is one entry in the autocomplete popup.
type slashCommand struct {
	Name string // with leading "/"
	Desc string
}

// slashCancelsTurn reports whether the named slash command, when run
// while a turn is in flight, requires the active turn to be cancelled
// first. The destructive commands (those that mutate the transcript
// or rebuild the agent) need a quiet state; the rest run alongside
// the streaming response without trouble.
func slashCancelsTurn(head string) bool {
	switch head {
	case "/clear", "/compact", "/logout", "/login", "/model":
		return true
	}
	return false
}

// slashCatalog lists every slash command the interactive mode handles.
// Keep in sync with runSlash().
var slashCatalog = []slashCommand{
	{"/help", "show key bindings and commands"},
	{"/login", "log in via api key or subscription"},
	{"/logout", "clear a provider's credentials"},
	{"/model", "pick a model (or /model <id>)"},
	{"/sessions", "resume a previous session for this directory"},
	{"/jump", "scroll the chat to a previous turn (or /jump <text>)"},
	{"/compact", "summarize and replace the transcript to free up context"},
	{"/lock", "confine tools to the current directory"},
	{"/unlock", "allow tools to touch paths outside this directory"},
	{"/clear", "clear the chat transcript"},
	{"/exit", "exit zot"},
}

// slashSuggester renders the popup that appears when the editor starts
// with "/". It does not own any input state — the editor drives.
type slashSuggester struct {
	cursor int
}

// looksLikeSlashCommand reports whether text is an attempt at a slash
// command (valid or not). Returns true for things like "/foo" or
// "/bar baz" but false for paths ("/Users/pat/...") and regexes
// ("/foo.bar/") so those can be sent to the model as-is.
//
// The head after "/" must be a single simple word: only letters,
// digits, hyphens, and underscores. That excludes paths (contain "/"),
// regexes (contain "."), and URLs.
func looksLikeSlashCommand(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) < 2 || text[0] != '/' {
		return false
	}
	head := text[1:]
	if i := strings.IndexAny(head, " \t\n"); i >= 0 {
		head = head[:i]
	}
	if head == "" {
		return false
	}
	for _, r := range head {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// isKnownSlashCommand reports whether text's head matches a registered
// slash command name in slashCatalog.
func isKnownSlashCommand(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '/' {
		return false
	}
	head := text
	if i := strings.IndexAny(text, " \t\n"); i >= 0 {
		head = text[:i]
	}
	for _, c := range slashCatalog {
		if c.Name == head {
			return true
		}
	}
	return false
}

func newSlashSuggester() *slashSuggester { return &slashSuggester{} }

// matches returns the commands whose name has input as a prefix.
// If input is just "/", everything is shown.
func (s *slashSuggester) matches(input string) []slashCommand {
	input = strings.TrimRight(input, " ")
	if input == "" || !strings.HasPrefix(input, "/") {
		return nil
	}
	// If there is a space, the user has moved past the command name.
	if idx := strings.IndexByte(input, ' '); idx >= 0 {
		return nil
	}
	var out []slashCommand
	for _, c := range slashCatalog {
		if strings.HasPrefix(c.Name, input) {
			out = append(out, c)
		}
	}
	return out
}

// clampCursor keeps the cursor inside the current match list.
func (s *slashSuggester) clampCursor(n int) {
	if n <= 0 {
		s.cursor = 0
		return
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
	if s.cursor >= n {
		s.cursor = n - 1
	}
}

// Up / Down navigate the suggestion list.
func (s *slashSuggester) Up()   { s.cursor-- }
func (s *slashSuggester) Down() { s.cursor++ }

// Active reports whether the popup is visible for the given input.
func (s *slashSuggester) Active(input string) bool {
	return len(s.matches(input)) > 0
}

// Selection returns the currently highlighted command for input, or "".
func (s *slashSuggester) Selection(input string) string {
	m := s.matches(input)
	if len(m) == 0 {
		return ""
	}
	s.clampCursor(len(m))
	return m[s.cursor].Name
}

// Render returns the popup lines or nil.
func (s *slashSuggester) Render(input string, th tui.Theme, width int) []string {
	m := s.matches(input)
	if len(m) == 0 {
		return nil
	}
	s.clampCursor(len(m))
	var lines []string
	for i, c := range m {
		name := c.Name
		if len(name) < 10 {
			name = name + strings.Repeat(" ", 10-len(name))
		}
		plain := "  " + name + "  " + c.Desc
		if i == s.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}
	lines = append(lines, th.FG256(th.Muted, "  ↑/↓ navigate · tab complete · enter run"))
	return lines
}

// Reset puts the cursor back to the first match. Call this whenever the
// input changes in a way that reshapes the match list.
func (s *slashSuggester) Reset() { s.cursor = 0 }
