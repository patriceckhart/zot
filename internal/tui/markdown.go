package tui

import (
	"regexp"
	"strings"
)

// FlushLeftSentinel was used previously to opt fenced code blocks
// out of the prose indent. The current rendering keeps fences
// aligned with surrounding prose, so the sentinel is no longer
// emitted; the constant is kept (and exported) so any older
// caller that still strips it remains a harmless no-op.
const FlushLeftSentinel = '\x1c'

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

	lines := strings.Split(src, "\n")
	var out strings.Builder
	var fenceBuf strings.Builder
	inFence := false
	fenceLang := ""
	fenceIndent := ""

	// flushFence emits the buffered fence content without decorative
	// horizontal rules. The tui draws rules around tool-result
	// boxes, where they delimit real content; inside assistant
	// prose they clutter the chat without adding information and
	// look particularly bad around one-line snippets like `rm -rf
	// foo`. Syntax highlighting alone is enough to signal "this is
	// code"; unambiguous because prose doesn't use the accent
	// palette.
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
			} else {
				inFence = true
				fenceIndent = line[:len(line)-len(trim)]
				fenceLang = strings.TrimSpace(strings.TrimPrefix(trim, "```"))
				// Rule will be emitted by flushFence once the
				// content is known so we can size it to the
				// widest line inside the fence.
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
	// Handle streaming / truncated input: the opening ``` arrived
	// but the closing one hasn't yet. Emit the buffered content
	// with both rules so the partial fence still reads cleanly.
	if inFence {
		flushFence()
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
