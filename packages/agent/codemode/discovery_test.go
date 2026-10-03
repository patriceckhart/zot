package codemode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestDiscoverySchemasAndNamespaces(t *testing.T) {
	tools := []provider.Tool{
		{Name: "fetch-tickets", Namespace: "ext__support", Description: "Search customer tickets", Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`), OutputSchema: json.RawMessage(`{"type":"array","items":{"$ref":"#/$defs/ticket"},"$defs":{"ticket":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}}}`)},
		{Name: "weather", Namespace: "forecast", Description: "Get current temperature", Schema: json.RawMessage(`{"type":"object"}`)},
	}
	value, err := discoveryCall("searchTools", json.RawMessage(`["tickets",{"namespace":"support","limit":1}]`), tools)
	if err != nil || !strings.Contains(string(value), "fetch_tickets") || strings.Contains(string(value), "weather") {
		t.Fatalf("%s %v", value, err)
	}
	value, err = discoveryCall("describeTool", json.RawMessage(`["fetch_tickets"]`), tools)
	var declaration string
	decodeErr := json.Unmarshal(value, &declaration)
	if err != nil || decodeErr != nil || !strings.Contains(declaration, "query: string") || !strings.Contains(declaration, "limit?: number") || !strings.Contains(declaration, "Array<") {
		t.Fatalf("%s %v", value, err)
	}
	value, err = discoveryCall("describeNamespace", json.RawMessage(`["support"]`), tools)
	if err != nil || !strings.Contains(string(value), "fetch_tickets") {
		t.Fatalf("%s %v", value, err)
	}
	value, err = discoveryCall("describeTool", json.RawMessage(`["missing"]`), tools)
	if err != nil || value != nil {
		t.Fatalf("%s %v", value, err)
	}
}

func TestOnlyModeKeepsCallableRegistryAndDeferredDiscovery(t *testing.T) {
	reg := core.NewRegistry(&Tool{Mode: "only"}, testTool{name: "echo"})
	specs := reg.Specs()
	if len(specs) != 1 || !strings.Contains(specs[0].Description, "tools: { echo") || len(reg.AllSpecs()) != 2 {
		t.Fatalf("%+v", specs)
	}
	budget := 0
	reg["codemode"] = &Tool{Mode: "only", InlineBudget: &budget}
	if strings.Contains(reg.Specs()[0].Description, "declare const") {
		t.Fatal("zero budget inlined declarations")
	}
	tool := &Tool{Mode: "only"}
	specs = tool.PresentTools([]provider.Tool{{Name: "codemode"}, {Name: "hidden", Deferred: true, Schema: json.RawMessage(`{"type":"object"}`)}})
	// Deferred definitions remain available for later activation, without being
	// inlined or becoming eager solely because only mode is selected.
	if len(specs) != 2 || specs[1].Name != "hidden" || !specs[1].Deferred || strings.Contains(specs[0].Description, "declare const") {
		t.Fatalf("deferred exposure changed: %+v", specs)
	}
}
