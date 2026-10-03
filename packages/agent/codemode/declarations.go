package codemode

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	executor "github.com/patriceckhart/zot/packages/codemode"
	"github.com/patriceckhart/zot/packages/provider"
)

type schemaContext struct {
	root       any
	refs       map[string]bool
	expansions int
}

func schemaType(raw json.RawMessage) string {
	var schema any
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil {
		return "unknown"
	}
	return (&schemaContext{root: schema, refs: map[string]bool{}}).render(schema)
}

func typeUnion(parts []string) string {
	seen := map[string]bool{}
	unique := []string{}
	for _, part := range parts {
		if part == "unknown" {
			return "unknown"
		}
		if !seen[part] {
			seen[part] = true
			unique = append(unique, part)
		}
	}
	if len(unique) == 0 {
		return "never"
	}
	return strings.Join(unique, " | ")
}

func (c *schemaContext) render(value any) string {
	schema, ok := value.(map[string]any)
	if !ok {
		if value == false {
			return "never"
		}
		return "unknown"
	}
	if ref, ok := schema["$ref"].(string); ok {
		if (ref != "#" && !strings.HasPrefix(ref, "#/")) || c.refs[ref] || c.expansions >= 32 {
			return "unknown"
		}
		resolved := c.root
		if ref != "#" {
			for _, segment := range strings.Split(ref[2:], "/") {
				if segment == "" {
					continue
				}
				part, err := url.PathUnescape(segment)
				if err != nil {
					return "unknown"
				}
				object, ok := resolved.(map[string]any)
				if !ok {
					return "unknown"
				}
				resolved = object[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
			}
		}
		c.expansions++
		c.refs[ref] = true
		out := c.render(resolved)
		delete(c.refs, ref)
		return out
	}
	if value, ok := schema["const"]; ok {
		raw, _ := json.Marshal(value)
		return string(raw)
	}
	if values, ok := schema["enum"].([]any); ok {
		parts := []string{}
		for _, value := range values {
			raw, _ := json.Marshal(value)
			parts = append(parts, string(raw))
		}
		return typeUnion(parts)
	}
	for _, name := range []string{"anyOf", "oneOf"} {
		if values, ok := schema[name].([]any); ok {
			parts := []string{}
			for _, value := range values {
				parts = append(parts, c.render(value))
			}
			return typeUnion(parts)
		}
	}
	if values, ok := schema["allOf"].([]any); ok {
		parts := []string{}
		for _, value := range values {
			part := c.render(value)
			if part == "unknown" {
				continue
			}
			if strings.Contains(part, " | ") {
				part = "(" + part + ")"
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			return "unknown"
		}
		return strings.Join(parts, " & ")
	}
	if types, ok := schema["type"].([]any); ok {
		parts := []string{}
		for _, kind := range types {
			copy := map[string]any{}
			for k, v := range schema {
				copy[k] = v
			}
			copy["type"] = kind
			parts = append(parts, c.render(copy))
		}
		return typeUnion(parts)
	}
	kind, _ := schema["type"].(string)
	switch kind {
	case "string", "boolean", "null":
		return kind
	case "integer", "number":
		return "number"
	case "array":
		return c.array(schema)
	case "object":
		return c.object(schema)
	case "":
		for _, key := range []string{"properties", "additionalProperties", "required"} {
			if _, ok := schema[key]; ok {
				return c.object(schema)
			}
		}
		for _, key := range []string{"items", "prefixItems"} {
			if _, ok := schema[key]; ok {
				return c.array(schema)
			}
		}
	}
	return "unknown"
}

func (c *schemaContext) array(schema map[string]any) string {
	if items, ok := schema["items"]; ok {
		if _, tuple := items.([]any); !tuple {
			return "Array<" + c.render(items) + ">"
		}
	}
	tuple, _ := schema["prefixItems"].([]any)
	if tuple == nil {
		tuple, _ = schema["items"].([]any)
	}
	if len(tuple) == 0 {
		return "unknown[]"
	}
	parts := []string{}
	for _, item := range tuple {
		parts = append(parts, c.render(item))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func propertyDescription(value any) string {
	if object, ok := value.(map[string]any); ok {
		if text, ok := object["description"].(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func (c *schemaContext) object(schema map[string]any) string {
	properties, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if names, ok := schema["required"].([]any); ok {
		for _, name := range names {
			if s, ok := name.(string); ok {
				required[s] = true
			}
		}
	}
	names := []string{}
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	members := []string{}
	descriptions := false
	for _, name := range names {
		key := name
		if executor.Identifier(name) != name {
			key = strconv.Quote(name)
		}
		if !required[name] {
			key += "?"
		}
		members = append(members, key+": "+c.render(properties[name])+";")
		descriptions = descriptions || propertyDescription(properties[name]) != ""
	}
	additional, present := schema["additionalProperties"]
	if present && additional != false {
		members = append(members, "[key: string]: "+c.render(additional)+";")
	} else if !present && len(names) == 0 {
		members = append(members, "[key: string]: unknown;")
	}
	if len(members) == 0 {
		return "{}"
	}
	if !descriptions {
		return "{ " + strings.Join(members, " ") + " }"
	}
	lines := []string{"{"}
	for i, name := range names {
		for _, line := range strings.Split(strings.ReplaceAll(propertyDescription(properties[name]), "\r\n", "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, "  // "+strings.TrimSpace(line))
			}
		}
		lines = append(lines, "  "+strings.ReplaceAll(members[i], "\n", "\n  "))
	}
	for _, member := range members[len(names):] {
		lines = append(lines, "  "+member)
	}
	return strings.Join(append(lines, "}"), "\n")
}

func mcpStructuredSchema(raw json.RawMessage) (json.RawMessage, bool) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return nil, false
	}
	var content struct {
		Type  string `json:"type"`
		Items struct {
			Type string `json:"type"`
		} `json:"items"`
	}
	var isError, meta struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(schema.Properties["content"], &content) != nil || content.Type != "array" || content.Items.Type != "object" || json.Unmarshal(schema.Properties["isError"], &isError) != nil || isError.Type != "boolean" || json.Unmarshal(schema.Properties["_meta"], &meta) != nil || meta.Type != "object" {
		return nil, false
	}
	structured := schema.Properties["structuredContent"]
	if len(structured) == 0 {
		structured = json.RawMessage(`true`)
	}
	return structured, true
}

func outputType(raw json.RawMessage) string {
	if structured, mcp := mcpStructuredSchema(raw); mcp {
		value := schemaType(structured)
		if value == "unknown" {
			return "CallToolResult"
		}
		return "CallToolResult<" + value + ">"
	}
	if len(raw) == 0 {
		return "string"
	}
	return schemaType(raw)
}

func toolSample(tool provider.Tool) string {
	input := schemaType(tool.Schema)
	if utf16Length(input) > 16000 {
		input = "unknown"
	}
	declaration := "declare const tools: { " + executor.Identifier(tool.Name) + "(args: " + input + "): Promise<" + outputType(tool.OutputSchema) + ">; };"
	return strings.TrimSpace(tool.Description) + "\n\ncodemode tool declaration:\n```ts\n" + declaration + "\n```"
}

func utf16Length(text string) int { return len(utf16.Encode([]rune(text))) }

func namespaceMatches(namespace, query string) bool {
	if namespace == query || executor.Identifier(namespace) == executor.Identifier(query) {
		return true
	}
	if index := strings.LastIndex(namespace, "__"); index >= 0 {
		return namespace[index+2:] == query || executor.Identifier(namespace[index+2:]) == executor.Identifier(query)
	}
	return false
}
