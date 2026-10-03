package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestStructuredToolResultWire(t *testing.T) {
	e := New("structured", "test")
	schema := json.RawMessage(`{"type":"object"}`)
	outputSchema := json.RawMessage(`{"type":"object","properties":{"exit":{"type":"integer"}}}`)
	e.StructuredTool("command", "Run command", schema, outputSchema, func(json.RawMessage) ToolResult {
		return ToolResult{IsError: true, Content: []ToolContent{{Type: "text", Text: "failed"}}, StructuredContent: json.RawMessage(`{"exit":7}`)}
	})
	if len(e.toolDefs) != 1 || !bytes.Equal(e.toolDefs[0].outputSchema, outputSchema) {
		t.Fatalf("%+v", e.toolDefs)
	}
	var wire bytes.Buffer
	e.out = &wire
	result := e.tools["command"](context.Background(), json.RawMessage(`{}`))
	e.respondTool("call", result)
	var decoded extproto.ToolResultFromExt
	if err := json.Unmarshal(wire.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.IsError || decoded.ID != "call" || string(decoded.StructuredContent) != `{"exit":7}` || len(decoded.Content) != 1 || decoded.Content[0].Text != "failed" {
		t.Fatalf("%+v", decoded)
	}
}
