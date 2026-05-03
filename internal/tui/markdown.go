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
// code, fenced code blocks, bullet lists, numbered lists, blockquotes,
// simple GitHub-style tables.
// Not supported: links with complex formatting, HTML.
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

	for idx := 0; idx < len(lines); idx++ {
		line := lines[idx]
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
		// GitHub-style table blocks. Detect a header row followed by
		// a separator row like "| --- | ---: |" and consume all
		// following pipe rows as table body. Done before inline
		// rendering so cell widths can be measured and padded.
		if idx+1 < len(lines) && looksLikeTableHeader(line, lines[idx+1]) {
			block := []string{line, lines[idx+1]}
			j := idx + 2
			for j < len(lines) && looksLikeTableRow(lines[j]) {
				block = append(block, lines[j])
				j++
			}
			for _, rendered := range renderTable(block, th, width) {
				out.WriteString(rendered)
				out.WriteString("\n")
			}
			idx = j - 1
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
	headingRE    = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	bulletRE     = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	numberRE     = regexp.MustCompile(`^(\s*)(\d+)\.\s+(.*)$`)
	tableSepCell = regexp.MustCompile(`^:?-{3,}:?$`)

	boldRE = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italRE = regexp.MustCompile(`\*([^*]+)\*`)
	codeRE = regexp.MustCompile("`([^`]+)`")
)

type tableAlign int

const (
	tableAlignLeft tableAlign = iota
	tableAlignCenter
	tableAlignRight
)

func looksLikeTableHeader(header, sep string) bool {
	h := splitTableRow(header)
	s, ok := parseTableSeparator(sep)
	return ok && len(h) >= 2 && len(s) == len(h)
}

func looksLikeTableRow(line string) bool {
	cells := splitTableRow(line)
	return len(cells) >= 2
}

func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "|") {
		return nil
	}
	if strings.HasPrefix(line, "|") {
		line = strings.TrimPrefix(line, "|")
	}
	if strings.HasSuffix(line, "|") {
		line = strings.TrimSuffix(line, "|")
	}
	parts := splitUnescapedPipes(line)
	for i := range parts {
		parts[i] = strings.TrimSpace(strings.ReplaceAll(parts[i], `\|`, "|"))
	}
	return parts
}

func splitUnescapedPipes(s string) []string {
	var parts []string
	var b strings.Builder
	escaped := false
	for _, r := range s {
		if escaped {
			if r != '|' {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '|' {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	parts = append(parts, b.String())
	return parts
}

func parseTableSeparator(line string) ([]tableAlign, bool) {
	cells := splitTableRow(line)
	if len(cells) < 2 {
		return nil, false
	}
	aligns := make([]tableAlign, len(cells))
	for i, cell := range cells {
		compact := strings.ReplaceAll(strings.TrimSpace(cell), " ", "")
		if !tableSepCell.MatchString(compact) {
			return nil, false
		}
		left := strings.HasPrefix(compact, ":")
		right := strings.HasSuffix(compact, ":")
		switch {
		case left && right:
			aligns[i] = tableAlignCenter
		case right:
			aligns[i] = tableAlignRight
		default:
			aligns[i] = tableAlignLeft
		}
	}
	return aligns, true
}

func renderTable(block []string, th Theme, maxWidth int) []string {
	if len(block) < 2 {
		return block
	}
	aligns, ok := parseTableSeparator(block[1])
	if !ok {
		return block
	}
	rows := make([][]string, 0, len(block)-1)
	rows = append(rows, splitTableRow(block[0]))
	for _, line := range block[2:] {
		cells := splitTableRow(line)
		if len(cells) == 0 {
			continue
		}
		rows = append(rows, cells)
	}
	cols := len(aligns)
	widths := make([]int, cols)
	rendered := make([][]string, len(rows))
	for r, row := range rows {
		rendered[r] = make([]string, cols)
		for c := 0; c < cols; c++ {
			cell := ""
			if c < len(row) {
				cell = row[c]
			}
			styled := renderInline(cell, th)
			rendered[r][c] = styled
			if w := visibleWidth(styled); w > widths[c] {
				widths[c] = w
			}
		}
	}
	for i := range widths {
		if widths[i] < 3 {
			widths[i] = 3
		}
	}
	fitTableWidths(widths, maxWidth)

	out := make([]string, 0, len(rows)+1)
	out = append(out, renderTableRow(rendered[0], widths, aligns, th, true)...)
	out = append(out, renderTableSeparator(widths, aligns, th))
	for _, row := range rendered[1:] {
		out = append(out, renderTableRow(row, widths, aligns, th, false)...)
	}
	return out
}

func fitTableWidths(widths []int, maxWidth int) {
	cols := len(widths)
	if cols == 0 || maxWidth <= 0 {
		return
	}
	// Each column contributes one leading and one trailing space;
	// there are cols+1 pipe separators. The rest is cell content.
	avail := maxWidth - (cols + 1) - 2*cols
	if avail < cols*3 {
		avail = cols * 3
	}
	for sumInts(widths) > avail {
		idx := widestColumn(widths)
		if idx < 0 || widths[idx] <= 3 {
			return
		}
		widths[idx]--
	}
}

func sumInts(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}

func widestColumn(widths []int) int {
	idx := -1
	best := 0
	for i, w := range widths {
		if w > best {
			idx = i
			best = w
		}
	}
	return idx
}

func renderTableRow(row []string, widths []int, aligns []tableAlign, th Theme, header bool) []string {
	wrapped := make([][]string, len(widths))
	height := 1
	for c := range widths {
		cell := ""
		if c < len(row) {
			cell = row[c]
		}
		parts := wrapANSILine(cell, widths[c])
		if len(parts) == 0 {
			parts = []string{""}
		}
		if header {
			for i := range parts {
				parts[i] = Bold(parts[i])
			}
		}
		wrapped[c] = parts
		if len(parts) > height {
			height = len(parts)
		}
	}

	out := make([]string, 0, height)
	for r := 0; r < height; r++ {
		var b strings.Builder
		b.WriteString(th.FG256(th.Muted, "|"))
		for c := range widths {
			cell := ""
			if r < len(wrapped[c]) {
				cell = wrapped[c][r]
			}
			b.WriteByte(' ')
			b.WriteString(alignCell(cell, widths[c], aligns[c]))
			b.WriteByte(' ')
			b.WriteString(th.FG256(th.Muted, "|"))
		}
		out = append(out, b.String())
	}
	return out
}

func renderTableSeparator(widths []int, aligns []tableAlign, th Theme) string {
	var b strings.Builder
	b.WriteString(th.FG256(th.Muted, "|"))
	for c, w := range widths {
		dashes := strings.Repeat("-", w)
		switch aligns[c] {
		case tableAlignCenter:
			dashes = ":" + strings.Repeat("-", maxInt(1, w-2)) + ":"
		case tableAlignRight:
			dashes = strings.Repeat("-", maxInt(1, w-1)) + ":"
		}
		b.WriteByte(' ')
		b.WriteString(th.FG256(th.Muted, dashes))
		b.WriteByte(' ')
		b.WriteString(th.FG256(th.Muted, "|"))
	}
	return b.String()
}

func alignCell(s string, width int, align tableAlign) string {
	pad := width - visibleWidth(s)
	if pad <= 0 {
		return s
	}
	switch align {
	case tableAlignRight:
		return strings.Repeat(" ", pad) + s
	case tableAlignCenter:
		left := pad / 2
		right := pad - left
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
	default:
		return s + strings.Repeat(" ", pad)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

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
