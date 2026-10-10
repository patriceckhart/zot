package modes

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sahilm/fuzzy"

	"github.com/patriceckhart/zot/packages/ignore"
	"github.com/patriceckhart/zot/packages/tui"
)

// recursiveScanLimits bound the recursive walk so the picker stays
// responsive in very large repos. Hitting either cap stops the walk
// early; the entries gathered so far are still searchable.
//
// The entry cap is set against the real bottleneck, which is not memory
// but the per-keystroke fuzzy.Find ranking that scores every entry. A
// fileEntry is ~120 bytes all-in (40-byte struct header plus the path
// string), so even 50k entries is only ~6 MB. fuzzy.Find scales roughly
// linearly: ~2 ms at 5k, ~13 ms at 50k, ~21 ms at 100k on a typical
// laptop. 50k keeps ranking under one 60 Hz frame (~16 ms) while
// comfortably holding a large monorepo once nested .gitignore pruning
// (node_modules, build outputs, tool caches) has done its job. The
// depth cap likewise guards against pathologically deep trees; 24
// levels is far below anything a human navigates yet still reaches the
// deeply nested source files real monorepos bury.
const (
	maxRecursiveEntries = 50000
	maxRecursiveDepth   = 24
)

// alwaysSkipDir is never descended into during a recursive scan,
// regardless of .gitignore. .git is a repo-internal directory that
// real projects rarely list in their own .gitignore yet never want
// surfaced as an @-mention target.
const alwaysSkipDir = ".git"

// fileSuggester provides an @-triggered file/directory picker popup.
// Type "@" followed by an optional filter to list files in the working
// directory. Arrow up/down navigate, enter selects, esc cancels.
// Right arrow opens a directory, left arrow goes back to the parent.
type fileSuggester struct {
	cursor      int
	lastMatches []fileEntry
	cwd         string // project root
	browseRel   string // relative path from cwd we're currently browsing ("" = cwd itself)
	// recursive enables a whole-tree fuzzy search instead of
	// directory-by-directory browsing. Mirrors the persisted
	// recursive_file_suggest setting; toggled live from /settings.
	recursive bool
	// respectGitignore drops entries matched by the project's root
	// .gitignore (and always .git) from both the flat and recursive
	// listings. Mirrors the persisted respect_gitignore setting; on by
	// default. Toggled live from /settings.
	respectGitignore bool
	cachedDir        string // absolute directory we last scanned
	cachedAll        []fileEntry
	// cachedMTime is the mtime of cachedDir at the time of the scan.
	// scan() compares the current mtime against this on every call and
	// re-reads the directory if it has changed, so files or folders
	// created mid-session show up in the picker without restarting zot.
	// Stat is cheap (single syscall) so doing it per keystroke while
	// the popup is open does not impact responsiveness.
	cachedMTime time.Time
}

type fileEntry struct {
	name  string // display name
	rel   string // path relative to cwd (used for chip insertion)
	isDir bool
}

// newFileSuggester returns a picker that respects .gitignore by
// default. Callers override via SetRespectGitignore once the persisted
// setting is known.
func newFileSuggester() *fileSuggester { return &fileSuggester{respectGitignore: true} }

// SetCWD updates the project root.
func (s *fileSuggester) SetCWD(cwd string) {
	if s.cwd != cwd {
		s.cwd = cwd
		s.browseRel = ""
		s.cachedDir = ""
		s.cachedAll = nil
	}
}

// SetRecursive toggles whole-tree fuzzy search. Switching modes drops
// the cache and resets the browse position so the next render reflects
// the new mode immediately.
func (s *fileSuggester) SetRecursive(on bool) {
	if s.recursive == on {
		return
	}
	s.recursive = on
	s.browseRel = ""
	s.cachedDir = ""
	s.cachedAll = nil
	s.cursor = 0
}

// SetRespectGitignore toggles .gitignore filtering for both modes.
// Switching drops the cache so the next scan reflects the new state.
func (s *fileSuggester) SetRespectGitignore(on bool) {
	if s.respectGitignore == on {
		return
	}
	s.respectGitignore = on
	s.cachedDir = ""
	s.cachedAll = nil
	s.cursor = 0
}

// browseDir returns the absolute directory currently being browsed.
func (s *fileSuggester) browseDir() string {
	if s.browseRel == "" {
		return s.cwd
	}
	return filepath.Join(s.cwd, s.browseRel)
}

// scan reads entries from the current browse directory.
//
// Results are cached by absolute path + mtime: a repeated call against
// the same directory returns the cached slice when nothing has changed
// on disk, and re-reads when an entry was added, removed, or renamed
// (any of which bumps the directory's mtime on every filesystem zot
// supports). A failed stat falls through to a fresh ReadDir rather
// than returning a stale cache so transient errors self-heal.
func (s *fileSuggester) scan() []fileEntry {
	if s.recursive {
		return s.scanRecursive()
	}
	dir := s.browseDir()
	var mtime time.Time
	if info, err := os.Stat(dir); err == nil {
		mtime = info.ModTime()
	}
	if s.cachedDir == dir && s.cachedAll != nil && !mtime.IsZero() && mtime.Equal(s.cachedMTime) {
		return s.cachedAll
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ig *ignore.Gitignore
	if s.respectGitignore {
		ig = ignore.Load(s.cwd)
	}
	var all []fileEntry
	for _, e := range entries {
		name := e.Name()
		rel := name
		if s.browseRel != "" {
			rel = filepath.Join(s.browseRel, name)
		}
		if s.respectGitignore {
			if e.IsDir() && name == alwaysSkipDir {
				continue
			}
			if ig.Match(filepath.ToSlash(rel), e.IsDir()) {
				continue
			}
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
	s.cachedMTime = mtime
	return all
}

// scanRecursive walks the whole project tree below cwd and returns
// every file and directory as a fileEntry whose rel is the path
// relative to cwd. The walk honors the project's root .gitignore (so
// build outputs, dependency directories, and tool caches like
// .terraform/.terragrunt-cache stay out of the picker) plus an
// unconditional .git skip, and stops once it hits the entry/depth caps.
//
// Results are cached by cwd + mtime of cwd. Unlike the flat scan a
// single mtime can't catch every nested change, so the cache is best
// effort; Invalidate() (called on each keystroke path that matters)
// and the explicit cache drops on toggle keep it fresh enough for an
// interactive picker.
func (s *fileSuggester) scanRecursive() []fileEntry {
	root := s.cwd
	var mtime time.Time
	if info, err := os.Stat(root); err == nil {
		mtime = info.ModTime()
	}
	if s.cachedDir == root && s.cachedAll != nil && !mtime.IsZero() && mtime.Equal(s.cachedMTime) {
		return s.cachedAll
	}

	var stack *ignore.Stack
	if s.respectGitignore {
		stack = ignore.NewStack(root)
	}
	var all []fileEntry
	rootSep := strings.Count(root, string(os.PathSeparator))
	// pushed mirrors the directories whose nested .gitignore we've
	// brought into scope on stack but not yet dropped. WalkDir is
	// depth-first in lexical order, so before each entry we pop frames
	// for directories we've finished walking (those that are no longer
	// an ancestor of the current path), then push the current
	// directory. Honoring nested .gitignore files is what stops a
	// vendored node_modules whose ignore rule lives in a subdirectory
	// (e.g. .opencode/.gitignore) from swamping the entry budget before
	// the walk ever reaches the user's deeply nested source file.
	var pushed []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if s.respectGitignore {
			// Drop stack frames for directories we've finished walking:
			// any pushed dir that is not an ancestor of this entry's dir.
			dirSlash := relSlash
			if !d.IsDir() {
				if i := strings.LastIndex(relSlash, "/"); i >= 0 {
					dirSlash = relSlash[:i]
				} else {
					dirSlash = ""
				}
			}
			for len(pushed) > 0 {
				top := pushed[len(pushed)-1]
				if top == dirSlash || strings.HasPrefix(dirSlash, top+"/") {
					break
				}
				pushed = pushed[:len(pushed)-1]
				stack.Pop()
			}
			// .gitignore patterns are matched against slash-separated paths.
			if stack.Match(relSlash, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			// .git is always pruned: it's never a useful @-mention target
			// and would otherwise blow the entry budget even when
			// gitignore filtering is off.
			if d.Name() == alwaysSkipDir {
				return filepath.SkipDir
			}
			if strings.Count(path, string(os.PathSeparator))-rootSep >= maxRecursiveDepth {
				return filepath.SkipDir
			}
			// This directory is kept; bring its nested .gitignore (if
			// any) into scope for the descendants we're about to visit.
			if s.respectGitignore {
				stack.Push(path, rel)
				pushed = append(pushed, relSlash)
			}
		}
		all = append(all, fileEntry{
			name:  rel,
			rel:   rel,
			isDir: d.IsDir(),
		})
		if len(all) >= maxRecursiveEntries {
			return filepath.SkipAll
		}
		return nil
	})

	s.cachedAll = all
	s.cachedDir = root
	s.cachedMTime = mtime
	return all
}

// extractAtQuery returns the filter string after "@".
func extractAtQuery(input string) (string, bool) {
	input = strings.TrimRight(input, " ")
	idx := strings.LastIndex(input, "@")
	if idx < 0 {
		return "", false
	}
	start := idx
	if start > 0 && input[start-1] == '!' {
		start--
	}
	if start > 0 && !strings.ContainsRune(" \t\n", rune(input[start-1])) {
		return "", false
	}
	query := input[idx+1:]
	if strings.ContainsAny(query, " \t\n") {
		return "", false
	}
	return query, true
}

// matches returns file entries matching the current @-query.
//
// An empty query returns every entry in scan order. A non-empty query
// is ranked with sahilm/fuzzy: in recursive mode the pattern is matched
// against each entry's path relative to cwd (so "foobar" can find
// "src/foo/bar.go"); in flat mode it matches the entry's display name.
func (s *fileSuggester) matches(input string) []fileEntry {
	query, ok := extractAtQuery(input)
	if !ok {
		return nil
	}
	all := s.scan()
	if len(all) == 0 {
		return nil
	}
	if query == "" {
		return all
	}
	haystack := make([]string, len(all))
	for i, e := range all {
		if s.recursive {
			haystack[i] = e.rel
		} else {
			haystack[i] = e.name
		}
	}
	ranked := fuzzy.Find(query, haystack)
	out := make([]fileEntry, 0, len(ranked))
	for _, m := range ranked {
		if m.Index >= 0 && m.Index < len(all) {
			out = append(out, all[m.Index])
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
// Returns true if a directory was entered. Disabled in recursive mode,
// where the whole tree is already flattened into the result list.
func (s *fileSuggester) Right() bool {
	if s.recursive {
		return false
	}
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
// Returns true if we moved up. Disabled in recursive mode.
func (s *fileSuggester) Left() bool {
	if s.recursive {
		return false
	}
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
	var hint string
	switch {
	case s.recursive:
		hint = "  \u2191/\u2193 navigate - enter select - esc cancel (recursive)"
	case s.browseRel != "":
		hint = "  \u2191/\u2193 navigate - \u2192 open - \u2190 back - enter select - esc cancel"
	default:
		hint = "  \u2191/\u2193 navigate - \u2192 open dir - enter select - esc cancel"
	}
	out = append(out, th.FG256(th.Muted, hint))
	out = append(out, "")
	return out
}

// handleFilePickerKey shares selection and browsing across chat editors.
// Content mode selects individual files, directories remain browsable.
func handleFilePickerKey(ed *tui.Editor, fs *fileSuggester, k tui.Key) (handled bool, errMsg string) {
	if fs == nil {
		return false, ""
	}
	val := ed.Value()
	_, query := extractAtQuery(val)
	if !fs.Active(val) && !(query && (k.Kind == tui.KeyEsc || k.Kind == tui.KeyLeft)) {
		return false, ""
	}
	idx := strings.LastIndex(val, "@")
	start := idx
	content := start > 0 && val[start-1] == '!'
	if content {
		start--
	}
	switch k.Kind {
	case tui.KeyUp:
		fs.Up()
	case tui.KeyDown:
		fs.Down()
	case tui.KeyRight:
		if fs.Right() {
			ed.SetValue(val[:idx+1])
		}
	case tui.KeyLeft:
		if fs.Left() {
			ed.SetValue(val[:idx+1])
		}
	case tui.KeyEnter:
		if entry, ok := fs.SelectedEntry(val); ok {
			chip := "[file:" + entry.rel + "]"
			if content {
				if entry.isDir {
					return true, "content selection requires a file, use right to browse the directory"
				}
				chip = "[content:" + entry.rel + "]"
			} else if entry.isDir {
				chip = "[dir:" + entry.rel + "/]"
			}
			ed.SetValue(val[:start] + chip + " ")
			fs.Reset()
		}
	case tui.KeyEsc:
		ed.SetValue(val[:start])
		fs.Reset()
	default:
		return false, ""
	}
	return true, ""
}

// fileChipRE matches @-picker chips inserted by modes:
// [file:relative/path] and [dir:relative/path/]. The lower-level
// tui.Editor has its own drag/drop chips ([file:N:basename]) which
// Editor.SubmitValue expands before callers reach this helper; if one
// leaks through, leave it untouched rather than guessing at a path.
var fileChipRE = regexp.MustCompile(`\[(file|dir):([^\]]+)\]`)
var editorFileChipRE = regexp.MustCompile(`^\d+:`)

// expandFileChips replaces [file:name] and [dir:name/] chips with
// the full path relative to cwd.
func expandFileChips(text, cwd string) string {
	return fileChipRE.ReplaceAllStringFunc(text, func(match string) string {
		groups := fileChipRE.FindStringSubmatch(match)
		if len(groups) < 3 {
			return match
		}
		name := groups[2]
		if editorFileChipRE.MatchString(name) {
			return match
		}
		// Strip trailing slash from dir names.
		name = strings.TrimRight(name, "/")
		return filepath.Join(cwd, name)
	})
}
