package skills

import (
	"embed"
	"io/fs"
	"path"
	"strings"
)

// builtinFS holds the SKILL.md files zot ships with the binary.
// They appear in the catalogue as ordinary skills — same on-demand
// load via the `skill` tool, same /skills picker — but never need
// to be installed by the user. A user-installed skill with the same
// name shadows the built-in one (Discover's first-match-wins).
//
//go:embed all:builtin
var builtinFS embed.FS

// loadBuiltins returns every SKILL.md compiled into the binary.
// Errors per file are silently dropped: built-ins are part of the
// release; if one is malformed it's a release bug we want to surface
// in tests, not panic in front of the user.
func loadBuiltins() []*Skill {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil
	}
	var out []*Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := fs.ReadFile(builtinFS, path.Join("builtin", e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		front, body := splitFrontmatter(string(raw))
		s := &Skill{
			Path:    "builtin:" + e.Name(),
			Source:  "built-in",
			Body:    strings.TrimSpace(body),
			Builtin: true,
		}
		parseFrontmatter(front, s)
		if s.Name == "" {
			s.Name = e.Name()
		}
		out = append(out, s)
	}
	return out
}
