package modes

import (
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestInteractiveToolResultPreservesMarkdown(t *testing.T) {
	i := &Interactive{toolCalls: map[string]*tui.ToolCallView{"call": {ID: "call", Name: "report"}}}
	i.handleEvent(core.EvToolResult{ID: "call", Result: core.ToolResult{Content: []provider.Content{
		provider.TextBlock{Text: "**finding**", Format: "markdown"},
	}}})
	tool := i.toolCalls["call"]
	if !tool.Done || tool.Result != "**finding**" || len(tool.ResultContent) != 1 || tool.ResultContent[0].(provider.TextBlock).Format != "markdown" {
		t.Fatalf("tool display hint lost: %+v", tool)
	}
}

func TestBtwToolResultPreservesMarkdown(t *testing.T) {
	d := &btwDialog{turns: []btwTurn{{Tools: []tui.ToolCallView{{ID: "call", Name: "report"}}}}}
	d.handleAgentEvent(0, core.EvToolResult{ID: "call", Result: core.ToolResult{Content: []provider.Content{
		provider.TextBlock{Text: "**finding**", Format: "markdown"},
	}}})
	tool := d.turns[0].Tools[0]
	if !tool.Done || tool.Result != "**finding**" || len(tool.ResultContent) != 1 || tool.ResultContent[0].(provider.TextBlock).Format != "markdown" {
		t.Fatalf("tool display hint lost: %+v", tool)
	}
}
