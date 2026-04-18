package tui

import (
	"bytes"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// HighlightCode syntax-colors src and returns the result split into lines,
// ready for a line-based diff renderer. If no language is given or
// chroma has no lexer for it, src is returned as-is (one entry per line).
// Safe to call from multiple goroutines.
//
// Results are memoised by (lang, src) so repeated calls from the view
// builder (which runs on every redraw) don't re-tokenise. Cache is
// bounded and evicts oldest entries past its cap.
func HighlightCode(src, lang string) []string {
	if out, ok := highlightCache.lookup(lang, src); ok {
		return out
	}
	lexer := chooseLexer(lang)
	if lexer == nil {
		out := strings.Split(src, "\n")
		highlightCache.store(lang, src, out)
		return out
	}
	style := zotChromaStyle
	formatter := formatters.Get("terminal256")
	if formatter == nil {
		return strings.Split(src, "\n")
	}

	iterator, err := lexer.Tokenise(nil, src)
	if err != nil {
		out := strings.Split(src, "\n")
		highlightCache.store(lang, src, out)
		return out
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iterator); err != nil {
		out := strings.Split(src, "\n")
		highlightCache.store(lang, src, out)
		return out
	}
	out := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	highlightCache.store(lang, src, out)
	return out
}

// highlightResultCache is a simple LRU-ish cache: when it exceeds its
// capacity we drop half of the entries (the oldest, based on insertion
// order). That's good enough since tool_result text doesn't change once
// emitted, so cache hits are frequent and evictions rare.
type highlightResultCache struct {
	mu    sync.Mutex
	max   int
	data  map[string][]string
	order []string
}

func (c *highlightResultCache) key(lang, src string) string {
	return lang + "\x00" + src
}

func (c *highlightResultCache) lookup(lang, src string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[c.key(lang, src)]
	return v, ok
}

func (c *highlightResultCache) store(lang, src string, out []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = make(map[string][]string)
	}
	k := c.key(lang, src)
	if _, ok := c.data[k]; !ok {
		c.order = append(c.order, k)
	}
	c.data[k] = out
	if c.max > 0 && len(c.order) > c.max {
		// Evict the oldest half so we amortise the cost.
		cut := len(c.order) / 2
		for _, old := range c.order[:cut] {
			delete(c.data, old)
		}
		c.order = append([]string(nil), c.order[cut:]...)
	}
}

var highlightCache = &highlightResultCache{max: 512}

// LanguageFromPath maps file extensions to chroma lexer names.
func LanguageFromPath(p string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(p)), ".")
	base := strings.ToLower(filepath.Base(p))
	if lang := filenameLang[base]; lang != "" {
		return lang
	}
	return extLang[ext]
}

// chooseLexer picks the best lexer for a language hint; falls back to
// nil (no highlighting) if the hint is empty or unknown.
func chooseLexer(lang string) chroma.Lexer {
	if lang == "" {
		return nil
	}
	if l := lexers.Get(lang); l != nil {
		return chroma.Coalesce(l)
	}
	// Chroma accepts aliases; try common spellings.
	if l := lexers.Get(strings.ToLower(lang)); l != nil {
		return chroma.Coalesce(l)
	}
	return nil
}

// zotChromaStyle is a terminal style tuned for zot's dark palette.
// Built lazily so the cost is paid once per process.
var zotChromaStyle = func() *chroma.Style {
	chromaStyleOnce.Do(func() {
		chromaStyleCached = buildZotStyle()
	})
	return chromaStyleCached
}()

var (
	chromaStyleOnce   sync.Once
	chromaStyleCached *chroma.Style
)

// buildZotStyle returns a chroma style with colors that match zot's
// theme: accent blue for keywords, green for strings, muted grey for
// comments, user-tan for numbers. Falls back to chroma's bundled
// "fruity" style if the builder fails for any reason.
func buildZotStyle() *chroma.Style {
	builder := styles.Get("monokai").Builder()
	// Override the most visible tokens with explicit colors that match
	// pi's cli-highlight look on a dark terminal.
	builder.Add(chroma.Keyword, "#81a1c1 bold")       // imports, funcs, control flow
	builder.Add(chroma.KeywordConstant, "#81a1c1")    // true, false, null
	builder.Add(chroma.KeywordDeclaration, "#81a1c1") // const, let, var, type
	builder.Add(chroma.KeywordNamespace, "#81a1c1")   // import, from, package
	builder.Add(chroma.KeywordReserved, "#81a1c1 bold")
	builder.Add(chroma.KeywordType, "#88c0d0") // string, int, bool
	builder.Add(chroma.NameBuiltin, "#88c0d0")
	builder.Add(chroma.NameFunction, "#8fbcbb")
	builder.Add(chroma.NameClass, "#a3be8c bold")
	builder.Add(chroma.NameDecorator, "#b48ead")
	builder.Add(chroma.LiteralString, "#a3be8c") // strings
	builder.Add(chroma.LiteralStringEscape, "#bf616a")
	builder.Add(chroma.LiteralNumber, "#d08770")
	builder.Add(chroma.Comment, "#616e88 italic")
	builder.Add(chroma.CommentPreproc, "#b48ead")
	builder.Add(chroma.Operator, "#eceff4")
	builder.Add(chroma.Punctuation, "#d8dee9")
	builder.Add(chroma.Text, "#e5e9f0")
	builder.Add(chroma.Background, " bg:")

	s, err := builder.Build()
	if err != nil {
		return styles.Fallback
	}
	return s
}

// extLang mirrors pi's extToLang. Only extensions that chroma supports
// are included.
var extLang = map[string]string{
	"ts":         "typescript",
	"tsx":        "tsx",
	"js":         "javascript",
	"jsx":        "jsx",
	"mjs":        "javascript",
	"cjs":        "javascript",
	"py":         "python",
	"rb":         "ruby",
	"rs":         "rust",
	"go":         "go",
	"java":       "java",
	"kt":         "kotlin",
	"swift":      "swift",
	"c":          "c",
	"h":          "c",
	"cpp":        "cpp",
	"cc":         "cpp",
	"cxx":        "cpp",
	"hpp":        "cpp",
	"cs":         "csharp",
	"php":        "php",
	"sh":         "bash",
	"bash":       "bash",
	"zsh":        "bash",
	"fish":       "fish",
	"ps1":        "powershell",
	"sql":        "sql",
	"html":       "html",
	"htm":        "html",
	"css":        "css",
	"scss":       "scss",
	"sass":       "sass",
	"less":       "less",
	"json":       "json",
	"yaml":       "yaml",
	"yml":        "yaml",
	"toml":       "toml",
	"xml":        "xml",
	"md":         "markdown",
	"markdown":   "markdown",
	"dockerfile": "docker",
	"makefile":   "makefile",
	"lua":        "lua",
	"perl":       "perl",
	"pl":         "perl",
	"proto":      "protobuf",
	"tf":         "terraform",
	"hcl":        "terraform",
	"graphql":    "graphql",
	"gql":        "graphql",
	"vue":        "vue",
	"svelte":     "svelte",
	"r":          "r",
	"jl":         "julia",
	"ex":         "elixir",
	"exs":        "elixir",
	"scala":      "scala",
	"nix":        "nix",
}

var filenameLang = map[string]string{
	"dockerfile":     "docker",
	"makefile":       "makefile",
	"gnumakefile":    "makefile",
	".gitconfig":     "ini",
	".gitattributes": "ini",
	"cargo.toml":     "toml",
	"go.mod":         "go-module",
	"go.sum":         "text",
	"package.json":   "json",
	"tsconfig.json":  "json",
}
