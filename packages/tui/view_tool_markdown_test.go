package tui

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestToolMarkdownOptIn(t *testing.T) {
	text := "# Findings\n\n**Important** and `code`\n\n| Item | Status |\n| --- | --- |\n| Auth | Fixed |\n\n```go\nvar ready = true\n```"
	for _, format := range []string{"", "markdown", "unknown"} {
		for _, mode := range []string{"box", "flat", "compact"} {
			for _, width := range []int{30, 80} {
				v := View{Theme: Dark, ExpandAll: true, FlatTools: mode == "flat", CompactMode: mode == "compact"}
				blocks := []provider.Content{provider.TextBlock{Text: text, Format: format}}
				outputs := [][]string{
					v.renderToolResultContent(blocks, width, Dark.ToolOut, "", 1),
					v.RenderToolCall(ToolCallView{Name: "report", Done: true, Result: text, ResultContent: blocks}, width),
				}
				for _, rows := range outputs {
					plain := stripANSI(strings.Join(rows, "\n"))
					if !strings.Contains(plain, "Important") || !strings.Contains(plain, "code") {
						t.Fatalf("content lost (%s, %s, %d): %s", format, mode, width, plain)
					}
					if strings.Contains(plain, "**Important**") == (format == "markdown") {
						t.Fatalf("unexpected Markdown rendering (%s, %s, %d): %s", format, mode, width, plain)
					}
				}
			}
		}
	}
}

func TestToolMarkdownMixedBlocksAndCollapse(t *testing.T) {
	v := View{Theme: Dark}
	blocks := []provider.Content{
		provider.TextBlock{Text: "**literal**"},
		provider.TextBlock{Text: "**formatted**\n" + strings.Repeat("line\n", ToolCollapseLines+10), Format: "markdown"},
	}
	rows := v.renderToolResultContent(blocks, 80, Dark.ToolOut, "", 1)
	plain := stripANSI(strings.Join(rows, "\n"))
	if !strings.Contains(plain, "**literal**") || strings.Contains(plain, "**formatted**") || !strings.Contains(plain, "ctrl+o to expand") {
		t.Fatalf("unexpected mixed/collapsed output: %s", plain)
	}
	v.ExpandAll = true
	plain = stripANSI(strings.Join(v.renderToolResultContent(blocks, 80, Dark.ToolOut, "", 1), "\n"))
	if strings.Contains(plain, "ctrl+o to expand") {
		t.Fatalf("expanded output still collapsed: %s", plain)
	}
}
