package core

import (
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestHydrateToolMarkdown(t *testing.T) {
	for _, format := range []string{"", "markdown", "unknown"} {
		msg := provider.Message{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: "call", Content: []provider.Content{
				provider.TextBlock{Text: "**finding**", Format: format},
			}},
		}}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		hydrated, err := hydrateMessageObject(raw)
		if err != nil {
			t.Fatal(err)
		}
		block := hydrated.Content[0].(provider.ToolResultBlock).Content[0].(provider.TextBlock)
		if block.Text != "**finding**" || block.Format != format {
			t.Fatalf("display hint lost during session round trip: %+v", block)
		}
	}
}
