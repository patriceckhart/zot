// Package skills implements zot's reusable-instruction system.
//
// A skill is a per-folder SKILL.md file with a YAML frontmatter
// header. Skills live in well-known directories under the project or
// the user home; zot discovers them at startup, lists their names +
// one-line descriptions in the system prompt, and exposes a built-in
// "skill" tool the model uses to pull the full body on demand.
//
// The on-demand-load model keeps token usage cheap: only the
// short manifest goes into every request; the body is fetched as a
// tool result the one or two turns the model actually needs it.
//
// Discovery layout (priority order — first match wins per name):
//
//	./.zot/skills/<name>/SKILL.md            — project (native)
//	$ZOT_HOME/skills/<name>/SKILL.md         — global (native)
//	./.claude/skills/<name>/SKILL.md         — project (claude-compat)
//	~/.claude/skills/<name>/SKILL.md         — global (claude-compat)
//	./.agents/skills/<name>/SKILL.md         — project (agent-compat)
//	~/.agents/skills/<name>/SKILL.md         — global (agent-compat)
//
// The compat paths are deliberate: a SKILL.md written for any of
// the related ecosystems works in zot unchanged.
//
// Discovery walks each source recursively and follows symlinked
// directories, so a skill can be a link into a shared checkout. A link
// cycle is ignored, and each resolved directory is walked only once.
package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// Skill is one discovered SKILL.md file.
// Source describes one directory of skills. Sources are evaluated in order;
// the first skill with a canonical name wins.
type Source struct {
	Root   string
	Label  string
	Prefix string
}

type Skill struct {
	// Name is the skill identifier — what the model uses when it
	// invokes the `skill` tool. Taken from the frontmatter `name`
	// field; falls back to the directory basename.
	Name string

	// Description is the one-line summary shown to the model in the
	// system-prompt manifest.
	Description string

	// Body is the markdown after the frontmatter. Returned as the
	// tool result when the model loads this skill.
	Body string

	// Path is the absolute path to the SKILL.md file.
	Path string

	// Source is a human-friendly label describing where the skill
	// came from ("project", "global", "project (claude)", etc.).
	// Shown in the /skills picker.
	Source string

	// Builtin marks skills that ship inside the zot binary. They are
	// fully active for the model (system-prompt manifest + skill
	// tool) but hidden from user-facing surfaces like the /skills
	// picker so users only see skills they actually installed or
	// shipped in their project.
	Builtin bool

	// DisableModelInvocation hides the skill from the system-prompt
	// manifest. The user can still load it explicitly with
	// /skill:<name>.
	DisableModelInvocation bool

	// AllowedTools and Permissions are parsed for forward-
	// compatibility but NOT enforced in this version. They appear
	// in the skill body so the model can self-regulate.
	AllowedTools []string
	Permissions  map[string][]string
}

// VisibleSkills returns the subset of skills users should see in
// pickers, /skills, and other interactive surfaces. Built-ins are
// hidden because they're implementation detail; the model still
// loads them through the system-prompt manifest + the skill tool.
func VisibleSkills(in []*Skill) []*Skill {
	out := make([]*Skill, 0, len(in))
	for _, s := range in {
		if s == nil || s.Builtin {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Discover returns the merged skill set. When includeUser is true,
// user-installed SKILL.md files are loaded before built-ins. Callers
// normally pass true; --no-skill skips discovery entirely before this
// function is called.
//
// First-match-wins per name; the order matches the priority list
// in the package doc (project-local before global before claude-
// compat before agents-compat, all before built-ins). That means a
// user-installed skill with the same name as a built-in shadows
// the built-in once includeUser is true.
//
// Errors per skill are returned alongside the partial result so a
// single broken file doesn't suppress the rest.
func Discover(zotHome, cwd, userHome string, includeUser bool) ([]*Skill, []error) {
	var sources []Source
	if includeUser {
		for _, loc := range searchDirs(zotHome, cwd, userHome) {
			sources = append(sources, Source{Root: loc.dir, Label: loc.label})
		}
	}
	discovered, errs := DiscoverSources(sources, true)
	// Preserve the historical Discover contract: duplicate ordinary skills
	// were silently ignored. New callers using DiscoverSources receive the
	// richer shadowing diagnostics.
	filtered := errs[:0]
	for _, err := range errs {
		if !strings.Contains(err.Error(), " shadowed: ") {
			filtered = append(filtered, err)
		}
	}
	return discovered, filtered
}

// DiscoverSources recursively discovers SKILL.md files from sources in
// precedence order, then appends built-ins when includeUser is true or false.
// It returns non-fatal diagnostics for malformed files and shadowed names.
func DiscoverSources(sources []Source, includeBuiltins bool) ([]*Skill, []error) {
	return DiscoverSourcesExcluding(sources, includeBuiltins, nil)
}

// DiscoverSourcesExcluding discovers skills while omitting the supplied paths
// and their descendants, including symlink aliases. Paths must be absolute.
// Built-in skills are unaffected by filesystem exclusions.
func DiscoverSourcesExcluding(sources []Source, includeBuiltins bool, excludes []string) ([]*Skill, []error) {
	var errs []error
	seen := map[string]*Skill{}
	for _, source := range sources {
		found, scanErrs := scanSource(source, excludes)
		errs = append(errs, scanErrs...)
		for _, s := range found {
			if prior, dup := seen[s.Name]; dup {
				if sameSkillFile(prior.Path, s.Path) {
					continue
				}
				errs = append(errs, fmt.Errorf("skill %q shadowed: selected %s; ignored %s", s.Name, prior.Path, s.Path))
				continue
			}
			seen[s.Name] = s
		}
	}
	if includeBuiltins {
		for _, s := range loadBuiltins() {
			if prior, dup := seen[s.Name]; dup {
				errs = append(errs, fmt.Errorf("skill %q shadowed: selected %s; ignored %s", s.Name, prior.Path, s.Path))
				continue
			}
			seen[s.Name] = s
		}
	}
	out := make([]*Skill, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// sameSkillFile distinguishes repeated discovery from a genuine name conflict.
// Stat follows symlinks, and SameFile also handles filesystem path aliases.
func sameSkillFile(a, b string) bool {
	if a == b {
		return true
	}
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
}

func scanSource(source Source, excludes []string) ([]*Skill, []error) {
	var out []*Skill
	var errs []error
	if excludedSkillPath(source.Root, excludes) {
		return out, errs
	}
	info, err := os.Stat(source.Root)
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("%s: %w", source.Root, err))
		}
		return out, errs
	}
	if !info.IsDir() {
		if filepath.Base(source.Root) != "SKILL.md" {
			return out, errs
		}
		s, e := load(source.Root, source.Label)
		if e != nil {
			return out, []error{fmt.Errorf("%s: %w", source.Root, e)}
		}
		if s.Name == "" {
			s.Name = kebabCase(strings.TrimSuffix(filepath.Base(filepath.Dir(source.Root)), ""))
		}
		if source.Prefix != "" {
			s.Name = source.Prefix + s.Name
		}
		return []*Skill{s}, nil
	}
	root, err := filepath.EvalSymlinks(source.Root)
	if err != nil {
		return out, []error{fmt.Errorf("resolve %s: %w", source.Root, err)}
	}
	// filepath.WalkDir does not descend into symlinked directories, which
	// would silently drop a skill directory that links into a shared
	// checkout. Walk real directories before following links so an alias
	// cannot rename an existing unnamed skill. Record resolved directories
	// so a link cycle cannot recurse forever.
	visited := map[string]bool{}
	var linkedDirs []string
	var walk func(dir string)
	walk = func(dir string) {
		if excludedSkillPath(dir, excludes) {
			return
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve %s: %w", dir, err))
			return
		}
		if visited[resolved] {
			return
		}
		visited[resolved] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
			return
		}
		for _, d := range entries {
			// Skip hidden entries, including hidden symlinks.
			// An explicitly supplied source root may itself be hidden.
			if strings.HasPrefix(d.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, d.Name())
			if excludedSkillPath(path, excludes) {
				continue
			}
			if d.IsDir() {
				walk(path)
				continue
			}
			if d.Type()&fs.ModeSymlink != 0 {
				info, statErr := os.Stat(path) // resolves the link
				if statErr != nil {
					// Dangling and cyclic links are not skills. Anything
					// else (permissions, I/O) is worth reporting.
					if !errors.Is(statErr, fs.ErrNotExist) && !symlinkLoopError(statErr) {
						errs = append(errs, fmt.Errorf("%s: %w", path, statErr))
					}
					continue
				}
				if info.IsDir() {
					linkedDirs = append(linkedDirs, path)
					continue
				}
			}
			if d.Name() != "SKILL.md" {
				continue
			}
			s, e := load(path, source.Label)
			if e != nil {
				errs = append(errs, fmt.Errorf("%s: %w", path, e))
				continue
			}
			if s.Name == "" {
				name, e := deriveName(root, path)
				if e != nil {
					errs = append(errs, e)
					continue
				}
				s.Name = name
			}
			if source.Prefix != "" {
				s.Name = source.Prefix + s.Name
			}
			out = append(out, s)
		}
	}
	walk(root)
	for i := 0; i < len(linkedDirs); i++ {
		walk(linkedDirs[i])
	}
	return out, errs
}

func symlinkLoopError(err error) bool {
	// Windows reports cyclic links as ERROR_CANT_RESOLVE_FILENAME (1921),
	// rather than ELOOP. syscall does not export a name for this error.
	return errors.Is(err, syscall.ELOOP) ||
		(runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1921)))
}

// deriveName builds a skill name for a SKILL.md file that carries no
// frontmatter `name`, from its directory path relative to the source root.
func deriveName(root, path string) (string, error) {
	dir := filepath.Dir(path)
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	var parts []string
	if rel != "." {
		for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
			if k := kebabCase(p); k != "" {
				parts = append(parts, k)
			}
		}
	}
	if len(parts) == 0 {
		parts = append(parts, kebabCase(filepath.Base(dir)))
	}
	return strings.Join(parts, "-"), nil
}

// KebabCase converts a path component or display name to a stable skill name.
func KebabCase(s string) string { return kebabCase(s) }

func kebabCase(s string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if sep && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			sep = false
		} else {
			sep = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// scanUserSkills walks the user-skill search dirs and populates
// `seen` with first-match-wins per name. Split out so Discover's
// includeUser=false path doesn't have to skip over a giant block.
func scanUserSkills(zotHome, cwd, userHome string, seen map[string]*Skill) []error {
	var errs []error
	for _, loc := range searchDirs(zotHome, cwd, userHome) {
		entries, err := os.ReadDir(loc.dir)
		if err != nil {
			continue // missing dir is fine
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(loc.dir, e.Name(), "SKILL.md")
			s, err := load(path, loc.label)
			if err != nil {
				if !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("%s: %w", path, err))
				}
				continue
			}
			if s.Name == "" {
				s.Name = e.Name()
			}
			if _, dup := seen[s.Name]; dup {
				continue // higher-priority location already won
			}
			seen[s.Name] = s
		}
	}
	return errs
}

// SystemPromptAddendum returns the text to append to the system
// prompt when at least one skill is loaded. Empty string if none.
//
// The format is deliberately compact: name, one-line description,
// and a source pointer telling the model where the full body
// lives. Built-in skills show "builtin" since their markdown is
// embedded in the zot binary and not on the filesystem; user
// skills show their SKILL.md path (shortened with ~ for HOME).
//
// Loading still goes through the `skill` tool with just the name.
// The pointer is there so the model can (a) mention the source
// honestly in explanations and (b) distinguish between built-ins
// and user-authored instruction sets when reasoning about trust.
func SystemPromptAddendum(skills []*Skill) string {
	home, _ := os.UserHomeDir()
	var sb strings.Builder
	for _, s := range skills {
		if s == nil || s.DisableModelInvocation {
			continue
		}
		if sb.Len() == 0 {
			sb.WriteString("Available skills (call the `skill` tool with a name from this list to load its full instructions):\n")
		}
		desc := strings.TrimSpace(s.Description)
		if desc == "" {
			desc = "(no description)"
		}
		pointer := skillSourcePointer(s, home)
		fmt.Fprintf(&sb, "- %s [%s]: %s\n", s.Name, pointer, desc)
	}
	return sb.String()
}

// skillSourcePointer returns a short tag describing where a skill
// originates. Built-ins are tagged "builtin" because their markdown
// is embedded in the zot binary and not reachable through the
// filesystem. User skills are tagged with their SKILL.md path,
// collapsed to use ~ for the user home when possible.
func skillSourcePointer(s *Skill, home string) string {
	if s == nil {
		return "unknown"
	}
	if s.Builtin {
		return "builtin"
	}
	p := s.Path
	if p == "" {
		return "unknown"
	}
	if home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// FindByName returns the skill with the given name, or nil.
func FindByName(skills []*Skill, name string) *Skill {
	for _, s := range skills {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// ---- internals ----

type location struct {
	dir   string
	label string
}

// SearchSources returns the ordinary skill roots in precedence order.
func SearchSources(zotHome, cwd, userHome string) []Source {
	out := make([]Source, 0)
	for _, loc := range searchDirs(zotHome, cwd, userHome) {
		out = append(out, Source{Root: loc.dir, Label: loc.label})
	}
	return out
}

func searchDirs(zotHome, cwd, userHome string) []location {
	var out []location
	add := func(dir, label string) {
		if dir == "" {
			return
		}
		out = append(out, location{dir: dir, label: label})
	}
	if extra := os.Getenv("ZOT_AGENT_SKILLS"); extra != "" {
		for _, dir := range filepath.SplitList(extra) {
			add(dir, "agent")
		}
	}
	if cwd != "" {
		add(filepath.Join(cwd, ".zot", "skills"), "project")
	}
	if zotHome != "" {
		add(filepath.Join(zotHome, "skills"), "global")
	}
	if cwd != "" {
		add(filepath.Join(cwd, ".claude", "skills"), "project (claude)")
	}
	if userHome != "" {
		add(filepath.Join(userHome, ".claude", "skills"), "global (claude)")
	}
	if cwd != "" {
		add(filepath.Join(cwd, ".agents", "skills"), "project (agents)")
	}
	if userHome != "" {
		add(filepath.Join(userHome, ".agents", "skills"), "global (agents)")
	}
	return out
}

func load(path, source string) (*Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	front, body := splitFrontmatter(string(raw))
	s := &Skill{
		Path:   path,
		Source: source,
		Body:   strings.TrimSpace(body),
	}
	parseFrontmatter(front, s)
	return s, nil
}

// splitFrontmatter returns (yamlBlock, restOfDocument) for a string
// whose first non-empty line is "---". If no frontmatter is present,
// returns ("", entireString).
func splitFrontmatter(raw string) (string, string) {
	rest := strings.TrimLeft(raw, " \t\r\n")
	if !strings.HasPrefix(rest, "---") {
		return "", raw
	}
	rest = strings.TrimPrefix(rest, "---")
	// Drop the trailing newline after the opening ---.
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	}
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", raw // malformed; treat as no frontmatter
	}
	front := rest[:end]
	body := rest[end+len("\n---"):]
	body = strings.TrimLeft(body, " \t\r\n")
	return front, body
}

// parseFrontmatter handles the small subset of YAML zot recognizes:
//   - simple `key: value` lines
//   - block scalars for one-line descriptions
//   - `key: [a, b, c]` flow-style lists
//   - `key:` followed by indented `- item` block lists
//   - nested `key:` followed by indented `subkey: [...]` for permissions
//
// Anything more elaborate is ignored. We deliberately avoid a yaml
// dependency to keep zot's binary lean.
func parseFrontmatter(front string, s *Skill) {
	lines := strings.Split(front, "\n")
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			i++
			continue
		}
		// Top-level key: value or key:
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			colon := strings.IndexByte(trimmed, ':')
			if colon < 0 {
				i++
				continue
			}
			key := strings.TrimSpace(trimmed[:colon])
			value := strings.TrimSpace(trimmed[colon+1:])
			switch key {
			case "name":
				s.Name = unquote(value)
			case "description":
				s.Description = unquote(value)
				header, _, _ := strings.Cut(value, " #")
				switch strings.TrimSpace(header) {
				case ">", ">-", ">+", "|", "|-", "|+":
					var parts []string
					for i+1 < len(lines) {
						next := lines[i+1]
						text := strings.TrimSpace(next)
						if text != "" && next[0] != ' ' && next[0] != '\t' {
							break
						}
						i++
						if text != "" {
							parts = append(parts, text)
						}
					}
					s.Description = strings.Join(parts, " ")
				}
			case "disable-model-invocation", "disable_model_invocation":
				s.DisableModelInvocation = strings.EqualFold(unquote(value), "true")
			case "allowed-tools", "allowed_tools":
				if value != "" {
					s.AllowedTools = parseInlineList(value)
				} else {
					items, consumed := parseBlockList(lines[i+1:])
					s.AllowedTools = items
					i += consumed
				}
			case "permissions":
				if value != "" {
					// Unusual layout; ignore single-line for now.
					i++
					continue
				}
				perms, consumed := parsePermissionsBlock(lines[i+1:])
				s.Permissions = perms
				i += consumed
			}
		}
		i++
	}
}

// unquote trims surrounding " or ' from a value.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// parseInlineList parses "[a, b, c]" or "[\"a\", \"b\"]".
func parseInlineList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, unquote(strings.TrimSpace(p)))
	}
	return out
}

// parseBlockList consumes indented "- item" lines until a less-
// indented line. Returns the items + how many lines to skip.
func parseBlockList(lines []string) ([]string, int) {
	var out []string
	consumed := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "  - ") || strings.HasPrefix(line, "    - ") {
			out = append(out, unquote(strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(line, " "), "-"))))
			consumed++
			continue
		}
		if strings.TrimSpace(line) == "" {
			consumed++
			continue
		}
		break
	}
	return out, consumed
}

// parsePermissionsBlock parses an indented map of tool->[patterns].
//
//	permissions:
//	  bash: ["git diff*", "git log*"]
//	  read: ["./*.go"]
func parsePermissionsBlock(lines []string) (map[string][]string, int) {
	out := map[string][]string{}
	consumed := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "  ") {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			consumed++
			continue
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			break
		}
		key := strings.TrimSpace(trimmed[:colon])
		val := strings.TrimSpace(trimmed[colon+1:])
		if val == "" {
			break // we don't support nested-block lists for permissions
		}
		out[key] = parseInlineList(val)
		consumed++
	}
	return out, consumed
}
