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
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one discovered SKILL.md file.
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

// Discover returns the merged skill set. By default this is just
// the built-in skills compiled into the zot binary; user-installed
// SKILL.md files are NOT loaded unless includeUser is true. Users
// opt in via the `--with-skills` flag.
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
	var errs []error
	seen := map[string]*Skill{}
	if includeUser {
		errs = append(errs, scanUserSkills(zotHome, cwd, userHome, seen)...)
	}
	// Built-ins fill in any name the user didn't already provide
	// (or every name, when includeUser is false).
	for _, s := range loadBuiltins() {
		if _, dup := seen[s.Name]; dup {
			continue
		}
		seen[s.Name] = s
	}
	out := make([]*Skill, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
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
// Format kept short and explicit so the model reliably calls the
// `skill` tool with a name from the list rather than guessing.
func SystemPromptAddendum(skills []*Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Available skills (call the `skill` tool with one of these names to load full instructions):\n")
	for _, s := range skills {
		desc := strings.TrimSpace(s.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&sb, "- %s — %s\n", s.Name, desc)
	}
	return sb.String()
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

func searchDirs(zotHome, cwd, userHome string) []location {
	var out []location
	add := func(dir, label string) {
		if dir == "" {
			return
		}
		out = append(out, location{dir: dir, label: label})
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
