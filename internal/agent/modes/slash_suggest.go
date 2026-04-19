package modes

import (
	"sort"
	"strings"

	"github.com/patriceckhart/zot/internal/tui"
)

// slashCommand is one entry in the autocomplete popup. Header rows
// (group dividers like "── extensions ───") are real entries
// flagged with header=true; they render but aren't navigable.
type slashCommand struct {
	Name   string // with leading "/"
	Desc   string
	Header bool // true = visual divider, not selectable
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
	{Name: "/help", Desc: "show key bindings and commands"},
	{Name: "/login", Desc: "log in via api key or subscription"},
	{Name: "/logout", Desc: "clear a provider's credentials"},
	{Name: "/model", Desc: "pick a model (or /model <id>)"},
	{Name: "/sessions", Desc: "resume a previous session for this directory"},
	{Name: "/jump", Desc: "scroll the chat to a previous turn (or /jump <text>)"},
	{Name: "/btw", Desc: "side-chat that doesn't add to the main thread (saves tokens)"},
	{Name: "/skills", Desc: "list discovered skills (SKILL.md files)"},
	{Name: "/compact", Desc: "summarize and replace the transcript to free up context"},
	{Name: "/lock", Desc: "confine tools to the current directory"},
	{Name: "/unlock", Desc: "allow tools to touch paths outside this directory"},
	{Name: "/clear", Desc: "clear the chat transcript"},
	{Name: "/exit", Desc: "exit zot"},
}

// slashSuggester renders the popup that appears when the editor starts
// with "/". It does not own any input state — the editor drives.
type slashSuggester struct {
	cursor int

	// extra are commands contributed by extensions, refreshed each
	// frame from the extension manager. Empty when no extensions
	// have registered any. Sorted by name in SetExtra so map
	// iteration order doesn't reshuffle the popup between frames.
	extra []slashCommand

	// lastMatches is the list shown in the most recent Render call.
	// Up/Down read it so they know which indexes to skip across
	// header rows.
	lastMatches []slashCommand
}

// SetExtra updates the extension-contributed command list. Called
// once per render with the live snapshot from the extension manager.
// The list is sorted by name so the popup ordering stays stable
// across redraws (Manager.Commands() iterates a map, which Go
// randomises).
func (s *slashSuggester) SetExtra(cmds []slashCommand) {
	sorted := append([]slashCommand(nil), cmds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	s.extra = sorted
}

// allCatalog returns slashCatalog plus the current extra commands
// (extension-registered) with a header divider between the two
// groups. Extra entries are only kept if they don't collide with
// a built-in name; the built-in always wins.
func (s *slashSuggester) allCatalog() []slashCommand {
	if len(s.extra) == 0 {
		return slashCatalog
	}
	out := make([]slashCommand, 0, len(slashCatalog)+len(s.extra)+1)
	out = append(out, slashCatalog...)
	var kept []slashCommand
	for _, c := range s.extra {
		dup := false
		for _, b := range slashCatalog {
			if b.Name == c.Name {
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, c)
		}
	}
	if len(kept) > 0 {
		out = append(out, slashCommand{Header: true, Name: "extensions"})
		out = append(out, kept...)
	}
	return out
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
// slash command name in slashCatalog. Built-in only; extension
// commands are looked up separately by the dispatcher (which
// consults the extension manager).
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
	for _, c := range s.allCatalog() {
		if c.Header {
			// Headers ride along whenever there's at least one
			// matching command from their group; we drop trailing
			// orphan headers below.
			out = append(out, c)
			continue
		}
		if strings.HasPrefix(c.Name, input) {
			out = append(out, c)
		}
	}
	return pruneOrphanHeaders(out)
}

// pruneOrphanHeaders removes header rows that have no commands
// after them (i.e. the next non-header is missing or another
// header). Keeps the popup clean when the input filters out a whole
// group.
func pruneOrphanHeaders(in []slashCommand) []slashCommand {
	out := make([]slashCommand, 0, len(in))
	for i, c := range in {
		if c.Header {
			nextReal := false
			for j := i + 1; j < len(in); j++ {
				if !in[j].Header {
					nextReal = true
					break
				}
			}
			if !nextReal {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// clampCursor keeps the cursor inside the current match list and
// nudges it past any header row so navigation never lands on one.
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

// Up / Down navigate the suggestion list, skipping header rows in
// either direction so the cursor only ever lands on selectable
// commands.
func (s *slashSuggester) Up() {
	s.skipHeader(-1)
}
func (s *slashSuggester) Down() {
	s.skipHeader(+1)
}

// skipHeader moves the cursor by step, then keeps moving in the same
// direction across header rows until it lands on a real command (or
// hits the edge, in which case it bounces back to the nearest real
// row).
func (s *slashSuggester) skipHeader(step int) {
	list := s.lastMatches
	n := len(list)
	if n == 0 {
		return
	}
	s.cursor += step
	for s.cursor >= 0 && s.cursor < n && list[s.cursor].Header {
		s.cursor += step
	}
	if s.cursor < 0 {
		// Bounce: find the first non-header from the top.
		for i, c := range list {
			if !c.Header {
				s.cursor = i
				return
			}
		}
		s.cursor = 0
	}
	if s.cursor >= n {
		// Bounce: find the last non-header.
		for i := n - 1; i >= 0; i-- {
			if !list[i].Header {
				s.cursor = i
				return
			}
		}
		s.cursor = n - 1
	}
}

// Active reports whether the popup is visible for the given input.
func (s *slashSuggester) Active(input string) bool {
	return len(s.matches(input)) > 0
}

// Selection returns the currently highlighted command for input, or "".
// Headers are never returned even if the cursor index would point at
// one; the cursor is moved forward to the next real command.
func (s *slashSuggester) Selection(input string) string {
	m := s.matches(input)
	if len(m) == 0 {
		return ""
	}
	s.clampCursor(len(m))
	if m[s.cursor].Header {
		for i := s.cursor + 1; i < len(m); i++ {
			if !m[i].Header {
				s.cursor = i
				break
			}
		}
	}
	if m[s.cursor].Header {
		return ""
	}
	return m[s.cursor].Name
}

// Render returns the popup lines or nil.
func (s *slashSuggester) Render(input string, th tui.Theme, width int) []string {
	m := s.matches(input)
	if len(m) == 0 {
		return nil
	}
	s.lastMatches = m
	s.clampCursor(len(m))
	// Snap cursor off any header (e.g. after a filter change put it on one).
	if s.cursor >= 0 && s.cursor < len(m) && m[s.cursor].Header {
		for i := s.cursor + 1; i < len(m); i++ {
			if !m[i].Header {
				s.cursor = i
				break
			}
		}
	}
	var lines []string
	for i, c := range m {
		if c.Header {
			rule := strings.Repeat("─", width)
			label := "── " + c.Name + " "
			if len(label) < width {
				rule = label + strings.Repeat("─", width-len(label))
			}
			lines = append(lines, th.FG256(th.Muted, rule))
			continue
		}
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
