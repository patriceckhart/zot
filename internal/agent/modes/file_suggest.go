package modes

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/patriceckhart/zot/internal/tui"
)

// fileSuggester provides an @-triggered file/directory picker popup.
// Type "@" followed by an optional filter to list files in the working
// directory. Arrow up/down navigate, enter selects, esc cancels.
// Right arrow opens a directory, left arrow goes back to the parent.
type fileSuggester struct {
	cursor      int
	lastMatches []fileEntry
	cwd         string // project root
	browseRel   string // relative path from cwd we're currently browsing ("" = cwd itself)
	cachedDir   string // absolute directory we last scanned
	cachedAll   []fileEntry
}

type fileEntry struct {
	name  string // display name
	rel   string // path relative to cwd (used for chip insertion)
	isDir bool
}

func newFileSuggester() *fileSuggester { return &fileSuggester{} }

// SetCWD updates the project root.
func (s *fileSuggester) SetCWD(cwd string) {
	if s.cwd != cwd {
		s.cwd = cwd
		s.browseRel = ""
		s.cachedDir = ""
		s.cachedAll = nil
	}
}

// browseDir returns the absolute directory currently being browsed.
func (s *fileSuggester) browseDir() string {
	if s.browseRel == "" {
		return s.cwd
	}
	return filepath.Join(s.cwd, s.browseRel)
}

// scan reads entries from the current browse directory (cached).
func (s *fileSuggester) scan() []fileEntry {
	dir := s.browseDir()
	if s.cachedDir == dir && s.cachedAll != nil {
		return s.cachedAll
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var all []fileEntry
	for _, e := range entries {
		name := e.Name()
		rel := name
		if s.browseRel != "" {
			rel = filepath.Join(s.browseRel, name)
		}
		all = append(all, fileEntry{
			name:  name,
			rel:   rel,
			isDir: e.IsDir(),
		})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].isDir != all[j].isDir {
			return all[i].isDir
		}
		return strings.ToLower(all[i].name) < strings.ToLower(all[j].name)
	})
	s.cachedAll = all
	s.cachedDir = dir
	return all
}

// extractAtQuery returns the filter string after "@".
func extractAtQuery(input string) (string, bool) {
	input = strings.TrimRight(input, " ")
	idx := strings.LastIndex(input, "@")
	if idx < 0 {
		return "", false
	}
	if idx > 0 && input[idx-1] != ' ' {
		return "", false
	}
	query := input[idx+1:]
	if strings.ContainsAny(query, " \t\n") {
		return "", false
	}
	return query, true
}

// matches returns file entries matching the current @-query.
func (s *fileSuggester) matches(input string) []fileEntry {
	query, ok := extractAtQuery(input)
	if !ok {
		return nil
	}
	all := s.scan()
	if len(all) == 0 {
		return nil
	}
	needle := strings.ToLower(query)
	if needle == "" {
		return all
	}
	var out []fileEntry
	for _, e := range all {
		if strings.Contains(strings.ToLower(e.name), needle) {
			out = append(out, e)
		}
	}
	return out
}

// Active reports whether the popup should be visible.
func (s *fileSuggester) Active(input string) bool {
	return len(s.matches(input)) > 0
}

// Reset clears state.
func (s *fileSuggester) Reset() {
	s.cursor = 0
	s.lastMatches = nil
	s.browseRel = ""
	s.cachedDir = ""
	s.cachedAll = nil
}

// Invalidate forces a rescan.
func (s *fileSuggester) Invalidate() {
	s.cachedDir = ""
	s.cachedAll = nil
}

func (s *fileSuggester) Up() {
	if s.cursor > 0 {
		s.cursor--
	}
}

func (s *fileSuggester) Down() {
	if s.cursor < len(s.lastMatches)-1 {
		s.cursor++
	}
}

// Right opens the selected directory, descending into it.
// Returns true if a directory was entered.
func (s *fileSuggester) Right() bool {
	m := s.lastMatches
	if len(m) == 0 || s.cursor >= len(m) {
		return false
	}
	e := m[s.cursor]
	if !e.isDir {
		return false
	}
	s.browseRel = e.rel
	s.cachedDir = ""
	s.cachedAll = nil
	s.cursor = 0
	return true
}

// Left goes back to the parent directory.
// Returns true if we moved up.
func (s *fileSuggester) Left() bool {
	if s.browseRel == "" {
		return false
	}
	parent := filepath.Dir(s.browseRel)
	if parent == "." {
		parent = ""
	}
	s.browseRel = parent
	s.cachedDir = ""
	s.cachedAll = nil
	s.cursor = 0
	return true
}

// Selection returns the relative path of the currently highlighted entry.
func (s *fileSuggester) Selection(input string) string {
	m := s.matches(input)
	if len(m) == 0 {
		return ""
	}
	if s.cursor >= len(m) {
		s.cursor = len(m) - 1
	}
	return m[s.cursor].rel
}

// SelectedEntry returns the currently highlighted entry.
func (s *fileSuggester) SelectedEntry(input string) (fileEntry, bool) {
	m := s.matches(input)
	if len(m) == 0 {
		return fileEntry{}, false
	}
	if s.cursor >= len(m) {
		s.cursor = len(m) - 1
	}
	return m[s.cursor], true
}

// Render returns the popup lines.
func (s *fileSuggester) Render(input string, th tui.Theme, width int) []string {
	m := s.matches(input)
	if len(m) == 0 {
		return nil
	}
	s.lastMatches = m
	if s.cursor >= len(m) {
		s.cursor = len(m) - 1
	}

	const maxVisible = 12
	visible := m
	offset := 0
	if len(m) > maxVisible {
		if s.cursor >= maxVisible {
			offset = s.cursor - maxVisible + 1
		}
		end := offset + maxVisible
		if end > len(m) {
			end = len(m)
			offset = end - maxVisible
		}
		visible = m[offset:end]
	}

	var out []string

	// Show breadcrumb when browsing a subdirectory.
	if s.browseRel != "" {
		crumb := th.FG256(th.Accent, "  "+s.browseRel+"/")
		out = append(out, crumb)
	}

	for i, e := range visible {
		idx := offset + i
		name := e.name
		if e.isDir {
			name += "/"
		}
		plain := "  " + name
		if idx == s.cursor {
			out = append(out, th.PadHighlight(plain, width))
		} else {
			out = append(out, th.FG256(th.Muted, plain))
		}
	}

	out = append(out, "")
	hint := "  \u2191/\u2193 navigate - enter select - esc cancel"
	if s.browseRel != "" {
		hint = "  \u2191/\u2193 navigate - \u2192 open - \u2190 back - enter select - esc cancel"
	} else {
		hint = "  \u2191/\u2193 navigate - \u2192 open dir - enter select - esc cancel"
	}
	out = append(out, th.FG256(th.Muted, hint))
	out = append(out, "")
	return out
}

// fileChipRE matches [file:N:name] and [dir:N:name/] chips.
var fileChipRE = regexp.MustCompile(`\[(file|dir):(\d+):[^\]]+\]`)

// expandFileChips replaces [file:name] and [dir:name/] chips with
// the full path relative to cwd.
func expandFileChips(text, cwd string) string {
	return fileChipRE.ReplaceAllStringFunc(text, func(match string) string {
		groups := fileChipRE.FindStringSubmatch(match)
		if len(groups) < 3 {
			return match
		}
		name := groups[2]
		// Strip trailing slash from dir names.
		name = strings.TrimRight(name, "/")
		return filepath.Join(cwd, name)
	})
}
