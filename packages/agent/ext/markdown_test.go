package ext

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestMarkdownResultWire(t *testing.T) {
	e := New("markdown", "test")
	var wire bytes.Buffer
	e.out = &wire
	e.respondTool("call", MarkdownResult("# Findings\n\n**Important**"))
	var decoded extproto.ToolResultFromExt
	if err := json.Unmarshal(wire.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Type != "text" || decoded.Content[0].Format != "markdown" || decoded.Content[0].Text != "# Findings\n\n**Important**" {
		t.Fatalf("unexpected wire result: %+v", decoded)
	}
	wire.Reset()
	e.respondTool("plain", TextResult("**literal**"))
	if bytes.Contains(wire.Bytes(), []byte(`"format"`)) {
		t.Fatalf("plain result gained a display hint: %s", wire.Bytes())
	}
}
