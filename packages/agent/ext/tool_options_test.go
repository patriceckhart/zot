package ext

import (
	"context"
	"encoding/json"
	"testing"
)

func TestToolOptionsCombineStructuredDeferredAndContext(t *testing.T) {
	extension := New("synthetic", "test")
	extension.ToolWithOptions("query", "Search", json.RawMessage(`{"type":"object"}`), ToolOptions{
		OutputSchema: json.RawMessage(`{"type":"object"}`), Namespace: "service", NamespaceDescription: "Customer service", NamespaceInstructions: "Escalate urgent tickets", Exposure: "codemode", Deferred: true,
	}, func(ctx context.Context, _ json.RawMessage) ToolResult {
		return ToolResult{IsError: ctx.Err() != nil, StructuredContent: json.RawMessage(`{"ok":false}`)}
	})
	definition := extension.toolDefs[0]
	if definition.namespace != "service" || definition.namespaceDescription != "Customer service" || definition.namespaceInstructions != "Escalate urgent tickets" || definition.exposure != "codemode" || !definition.deferred || len(definition.outputSchema) == 0 {
		t.Fatalf("definition: %+v", definition)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := extension.tools["query"](ctx, json.RawMessage(`{}`))
	if !result.IsError || string(result.StructuredContent) != `{"ok":false}` {
		t.Fatalf("context or structured result lost: %+v", result)
	}
}
