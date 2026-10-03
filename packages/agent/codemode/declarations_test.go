package codemode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestSchemaDeclarationContract(t *testing.T) {
	for _, tc := range []struct{ schema, want string }{
		{`{"enum":["a","a","b"]}`, `"a" | "b"`},
		{`{"anyOf":[{"type":"string"},true]}`, `unknown`},
		{`{"allOf":[{"anyOf":[{"type":"string"},{"type":"null"}]},true,{"type":"number"}]}`, `(string | null) & number`},
		{`{"type":"array","prefixItems":[{"type":"string"},{"type":"integer"}]}`, `[string, number]`},
		{`{"items":{"type":"boolean"}}`, `Array<boolean>`},
		{`{"type":"object","additionalProperties":false}`, `{}`},
		{`{"type":"object","additionalProperties":{"type":"number"}}`, `{ [key: string]: number; }`},
		{`{"type":"object","properties":{"value":{"$ref":"#"}}}`, `{ value?: { value?: unknown; }; }`},
		{`{"$ref":"#/$defs/a%20b","$defs":{"a b":{"type":"string"}}}`, `string`},
		{`{"type":"object","properties":{"b":{"type":"number"},"a":{"type":"string","description":"A field"}},"required":["a"]}`, "{\n  // A field\n  a: string;\n  b?: number;\n}"},
	} {
		if got := schemaType(json.RawMessage(tc.schema)); got != tc.want {
			t.Errorf("%s\ngot %s\nwant %s", tc.schema, got, tc.want)
		}
	}
}

func TestNamespaceMetadataDiscovery(t *testing.T) {
	tools := []provider.Tool{{Name: "answer", Namespace: "ext__support", NamespaceDescription: "Customer service", NamespaceInstructions: "Escalate urgent tickets", Schema: json.RawMessage(`{"type":"object"}`)}}
	raw, err := discoveryCall("describeNamespace", json.RawMessage(`["support"]`), tools)
	var got struct {
		Name, Description, Instructions string
		Tools                           []string
	}
	if err != nil || json.Unmarshal(raw, &got) != nil || got.Description != "Customer service" || got.Instructions != "Escalate urgent tickets" || len(got.Tools) != 1 {
		t.Fatalf("%s %v", raw, err)
	}
	matches := rankedTools("urgent tickets", tools, 8)
	if len(matches) != 1 || matches[0].Name != "answer" {
		t.Fatalf("namespace instructions not ranked: %+v", matches)
	}
}

func TestPresentationRespectsExposureAndNamespaceBudget(t *testing.T) {
	specs := []provider.Tool{{Name: "codemode"}, {Name: "direct", Exposure: "direct", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "script", Exposure: "codemode", Deferred: true, Namespace: "support", NamespaceDescription: "Customer service", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "hidden", Exposure: "deferred", Deferred: true}, {Name: "model", Exposure: "model-only"}}
	shown := (&Tool{Mode: "on"}).PresentTools(specs)
	var description string
	for _, tool := range shown {
		if tool.Name == "codemode" {
			description = tool.Description
		}
	}
	if !strings.Contains(description, "### `script`") || strings.Contains(description, "### `direct`") || strings.Contains(description, "### `hidden`") || !strings.Contains(description, "## support\nCustomer service") {
		t.Fatal(description)
	}
	zero := 0
	shown = (&Tool{Mode: "only", InlineBudget: &zero}).PresentTools(specs)
	for _, tool := range shown {
		if tool.Name == "direct" {
			t.Fatal("direct definition retained")
		}
		if tool.Name == "codemode" {
			if !strings.Contains(tool.Description, "support (tools not listed)") || strings.Contains(tool.Description, "declare const") {
				t.Fatal(tool.Description)
			}
		}
	}
}

func TestInlineBudgetUsesUTF16AndRoundRobin(t *testing.T) {
	tools := []provider.Tool{{Name: "a", Namespace: "a", Description: strings.Repeat("\U00010400", 10), Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "b", Namespace: "b", Schema: json.RawMessage(`{"type":"object"}`)}, {Name: "c", Namespace: "a", Description: strings.Repeat("x", 100), Schema: json.RawMessage(`{"type":"object"}`)}}
	budget := catalogCost(tools[0]) + catalogCost(tools[1])
	text := inlineCatalog(tools, budget)
	if !strings.Contains(text, "### `a`") || !strings.Contains(text, "### `b`") || strings.Contains(text, "### `c`") {
		t.Fatal(text)
	}
	if catalogCost(tools[0]) != (utf16Length(toolSection(tools[0]))+3)/4 {
		t.Fatal("budget did not count UTF-16")
	}
}

func TestMCPOutputDeclarations(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"content":{"type":"array","items":{"type":"object"}},"isError":{"type":"boolean"},"_meta":{"type":"object"},"structuredContent":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}}}`)
	tool := provider.Tool{Name: "structured", Schema: json.RawMessage(`{"type":"object"}`), OutputSchema: schema}
	if got := outputType(schema); got != "CallToolResult<{ value: string; }>" {
		t.Fatal(got)
	}
	text := inlineCatalog([]provider.Tool{tool}, 3000)
	if !strings.Contains(text, "Shared MCP Types:") || !strings.Contains(text, "Promise<CallToolResult<") {
		t.Fatal(text)
	}
}

func TestInlineBudgetChargesSharedMCPTypesOnce(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"content":{"type":"array","items":{"type":"object"}},"isError":{"type":"boolean"},"_meta":{"type":"object"}}}`)
	first := provider.Tool{Name: "a", Schema: json.RawMessage(`{"type":"object"}`), OutputSchema: schema}
	second := first
	second.Name = "b"
	sharedCost := (utf16Length("\n\nShared MCP Types:\n```ts\n"+mcpTypes+"\n```") + 3) / 4
	budget := catalogCost(first) + catalogCost(second) + sharedCost
	text := inlineCatalog([]provider.Tool{first, second}, budget)
	if strings.Count(text, "Shared MCP Types:") != 1 || !strings.Contains(text, "### `a`") || !strings.Contains(text, "### `b`") {
		t.Fatalf("shared types were not charged exactly once: %s", text)
	}
	text = inlineCatalog([]provider.Tool{first}, catalogCost(first)+sharedCost-1)
	if strings.Contains(text, "declare const") || strings.Contains(text, "Shared MCP Types:") {
		t.Fatalf("MCP declaration exceeded the budget: %s", text)
	}
	plain := provider.Tool{Name: "plain", Description: strings.Repeat("x", 100), Schema: first.Schema}
	text = inlineCatalog([]provider.Tool{first, plain}, catalogCost(plain))
	if !strings.Contains(text, "### `plain`") || strings.Contains(text, "### `a`") {
		t.Fatalf("unaffordable MCP types prevented a plain tool from fitting: %s", text)
	}
}
