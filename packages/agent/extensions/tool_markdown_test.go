package extensions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestToolMarkdownDisplayHint(t *testing.T) {
	m, ext, frames, replies := toolPeer(t)
	done := make(chan core.ToolResult, 1)
	go func() {
		result, err := NewTool(m, ToolInfo{Name: "ask", Extension: ext.Manifest.Name}).Execute(context.Background(), json.RawMessage(`{}`), nil)
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	call := toolCallFrame(t, frames)
	sendToolFrame(t, replies, extproto.ToolResultFromExt{Type: "tool_result", ID: call.ID, Content: []extproto.ContentBlock{
		{Type: "text", Text: "**finding**", Format: "markdown"},
	}})
	result := <-done
	if len(result.Content) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	block := result.Content[0].(provider.TextBlock)
	if block.Format != "markdown" || block.Text != "**finding**" {
		t.Fatalf("display hint lost: %+v", block)
	}
}
