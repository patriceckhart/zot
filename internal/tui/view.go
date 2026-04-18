package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/patriceckhart/zot/internal/provider"
)

// pathFromToolArgs returns the "path" argument from a tool_call's
// JSON arguments, or "" if the args aren't a JSON object or don't
// include one. Used to pick a syntax language for rendering the
// corresponding tool_result.
func pathFromToolArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"path", "file_path"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// osUserHomeDir is aliased so the test file can swap it.
var osUserHomeDir = os.UserHomeDir

// View turns a transcript + live state into a slice of styled lines,
// already wrapped to width.
type View struct {
	Theme      Theme
	ImageProto ImageProtocol // how to render inline images in this terminal
	Messages   []provider.Message
	// toolPaths maps tool_use_id to the "path" argument of the call, if
	// any, so tool_result rendering can pick the right syntax language.
	// Rebuilt on each Build().
	toolPaths map[string]string
	Streaming       string // current assistant text delta
	StreamingActive bool
	ToolCalls       []ToolCallView // tool calls in flight or completed
	StatusLine      string
	Err             string
}

// ToolCallView is a pending tool invocation plus optional result.
type ToolCallView struct {
	ID     string
	Name   string
	Args   string // rendered argument summary
	Result string // rendered result preview (truncated)
	Error  bool
	Done   bool
}

// Build returns the chat log lines for the given width.
func (v *View) Build(width int) []string {
	// Map tool_use_id -> path argument, if any, so tool results can be
	// rendered with the file's language for syntax highlighting.
	v.toolPaths = map[string]string{}
	for _, m := range v.Messages {
		for _, c := range m.Content {
			if tc, ok := c.(provider.ToolCallBlock); ok {
				if p := pathFromToolArgs(tc.Arguments); p != "" {
					v.toolPaths[tc.ID] = p
				}
			}
		}
	}

	var out []string
	for _, m := range v.Messages {
		out = append(out, v.renderMessage(m, width)...)
		out = append(out, "")
	}
	if v.StreamingActive {
		out = append(out, v.Theme.FG256(v.Theme.Assistant, "▍ zot"))
		for _, line := range wrapLine(v.Streaming, width, "") {
			out = append(out, line)
		}
		out = append(out, "")
	}
	for _, tc := range v.ToolCalls {
		out = append(out, v.renderToolCall(tc, width)...)
		out = append(out, "")
	}
	if v.Err != "" {
		out = append(out, v.Theme.FG256(v.Theme.Error, "✖ "+v.Err))
		out = append(out, "")
	}
	return out
}

func (v *View) renderMessage(m provider.Message, width int) []string {
	var lines []string
	switch m.Role {
	case provider.RoleUser:
		header := v.Theme.FG256(v.Theme.User, "▍ you")
		lines = append(lines, header)
		for _, c := range m.Content {
			switch b := c.(type) {
			case provider.TextBlock:
				for _, l := range strings.Split(b.Text, "\n") {
					for _, w := range wrapLine(l, width-2, "  ") {
						lines = append(lines, "  "+v.Theme.FG256(v.Theme.Muted, w))
					}
				}
			case provider.ImageBlock:
				lines = append(lines, "  "+v.Theme.FG256(v.Theme.Muted, fmt.Sprintf("[image %s, %d bytes]", b.MimeType, len(b.Data))))
			}
		}
	case provider.RoleAssistant:
		header := v.Theme.FG256(v.Theme.Assistant, "▍ zot")
		lines = append(lines, header)
		for _, c := range m.Content {
			switch b := c.(type) {
			case provider.TextBlock:
				md := RenderMarkdown(b.Text, v.Theme, width)
				for _, l := range strings.Split(md, "\n") {
					for _, w := range wrapLine(l, width, "") {
						lines = append(lines, w)
					}
				}
			case provider.ToolCallBlock:
				lines = append(lines, v.Theme.FG256(v.Theme.Tool, "▸ "+b.Name+" "+shortArgs(b.Arguments)))
			}
		}
	case provider.RoleTool:
		for _, c := range m.Content {
			if tr, ok := c.(provider.ToolResultBlock); ok {
				title := "  result"
				color := v.Theme.ToolOut
				if tr.IsError {
					title = "  error"
					color = v.Theme.Error
				}
				path := ""
				if v.toolPaths != nil {
					path = v.toolPaths[tr.CallID]
				}
				lines = append(lines, v.Theme.FG256(color, title))
				lines = append(lines, v.renderToolResultContent(tr.Content, width, color, path)...)
			}
		}
	}
	return lines
}

func (v *View) renderToolCall(tc ToolCallView, width int) []string {
	var lines []string
	head := v.Theme.FG256(v.Theme.Tool, "▸ "+tc.Name+" "+tc.Args)
	lines = append(lines, head)
	if tc.Result != "" {
		color := v.Theme.ToolOut
		if tc.Error {
			color = v.Theme.Error
		}
		lines = append(lines, toolResultBlock(v.Theme, tc.Result, width, color)...)
	}
	return lines
}

// toolResultBlock wraps text in thin horizontal rules (top + bottom),
// indenting the body with four spaces. The rules span the content column.
// renderToolResultContent renders the body of a tool result block.
// Text blocks get the usual rules-wrapped treatment; text that looks
// like a unified diff gets +/- coloring. Image blocks are rendered
// inline when the terminal supports a protocol, else as a text
// placeholder with dimensions.
func (v *View) renderToolResultContent(blocks []provider.Content, width, color int, sourcePath string) []string {
	rule := v.Theme.FG256(v.Theme.Muted, strings.Repeat("─", width))

	var out []string
	out = append(out, rule)
	for _, b := range blocks {
		switch bb := b.(type) {
		case provider.TextBlock:
			out = append(out, v.renderToolText(bb.Text, width, color, sourcePath)...)
		case provider.ImageBlock:
			out = append(out, v.renderImageBlock(bb, width)...)
		}
	}
	out = append(out, rule)
	return out
}

// renderToolText renders a text block inside a tool result. If the
// text contains a unified-diff section (lines starting with "--- " /
// "+++ " / "+" / "-"/" "), those rows are styled with add/remove
// colors matching git diff conventions.
func (v *View) renderToolText(text string, width, defaultColor int, sourcePath string) []string {
	// Detect whether the text is `read`-style numbered output
	// ("     1\t…") so we can strip the gutter, highlight the code, and
	// re-apply the line numbers in muted color. Runs even without a
	// source path — language is guessed from the first line, falling
	// back to "text" (no highlighting) if nothing obvious matches.
	if looksLikeNumberedFile(text) {
		return v.renderNumberedFile(text, sourcePath)
	}

	// No truncation — the full tool output is rendered into chat and
	// becomes part of the scrollback you can page back through.
	lines := strings.Split(text, "\n")

	inDiff := false
	var out []string
	for _, l := range lines {
		// Detect diff header: "--- name" followed somewhere by "+++ name".
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
			inDiff = true
			out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, l))
			continue
		}
		if inDiff && len(l) > 0 {
			switch l[0] {
			case '+':
				out = append(out, v.renderDiffRow(l, width, v.Theme.Tool))
				continue
			case '-':
				out = append(out, v.renderDiffRow(l, width, v.Theme.Error))
				continue
			case ' ':
				out = append(out, v.renderDiffRow(l, width, v.Theme.Muted))
				continue
			}
		}
		// Regular line.
		for _, w := range wrapLine(l, width-4, "    ") {
			out = append(out, "    "+v.Theme.FG256(defaultColor, w))
		}
	}
	return out
}

// renderDiffRow renders a single unified-diff line in fg color only.
// The leading +/-/space stays visible so the user can tell at a glance
// what changed; the rest of the line is colored the same. Long lines
// are wrapped with a 4-cell indent preserved.
func (v *View) renderDiffRow(line string, width, color int) string {
	body := line
	if len(body) > width-4 {
		body = body[:width-7] + "…"
	}
	return "    " + v.Theme.FG256(color, body)
}

// renderImageBlock returns the lines for one image, inline if possible.
//
// Inline image escapes paint into multiple terminal rows but the zot
// renderer treats each slice entry as a single row. To prevent chat
// content from being drawn on top of the image, we pad with blank rows
// so the image's real footprint is reflected in the frame height.
func (v *View) renderImageBlock(b provider.ImageBlock, width int) []string {
	w, h := ImageDimensions(b.Data)
	kb := len(b.Data) / 1024
	info := fmt.Sprintf("  image · %s · %dx%d · %d KB", b.MimeType, w, h, kb)

	if v.ImageProto != ImageProtocolNone {
		// Clamp rendered width so the image never overflows the chat
		// column. Subtract a 4-cell indent for the tool result block,
		// cap at 60 cells, floor at 10.
		cells := width - 4
		if cells > 60 {
			cells = 60
		}
		if cells < 10 {
			cells = 10
		}
		const maxRows = 20
		if seq := RenderInlineImageScaled(v.ImageProto, b.Data, b.MimeType, cells, maxRows); seq != "" {
			rows := RowsForInlineImage(b.Data, cells, maxRows)
			if rows < 1 {
				rows = 1
			}
			out := make([]string, 0, rows+1)
			out = append(out, "    "+seq)
			// Reserve blank rows matching the image's on-screen height.
			for i := 1; i < rows; i++ {
				out = append(out, "")
			}
			out = append(out, v.Theme.FG256(v.Theme.Muted, info))
			return out
		}
	}
	// Text fallback.
	return []string{v.Theme.FG256(v.Theme.Muted, info)}
}

// looksLikeNumberedFile returns true when text matches the `read`
// tool's "     N\tcontent" format for most of its lines.
func looksLikeNumberedFile(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return false
	}
	hits := 0
	scanned := 0
	for _, l := range lines {
		if l == "" {
			continue
		}
		scanned++
		if scanned > 20 {
			break
		}
		if numberedLineRE.MatchString(l) {
			hits++
		}
	}
	if scanned == 0 {
		return false
	}
	return hits*2 >= scanned // majority of non-empty lines match
}

var numberedLineRE = regexp.MustCompile(`^\s*\d+\t`)

// renderNumberedFile strips line numbers, highlights the code, and
// re-attaches the line numbers in muted color.
func (v *View) renderNumberedFile(text, sourcePath string) []string {
	lines := strings.Split(text, "\n")
	gutters := make([]string, 0, len(lines))
	codes := make([]string, 0, len(lines))
	for _, l := range lines {
		idx := strings.IndexByte(l, '\t')
		if idx < 0 || !numberedLineRE.MatchString(l) {
			// Non-code footer (e.g. "[truncated at 2000 lines]").
			gutters = append(gutters, "")
			codes = append(codes, l)
			continue
		}
		gutter := l[:idx+1] // keep the tab so alignment is preserved
		code := l[idx+1:]
		gutters = append(gutters, gutter)
		codes = append(codes, code)
	}
	lang := LanguageFromPath(sourcePath)
	var highlighted []string
	if lang != "" {
		highlighted = HighlightCode(strings.Join(codes, "\n"), lang)
		// Chroma sometimes collapses the trailing empty line. Pad to
		// align with the gutter slice so per-line zipping works.
		for len(highlighted) < len(codes) {
			highlighted = append(highlighted, "")
		}
		if len(highlighted) > len(codes) {
			highlighted = highlighted[:len(codes)]
		}
	} else {
		// No lexer for this file type — render in the ToolOut color so
		// code is visually distinct from the muted gutter.
		highlighted = make([]string, len(codes))
		for i, c := range codes {
			highlighted[i] = v.Theme.FG256(v.Theme.ToolOut, c)
		}
	}
	out := make([]string, 0, len(codes))
	for i, code := range highlighted {
		g := gutters[i]
		if g == "" {
			out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, code))
			continue
		}
		out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, g)+code)
	}
	return out
}

func toolResultBlock(th Theme, text string, width int, color int) []string {
	rule := th.FG256(th.Muted, strings.Repeat("─", width))

	var out []string
	out = append(out, rule)
	for _, l := range strings.Split(text, "\n") {
		for _, w := range wrapLine(l, width-4, "    ") {
			out = append(out, "    "+th.FG256(color, w))
		}
	}
	out = append(out, rule)
	return out
}

func shortArgs(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range []string{"path", "file_path", "command"} {
			if s, ok := x[k].(string); ok {
				if len(s) > 60 {
					s = s[:57] + "..."
				}
				return s
			}
		}
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func collectText(blocks []provider.Content) string {
	var sb strings.Builder
	for _, b := range blocks {
		if tb, ok := b.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func truncateLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n  … (" + fmt.Sprintf("%d", len(lines)-n) + " more)"
}

// StatusBar builds the single-line status shown above the editor.
//
// Layout:
//   [busyPrefix]  provider-model   tokens-cost   ctrl+c hint        cwd
//                                                          ↑ right-aligned
//
// cols is the terminal width; when > 0 the cwd is placed flush-right
// with spaces. busyPrefix, if non-empty, is injected at the far left.
// StatusBarParams groups the many bits of state the status bar needs.
// Grew from a flat argument list once we started matching pi's format.
type StatusBarParams struct {
	Theme      Theme
	Provider   string
	Model      string
	Busy       bool
	BusyPrefix string // spinner + funny line when busy
	CWD        string
	Locked     bool // sandbox on?

	// Cumulative session usage and cost.
	Usage provider.Usage
	// Subscription is true when the credential is an OAuth token (claude
	// pro/max, chatgpt plus/pro) rather than a paid api key. We still
	// compute a cost for visibility, but pi-style we append "(sub)" so
	// the user knows no money actually moved.
	Subscription bool

	// Last turn's input+cache tokens (approximates current live context).
	ContextUsed int
	ContextMax  int // model's context window; 0 disables the percentage

	Cols int // terminal width; drives right-alignment of cwd
}

// StatusBar builds the single-line status shown above the editor.
// Format: ↑N ↓N RN WN $cost[(sub)] pct%/ctxMax
func StatusBar(p StatusBarParams) string {
	th := p.Theme
	sep := " - "

	// Token stats: only include each segment when non-zero (pi's
	// behavior). Keeps the bar compact on brand-new sessions.
	var stats []string
	if p.Usage.InputTokens > 0 {
		stats = append(stats, fmt.Sprintf("↑%s", piFormatTokens(p.Usage.InputTokens)))
	}
	if p.Usage.OutputTokens > 0 {
		stats = append(stats, fmt.Sprintf("↓%s", piFormatTokens(p.Usage.OutputTokens)))
	}
	if p.Usage.CacheReadTokens > 0 {
		stats = append(stats, fmt.Sprintf("R%s", piFormatTokens(p.Usage.CacheReadTokens)))
	}
	if p.Usage.CacheWriteTokens > 0 {
		stats = append(stats, fmt.Sprintf("W%s", piFormatTokens(p.Usage.CacheWriteTokens)))
	}

	var costStr string
	if p.Subscription {
		costStr = "$0.000 (sub)"
	} else if p.Usage.CostUSD > 0 {
		costStr = fmt.Sprintf("$%.3f", p.Usage.CostUSD)
	}
	if costStr != "" {
		stats = append(stats, costStr)
	}

	// Context %. Color-coded: yellow >70, red >90.
	ctx, ctxColor := piContextUsage(th, p.ContextUsed, p.ContextMax)
	if ctx != "" {
		stats = append(stats, th.FG256(ctxColor, ctx))
	}

	left := fmt.Sprintf(" (%s) %s ", p.Provider, p.Model)
	middle := " " + strings.Join(stats, " ") + " "

	var hint string
	if p.Busy {
		hint = " esc cancel "
	} else {
		hint = " ctrl+c exit " + sep + " /help "
	}

	var leftBuilder strings.Builder
	if p.BusyPrefix != "" {
		leftBuilder.WriteString(th.FG256(th.Accent, " "+p.BusyPrefix+" "))
		leftBuilder.WriteString("  ")
	}
	leftBuilder.WriteString(th.FG256(th.Muted, left))
	leftBuilder.WriteString("  ")
	// `middle` already has colorized context segments; wrap the rest in muted.
	leftBuilder.WriteString(th.FG256(th.Muted, middle))
	leftBuilder.WriteString("  ")
	leftBuilder.WriteString(th.FG256(th.Muted, hint))

	// Compose cwd on the right side. shortenHome abbreviates $HOME to "~".
	right := shortenHome(p.CWD)
	if p.Locked && right != "" {
		right = "· locked · " + right
	}
	if right == "" || p.Cols <= 0 {
		return leftBuilder.String()
	}

	leftRendered := leftBuilder.String()
	leftWidth := visibleWidth(leftRendered)
	rightStr := " " + right + " "
	rightWidth := visibleWidth(rightStr)

	gap := p.Cols - leftWidth - rightWidth
	if gap < 1 {
		return leftRendered
	}
	return leftRendered + strings.Repeat(" ", gap) + th.FG256(th.Muted, rightStr)
}

// piContextUsage renders the "N%/ctxMax" fragment, returning the
// rendered string plus the colour to wrap it in.
func piContextUsage(th Theme, used, max int) (string, int) {
	if max <= 0 {
		if used <= 0 {
			return "", th.Muted
		}
		return piFormatTokens(used), th.Muted
	}
	pct := float64(used) / float64(max) * 100
	text := fmt.Sprintf("%.1f%%/%s", pct, piFormatTokens(max))
	switch {
	case pct > 90:
		return text, th.Error
	case pct > 70:
		return text, th.Warning
	}
	return text, th.Muted
}

// piFormatTokens footer formatter:
//
//	< 1000      -> "42"
//	< 10_000    -> "2.7k"
//	< 1_000_000 -> "35k"
//	< 10M       -> "1.1M"
//	else        -> "12M"
func piFormatTokens(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%dM", (n+500_000)/1_000_000)
	}
}

// shortenHome replaces the user's $HOME prefix with "~" for readability.
func shortenHome(p string) string {
	if p == "" {
		return ""
	}
	home := osUserHome()
	if home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// osUserHome is a tiny wrapper around os.UserHomeDir so tests can mock it.
var osUserHome = func() string {
	if h, err := osUserHomeDir(); err == nil {
		return h
	}
	return ""
}
