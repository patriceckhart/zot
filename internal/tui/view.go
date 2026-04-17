package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/patriceckhart/zot/internal/provider"
)

// osUserHomeDir is aliased so the test file can swap it.
var osUserHomeDir = os.UserHomeDir

// View turns a transcript + live state into a slice of styled lines,
// already wrapped to width.
type View struct {
	Theme           Theme
	ImageProto      ImageProtocol // how to render inline images in this terminal
	Messages        []provider.Message
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
				lines = append(lines, v.Theme.FG256(color, title))
				lines = append(lines, v.renderToolResultContent(tr.Content, width, color)...)
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
func (v *View) renderToolResultContent(blocks []provider.Content, width, color int) []string {
	rule := v.Theme.FG256(v.Theme.Muted, strings.Repeat("─", width))

	var out []string
	out = append(out, rule)
	for _, b := range blocks {
		switch bb := b.(type) {
		case provider.TextBlock:
			out = append(out, v.renderToolText(bb.Text, width, color)...)
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
func (v *View) renderToolText(text string, width, defaultColor int) []string {
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
func StatusBar(th Theme, model, prov string, cum provider.Usage, busy bool, busyPrefix, cwd string, locked bool, ctxUsed, ctxMax int, cols int) string {
	sep := " - "
	cost := fmt.Sprintf("$%.4f", cum.CostUSD)
	tokens := fmt.Sprintf("%d↑ %d↓", cum.InputTokens, cum.OutputTokens)
	left := fmt.Sprintf(" %s%s%s ", prov, sep, model)
	ctx := formatContextUsage(ctxUsed, ctxMax)
	middle := fmt.Sprintf(" %s%s%s%s%s ", tokens, sep, cost, sep, ctx)
	var hint string
	if busy {
		hint = " esc cancel "
	} else {
		hint = " ctrl+c exit " + sep + " /help "
	}

	var leftBuilder strings.Builder
	if busyPrefix != "" {
		leftBuilder.WriteString(th.FG256(th.Accent, " "+busyPrefix+" "))
		leftBuilder.WriteString("  ")
	}
	leftBuilder.WriteString(th.FG256(th.Muted, left))
	leftBuilder.WriteString("  ")
	leftBuilder.WriteString(th.FG256(th.Muted, middle))
	leftBuilder.WriteString("  ")
	leftBuilder.WriteString(th.FG256(th.Muted, hint))

	// Compose cwd on the right side. shortenHome abbreviates $HOME to "~".
	right := shortenHome(cwd)
	if locked && right != "" {
		right = "· locked · " + right
	}
	if right == "" || cols <= 0 {
		return leftBuilder.String()
	}

	leftRendered := leftBuilder.String()
	leftWidth := visibleWidth(leftRendered)
	rightStr := " " + right + " "
	rightWidth := visibleWidth(rightStr)

	gap := cols - leftWidth - rightWidth
	if gap < 1 {
		// Not enough room for the cwd; drop it silently to avoid wrapping.
		return leftRendered
	}
	return leftRendered + strings.Repeat(" ", gap) + th.FG256(th.Muted, rightStr)
}

// formatContextUsage returns a short string describing the share of the
// model's context window currently occupied by the latest turn's input.
func formatContextUsage(used, max int) string {
	if max <= 0 {
		if used <= 0 {
			return "ctx –"
		}
		return fmt.Sprintf("ctx %s", shortTokens(used))
	}
	pct := float64(used) / float64(max) * 100
	return fmt.Sprintf("ctx %s/%s (%.0f%%)", shortTokens(used), shortTokens(max), pct)
}

func shortTokens(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
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
