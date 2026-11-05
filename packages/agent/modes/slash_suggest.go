package modes

import (
	"sort"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/sahilm/fuzzy"

	"github.com/patriceckhart/zot/packages/agent/skills"
	"github.com/patriceckhart/zot/packages/tui"
)

// slashCommand is one entry in the autocomplete popup. Header rows
// (group dividers like "── extensions ───") are real entries
// flagged with header=true; they render but aren't navigable.
type slashCommand struct {
	Name           string // with leading "/"
	Desc           string
	Header         bool  // true = visual divider, not selectable
	Builtin        bool  // dynamic host command, shown in the main group
	matchedIndexes []int // byte offsets in Name for fuzzy skill matches
}

// slashCancelsTurn reports whether the named slash command, when run
// while a turn is in flight, requires the active turn to be cancelled
// first. The destructive commands (those that mutate the transcript
// or rebuild the agent) need a quiet state; the rest run alongside
// the streaming response without trouble.
func slashCancelsTurn(head string) bool {
	switch strings.ToLower(head) {
	case "/new", "/clear", "/compact", "/logout", "/login", "/model", "/llama", "/reload-ext", "/cd":
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
	{Name: "/reasoning", Desc: "set the reasoning level"},
	{Name: "/llama", Desc: "manage llama.cpp router models"},
	{Name: "/sessions", Desc: "resume a previous session for this directory"},
	{Name: "/new", Desc: "start a fresh session in this directory"},
	{Name: "/session", Desc: "manage the current session"},
	{Name: "/jump", Desc: "scroll the chat to a previous turn (or /jump <text>)"},
	{Name: "/compact", Desc: "summarize and replace the transcript to free up context"},
	{Name: "/study", Desc: "read every file in the cwd (or a passed file/dir) so the agent has full context"},
	{Name: "/btw", Desc: "side-chat that doesn't add to the main thread (saves tokens)"},
	{Name: "/jail", Desc: "confine tools to the current directory"},
	{Name: "/unjail", Desc: "allow tools to touch paths outside this directory"},
	{Name: "/skills", Desc: "list discovered skills (SKILL.md files)"},
	{Name: "/swarm", Desc: "supervise background agents that share this working directory"},
	{Name: "/wip", Desc: "open draft mode controls (on, off, status, done)"},
	{Name: "/reload-ext", Desc: "hot-reload all extensions (re-read manifests and respawn)"},
	{Name: "/telegram", Desc: "connect, disconnect, or show status of the telegram bridge"},
	{Name: "/settings", Desc: "open settings"},
	{Name: "/clear", Desc: "clear the chat transcript"},
	{Name: "/exit", Desc: "exit zot"},
}

var wipSlashCommands = []slashCommand{
	{Name: "/wip on", Desc: "enable draft mode"},
	{Name: "/wip off", Desc: "disable draft mode"},
	{Name: "/wip status", Desc: "show draft mode and its start time"},
	{Name: "/wip done", Desc: "disable draft mode and request cleanup"},
}

var sessionSlashCommands = []slashCommand{
	{Name: "/session timeline", Desc: "inspect context, messages, and tool calls"},
	{Name: "/session export", Desc: "write the current session to a .zotsession file"},
	{Name: "/session import", Desc: "load a .zotsession file into this directory"},
	{Name: "/session fork", Desc: "branch from a past user message"},
	{Name: "/session tree", Desc: "switch between session branches"},
}

// slashSuggester renders the popup that appears when the editor starts
// with "/". It does not own any input state — the editor drives.
const slashSuggestPageSize = 8

type slashSuggester struct {
	cursor int

	// jailed and llamaConfigured drive state-dependent command visibility.
	jailed          bool
	llamaConfigured bool
	sessionsEnabled bool

	// extra are dynamic built-in and extension commands, refreshed each
	// frame from the host registry. Sorted by name in SetExtra so map
	// iteration order doesn't reshuffle the popup between frames.
	extra []slashCommand

	// skills are dynamic /skill:<name> entries. skillInputActive tracks
	// whether the editor is already in one autocomplete session so the
	// interactive mode can refresh discovery once instead of every frame.
	skills           []slashCommand
	skillInputActive bool
	fuzzySkills      bool

	// lastMatches is the list shown in the most recent Render call.
	// Up/Down read it so they know which indexes to skip across
	// header rows.
	lastMatches []slashCommand
}

// SetExtra updates the dynamic host command list. Called
// once per render with the live snapshot from the extension manager.
// The list is sorted by name so the popup ordering stays stable
// across redraws (Manager.Commands() iterates a map, which Go
// randomises).
func (s *slashSuggester) SetExtra(cmds []slashCommand) {
	sorted := append([]slashCommand(nil), cmds...)
	sortSlashCommands(sorted)
	s.extra = sorted
}

// SetSkills updates the user-visible /skill:<name> completion entries.
// Built-ins stay hidden, while skills that disable model invocation remain
// available because explicit invocation is their intended entry point.
func (s *slashSuggester) SetSkills(list []*skills.Skill) {
	visible := skills.VisibleSkills(list)
	commands := make([]slashCommand, 0, len(visible))
	for _, skill := range visible {
		commands = append(commands, slashCommand{
			Name: skillCommandPrefix + skill.Name,
			Desc: strings.TrimSpace(skill.Description),
		})
	}
	sortSlashCommands(commands)
	s.skills = commands
	s.cursor = 0
}

func sortSlashCommands(commands []slashCommand) {
	sort.Slice(commands, func(i, j int) bool {
		left, right := strings.ToLower(commands[i].Name), strings.ToLower(commands[j].Name)
		if left == right {
			return commands[i].Name < commands[j].Name
		}
		return left < right
	})
}

// SkillInputStarted reports the first frame of a /skill: autocomplete
// session. Leaving the prefix resets it, so the next attempt refreshes skill
// discovery while ordinary redraws reuse the existing list.
func (s *slashSuggester) SkillInputStarted(input string) bool {
	active := strings.HasPrefix(strings.ToLower(input), skillCommandPrefix)
	started := active && !s.skillInputActive
	s.skillInputActive = active
	return started
}

// SetFuzzySkills changes skill matching without affecting other commands.
func (s *slashSuggester) SetFuzzySkills(enabled bool) {
	if s.fuzzySkills != enabled {
		s.fuzzySkills = enabled
		s.Reset()
		s.lastMatches = nil
	}
}

// fuzzySkillMatches ranks names only, breaking score ties by the index in
// the alphabetically sorted catalog.
func (s *slashSuggester) fuzzySkillMatches(query string) []slashCommand {
	if query == "" {
		return s.skills
	}
	names := make([]string, len(s.skills))
	for i, c := range s.skills {
		names[i] = strings.TrimPrefix(c.Name, skillCommandPrefix)
	}
	ranked := fuzzy.Find(query, names)
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			return ranked[i].Index < ranked[j].Index
		}
		return ranked[i].Score > ranked[j].Score
	})
	var out []slashCommand
	for _, match := range ranked {
		c := s.skills[match.Index]
		c.matchedIndexes = make([]int, len(match.MatchedIndexes))
		for i, index := range match.MatchedIndexes {
			c.matchedIndexes[i] = len(skillCommandPrefix) + index
		}
		if strings.EqualFold(names[match.Index], query) {
			return []slashCommand{c}
		}
		out = append(out, c)
	}
	return out
}

// SetJailed updates the current sandbox state. Called once per render
// so state-dependent commands can appear/disappear immediately.
func (s *slashSuggester) SetJailed(jailed bool) { s.jailed = jailed }

// SetLlamaConfigured shows the management command only when a router login is
// available.
func (s *slashSuggester) SetLlamaConfigured(configured bool) { s.llamaConfigured = configured }

// SetSessionsEnabled controls commands that require persisted sessions.
func (s *slashSuggester) SetSessionsEnabled(enabled bool) { s.sessionsEnabled = enabled }

// allCatalog groups static and dynamic built-ins before the extensions
// divider. Dynamic built-ins follow /wip. Dynamic entries cannot shadow
// static built-in names.
func (s *slashSuggester) allCatalog() []slashCommand {
	base := s.baseCatalog()
	if len(s.extra) == 0 {
		return base
	}
	at := len(base)
	for idx, c := range base {
		if c.Name == "/wip" {
			at = idx + 1
			break
		}
	}
	var dynamic []slashCommand
	for _, c := range s.extra {
		if !c.Builtin {
			continue
		}
		dup := false
		for _, b := range base {
			if strings.EqualFold(b.Name, c.Name) {
				dup = true
				break
			}
		}
		if !dup {
			dynamic = append(dynamic, c)
		}
	}
	base = append(base[:at:at], append(dynamic, base[at:]...)...)
	out := make([]slashCommand, 0, len(base)+len(s.extra)+1)
	out = append(out, base...)
	var kept []slashCommand
	for _, c := range s.extra {
		dup := false
		for _, b := range base {
			if strings.EqualFold(b.Name, c.Name) {
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

// baseCatalog returns the built-in commands visible for the current
// interactive state.
func (s *slashSuggester) baseCatalog() []slashCommand {
	hide := "/unjail"
	if s.jailed {
		hide = "/jail"
	}
	out := make([]slashCommand, 0, len(slashCatalog)-1)
	for _, c := range slashCatalog {
		if c.Name == hide || (c.Name == "/llama" && !s.llamaConfigured) || (c.Name == "/new" && !s.sessionsEnabled) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// looksLikeSlashCommand reports whether text is an attempt at a slash
// command (valid or not). Returns true for things like "/foo" or
// "/bar baz" but false for paths ("/Users/pat/...") and regexes
// ("/foo.bar/") so those can be sent to the model as-is.
//
// The head after "/" must be a single simple word: only letters,
// digits, hyphens, and underscores. The /skill:<name> form additionally
// permits one colon. Paths, regexes, and URLs are still excluded.
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
	if strings.HasPrefix(strings.ToLower(head), "skill:") {
		head = head[len("skill:"):]
		if head == "" {
			return true
		}
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
// slash command name in slashCatalog or hiddenSlashCommands. Built-in
// only; extension commands are looked up separately by the dispatcher
// (which consults the extension manager).
func isKnownSlashCommand(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '/' {
		return false
	}
	head := text
	if i := strings.IndexAny(text, " \t\n"); i >= 0 {
		head = text[:i]
	}
	if len(head) >= len(skillCommandPrefix) && strings.EqualFold(head[:len(skillCommandPrefix)], skillCommandPrefix) {
		return true
	}
	for _, c := range slashCatalog {
		if strings.EqualFold(c.Name, head) {
			return true
		}
	}
	for _, h := range hiddenSlashCommands {
		if strings.EqualFold(h, head) {
			return true
		}
	}
	return false
}

// hiddenSlashCommands are dispatchable but intentionally absent from
// the autocomplete popup, /help, and the README. Used for commands
// driven by extensions or by other internal flows where surfacing
// the verb to humans would be confusing or premature.
var hiddenSlashCommands = []string{
	"/cd",
}

func newSlashSuggester() *slashSuggester { return &slashSuggester{} }

// matches uses name prefixes by default, or ranked name subsequences for
// skills when explicitly enabled. If input is just "/", everything is shown.
// The /session command exposes
// its actions after the first space.
func (s *slashSuggester) matches(input string) []slashCommand {
	if input == "" || !strings.HasPrefix(input, "/") {
		return nil
	}
	matchPrefix := strings.ToLower(input)
	if strings.HasPrefix(matchPrefix, "/session ") || strings.HasPrefix(matchPrefix, "/wip ") {
		commands := sessionSlashCommands
		if strings.HasPrefix(matchPrefix, "/wip ") {
			commands = wipSlashCommands
		}
		var out []slashCommand
		for _, c := range commands {
			if strings.HasPrefix(strings.ToLower(c.Name), matchPrefix) {
				out = append(out, c)
			}
		}
		return out
	}
	if strings.ContainsRune(input, ' ') {
		return nil
	}
	catalog := s.allCatalog()
	if strings.HasPrefix(matchPrefix, skillCommandPrefix) {
		catalog = s.skills
		if s.fuzzySkills {
			return s.fuzzySkillMatches(input[len(skillCommandPrefix):])
		}
	}
	var out []slashCommand
	for _, c := range catalog {
		if c.Header {
			// Headers ride along whenever there's at least one
			// matching command from their group; we drop trailing
			// orphan headers below.
			out = append(out, c)
			continue
		}
		if strings.HasPrefix(strings.ToLower(c.Name), matchPrefix) {
			out = append(out, c)
		}
	}
	out = pruneOrphanHeaders(out)
	// Prefer an exact command over longer commands sharing its prefix. This
	// keeps /session selected instead of the earlier /sessions entry.
	for _, c := range out {
		if !c.Header && strings.EqualFold(c.Name, input) {
			return []slashCommand{c}
		}
	}
	return out
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

func (s *slashSuggester) PageUp() {
	if len(s.lastMatches) == 0 {
		return
	}
	s.cursor -= slashSuggestPageSize
	if s.cursor < 0 {
		s.cursor = 0
	}
	if s.lastMatches[s.cursor].Header {
		s.skipHeader(+1)
	}
}

func (s *slashSuggester) PageDown() {
	if len(s.lastMatches) == 0 {
		return
	}
	s.cursor += slashSuggestPageSize
	if s.cursor >= len(s.lastMatches) {
		s.cursor = len(s.lastMatches) - 1
	}
	if s.lastMatches[s.cursor].Header {
		s.skipHeader(-1)
	}
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
	// Compute the widest command name across the whole match list
	// (built-ins + extension-contributed) so every row's description
	// starts at the same x-position. A minimum keeps short lists
	// from collapsing the descriptions into the name column.
	nameWidth := 10
	for _, c := range m {
		if c.Header {
			continue
		}
		if n := runewidth.StringWidth(c.Name); n > nameWidth {
			nameWidth = n
		}
	}
	var lines []string
	for i := 0; i < len(m); i++ {
		c := m[i]
		if c.Header {
			// Breathing room around group dividers — a blank row
			// before AND after makes the boundary read at a glance.
			lines = append(lines, "")
			rule := strings.Repeat("─", width)
			label := "── " + c.Name + " "
			// runewidth, not len: the leading "── " glyphs are multi-byte,
			// so byte-length padding leaves the rule short of the right edge.
			if lw := runewidth.StringWidth(label); lw < width {
				rule = label + strings.Repeat("─", width-lw)
			}
			lines = append(lines, th.FG256(th.Muted, rule))
			lines = append(lines, "")
			continue
		}
		name := highlightedSlashName(c)
		if n := runewidth.StringWidth(c.Name); n < nameWidth {
			name += strings.Repeat(" ", nameWidth-n)
		}
		plain := "  " + name + "  " + c.Desc
		if i == s.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, plain))
		}
	}
	// Blank row before the hint visually detaches it from the
	// command list and groups it with its trailing blank.
	lines = append(lines, "")
	lines = append(lines, th.FG256(th.Muted, "  ↑/↓ navigate - tab complete - enter run"))
	// Blank row after the hint separates the popup from the status
	// bar / editor below it.
	lines = append(lines, "")
	return lines
}

// highlightedSlashName uses emphasis without resetting the row's foreground
// or selection background. Match offsets are UTF-8 byte indexes.
func highlightedSlashName(c slashCommand) string {
	if len(c.matchedIndexes) == 0 {
		return c.Name
	}
	var out strings.Builder
	match := 0
	for index, r := range c.Name {
		text := string(r)
		if match < len(c.matchedIndexes) && index == c.matchedIndexes[match] {
			text = tui.Bold(text)
			match++
		}
		out.WriteString(text)
	}
	return out.String()
}

// Reset puts the cursor back to the first match. Call this whenever the
// input changes in a way that reshapes the match list.
func (s *slashSuggester) Reset() { s.cursor = 0 }
