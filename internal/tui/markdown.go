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
	var fenceBuf strings.Builder
	inFence := false
	fenceLang := ""
	fenceIndent := ""

	flushFence := func() {
		if fenceBuf.Len() == 0 {
			return
		}
		code := strings.TrimRight(fenceBuf.String(), "\n")
		if fenceLang != "" {
			for _, l := range HighlightCode(code, fenceLang) {
				out.WriteString(l)
				out.WriteString("\n")
			}
		} else {
			for _, l := range strings.Split(code, "\n") {
				out.WriteString(th.FG256(th.Accent, l))
				out.WriteString("\n")
			}
		}
		fenceBuf.Reset()
	}

	for _, line := range lines {
		trim := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trim, "```") {
			if inFence {
				flushFence()
				inFence = false
				fenceLang = ""
				out.WriteString(rule + "\n")
			} else {
				inFence = true
				fenceIndent = line[:len(line)-len(trim)]
				fenceLang = strings.TrimSpace(strings.TrimPrefix(trim, "```"))
				out.WriteString(rule + "\n")
			}
			continue
		}
		if inFence {
			if strings.HasPrefix(line, fenceIndent) {
				line = line[len(fenceIndent):]
			}
			fenceBuf.WriteString(line)
			fenceBuf.WriteString("\n")
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
