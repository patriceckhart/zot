package tui

import (
	"regexp"
	"strings"
)

// RenderMarkdown renders a small subset of Markdown to styled terminal
// text using theme colors. Supported: headings, bold, italic, inline
// code, fenced code blocks, bullet lists, numbered lists, blockquotes.
// Not supported: tables, links with complex formatting, HTML.
//
// width is used to draw horizontal rules (e.g. around code fences).
// Pass 0 to use a reasonable fallback.
func RenderMarkdown(src string, th Theme, width int) string {
	if width <= 0 {
		width = 80
	}
	rule := th.FG256(th.Muted, strings.Repeat("─", width))

	lines := strings.Split(src, "\n")
	var out strings.Builder
	inFence := false
	fenceIndent := ""
	for _, line := range lines {
		trim := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trim, "```") {
			if inFence {
				// closing fence: emit bottom rule
				inFence = false
				out.WriteString(rule + "\n")
			} else {
				// opening fence: emit top rule
				inFence = true
				fenceIndent = line[:len(line)-len(trim)]
				out.WriteString(rule + "\n")
			}
			continue
		}
		if inFence {
			if strings.HasPrefix(line, fenceIndent) {
				line = line[len(fenceIndent):]
			}
			out.WriteString(th.FG256(th.Accent, line) + "\n")
			continue
		}
		// Headings.
		if m := headingRE.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			body := strings.TrimSpace(m[2])
			prefix := strings.Repeat("#", level) + " "
			out.WriteString(Bold(th.FG256(th.Accent, prefix+body)) + "\n")
			continue
		}
		// Blockquote.
		if strings.HasPrefix(trim, "> ") {
			body := strings.TrimPrefix(trim, "> ")
			out.WriteString(th.FG256(th.Muted, "┃ ") + renderInline(body, th) + "\n")
			continue
		}
		// Bullet list.
		if m := bulletRE.FindStringSubmatch(line); m != nil {
			indent, body := m[1], m[2]
			out.WriteString(indent + th.FG256(th.Accent, "• ") + renderInline(body, th) + "\n")
			continue
		}
		// Numbered list.
		if m := numberRE.FindStringSubmatch(line); m != nil {
			indent, num, body := m[1], m[2], m[3]
			out.WriteString(indent + th.FG256(th.Accent, num+". ") + renderInline(body, th) + "\n")
			continue
		}
		out.WriteString(renderInline(line, th) + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

var (
	headingRE = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	bulletRE  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	numberRE  = regexp.MustCompile(`^(\s*)(\d+)\.\s+(.*)$`)

	boldRE = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italRE = regexp.MustCompile(`\*([^*]+)\*`)
	codeRE = regexp.MustCompile("`([^`]+)`")
)

func renderInline(s string, th Theme) string {
	s = codeRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		return th.FG256(th.Accent, inner)
	})
	s = boldRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-2]
		return Bold(inner)
	})
	s = italRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		return Italic(inner)
	})
	return s
}
