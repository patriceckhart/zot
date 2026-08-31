package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/ignore"
	"github.com/patriceckhart/zot/packages/provider"
)

const (
	maxGlobMatches = 500
	maxGlobDepth   = 24
)

// GlobTool finds files matching a pattern within a directory tree.
type GlobTool struct {
	CWD     string
	Sandbox *Sandbox
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
}

const globSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern to match files against (e.g. \"**/*.go\", \"*.json\", \"src/**/*.ts\")"},"path":{"type":"string","description":"Directory to search within, relative to CWD (defaults to \".\")"},"hidden":{"type":"boolean","description":"Whether to include hidden files and directories (default false)"}},"required":["pattern"]}`

func (t *GlobTool) Name() string { return "glob" }
func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern (e.g. \"**/*.go\", \"*.json\", \"src/**/*.ts\"). Honors .gitignore rules."
}
func (t *GlobTool) Schema() json.RawMessage { return json.RawMessage(globSchema) }

func (t *GlobTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a globArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return core.ToolResult{}, fmt.Errorf("pattern is required")
	}

	re, hasSlash, err := compileGlob(a.Pattern)
	if err != nil {
		return core.ToolResult{}, err
	}

	givenSearchDir := resolvePath(t.CWD, a.Path)
	if err := t.Sandbox.CheckReadPath(givenSearchDir); err != nil {
		return core.ToolResult{}, err
	}

	info, err := os.Stat(givenSearchDir)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !info.IsDir() {
		shown := t.Sandbox.DisplayPath(givenSearchDir, a.Path)
		return core.ToolResult{}, fmt.Errorf("%s is not a directory", shown)
	}

	// WalkDir does not follow a symlink used as its root. Resolve the explicit
	// search directory so a path that Stat identified as a directory is
	// actually traversed, then re-check the resolved target before reading it.
	searchDir, err := filepath.EvalSymlinks(givenSearchDir)
	if err != nil {
		return core.ToolResult{}, err
	}
	if err := t.Sandbox.CheckReadPath(searchDir); err != nil {
		return core.ToolResult{}, err
	}

	stack, ignoreRoot, searchIgnored := t.globIgnoreStack(searchDir)
	rootSep := strings.Count(searchDir, string(os.PathSeparator))
	var pushed []string
	var matches []string
	truncated := false

	var walkErr error
	if !searchIgnored {
		walkErr = filepath.WalkDir(searchDir, func(path string, d os.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if path == searchDir {
				return nil
			}

			rel, relErr := filepath.Rel(searchDir, path)
			if relErr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)

			// Check hidden files/directories unless explicitly requested.
			// .git is always skipped.
			if !a.Hidden {
				if strings.HasPrefix(d.Name(), ".") {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			} else if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}

			ignoreRel, ignoreRelErr := filepath.Rel(ignoreRoot, path)
			if ignoreRelErr != nil {
				return nil
			}
			ignoreRelSlash := filepath.ToSlash(ignoreRel)

			// .gitignore filtering: pop stack frames for directories no longer in scope.
			dirSlash := relSlash
			if !d.IsDir() {
				if idx := strings.LastIndex(relSlash, "/"); idx >= 0 {
					dirSlash = relSlash[:idx]
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

			if stack.Match(ignoreRelSlash, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if d.IsDir() {
				if strings.Count(path, string(os.PathSeparator))-rootSep >= maxGlobDepth {
					return filepath.SkipDir
				}
				stack.Push(path, ignoreRel)
				pushed = append(pushed, relSlash)
				return nil
			}

			// Check if file matches the glob pattern.
			matched := false
			if hasSlash {
				matched = re.MatchString(relSlash)
			} else {
				matched = re.MatchString(d.Name())
			}

			if matched {
				matches = append(matches, t.globDisplayPath(givenSearchDir, a.Path, relSlash))
				if len(matches) >= maxGlobMatches {
					truncated = true
					return filepath.SkipAll
				}
			}
			return nil
		})
	}

	if walkErr != nil && walkErr != filepath.SkipAll {
		return core.ToolResult{}, walkErr
	}

	sort.Strings(matches)

	var text string
	if len(matches) == 0 {
		text = "No files matched the pattern."
	} else {
		text = strings.Join(matches, "\n")
		if truncated {
			text += fmt.Sprintf("\n\n(Truncated: showing first %d matches)", maxGlobMatches)
		}
	}

	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Details: map[string]any{
			"matches":   len(matches),
			"truncated": truncated,
			"pattern":   a.Pattern,
		},
	}, nil
}

// globIgnoreStack builds an ignore stack rooted at CWD when the search path is
// below it, preloading the .gitignore files between CWD and the search root.
// This preserves inherited rules when the caller narrows a search with path.
func (t *GlobTool) globIgnoreStack(searchDir string) (*ignore.Stack, string, bool) {
	cwd := resolvePath(t.CWD, ".")
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil || t.Sandbox.CheckReadPath(cwd) != nil {
		return ignore.NewStack(searchDir), searchDir, false
	}

	rel, err := filepath.Rel(cwd, searchDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return ignore.NewStack(searchDir), searchDir, false
	}

	stack := ignore.NewStack(cwd)
	if rel == "." {
		return stack, cwd, false
	}

	current := cwd
	var relParts []string
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		relParts = append(relParts, part)
		currentRel := filepath.Join(relParts...)
		if stack.Match(filepath.ToSlash(currentRel), true) {
			return stack, cwd, true
		}
		stack.Push(current, currentRel)
	}
	return stack, cwd, false
}

func (t *GlobTool) globDisplayPath(searchDir, given, relSlash string) string {
	if given == "" || given == "." {
		return relSlash
	}
	prefix := filepath.ToSlash(t.Sandbox.DisplayPath(searchDir, given))
	if prefix == "" || prefix == "." {
		return relSlash
	}
	return strings.TrimSuffix(prefix, "/") + "/" + relSlash
}

// compileGlob converts a glob pattern into a regular expression.
// It supports '*', '**', '?', character classes '[...]', and brace expansion '{a,b}'.
func compileGlob(pattern string) (*regexp.Regexp, bool, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	pattern = strings.TrimPrefix(pattern, "./")
	if pattern == "" {
		return nil, false, fmt.Errorf("empty pattern")
	}

	hasSlash := strings.Contains(pattern, "/")

	var sb strings.Builder
	sb.WriteString("^")

	inBrace := false
	i := 0
	n := len(pattern)
	for i < n {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < n && pattern[i+1] == '*' {
				i += 2
				if i < n && pattern[i] == '/' {
					i++
					sb.WriteString("(?:.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				i++
				if hasSlash {
					sb.WriteString("[^/]*")
				} else {
					sb.WriteString(".*")
				}
			}
		case '?':
			i++
			if hasSlash {
				sb.WriteString("[^/]")
			} else {
				sb.WriteString(".")
			}
		case '{':
			closeIdx := strings.IndexByte(pattern[i:], '}')
			if closeIdx > 0 && strings.Contains(pattern[i:i+closeIdx], ",") {
				inBrace = true
				sb.WriteString("(?:")
				i++
			} else {
				sb.WriteString("\\{")
				i++
			}
		case ',':
			if inBrace {
				sb.WriteString("|")
				i++
			} else {
				sb.WriteString(",")
				i++
			}
		case '}':
			if inBrace {
				sb.WriteString(")")
				inBrace = false
				i++
			} else {
				sb.WriteString("\\}")
				i++
			}
		case '[':
			j := i + 1
			if j < n && pattern[j] == ']' {
				j++
			}
			for j < n && pattern[j] != ']' {
				j++
			}
			if j < n {
				sb.WriteString(pattern[i : j+1])
				i = j + 1
			} else {
				sb.WriteString("\\[")
				i++
			}
		case '.', '+', '(', ')', '^', '$', '|', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
			i++
		default:
			sb.WriteByte(c)
			i++
		}
	}
	sb.WriteString("$")

	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, false, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}
	return re, hasSlash, nil
}
