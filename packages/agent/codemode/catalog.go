package codemode

import (
	"sort"
	"strings"

	executor "github.com/patriceckhart/zot/packages/codemode"
	"github.com/patriceckhart/zot/packages/provider"
)

// Shared types are included only when an inline declaration uses CallToolResult.
const mcpTypes = `type Role = "user" | "assistant";
type MetaObject = Record<string, unknown>;
type Annotations = { audience?: Role[]; priority?: number; lastModified?: string; };
type Icon = { src: string; mimeType?: string; sizes?: string[]; theme?: "light" | "dark"; };
type TextResourceContents = { uri: string; mimeType?: string; _meta?: MetaObject; text: string; };
type BlobResourceContents = { uri: string; mimeType?: string; _meta?: MetaObject; blob: string; };
type TextContent = { type: "text"; text: string; annotations?: Annotations; _meta?: MetaObject; };
type ImageContent = { type: "image"; data: string; mimeType: string; annotations?: Annotations; _meta?: MetaObject; };
type AudioContent = { type: "audio"; data: string; mimeType: string; annotations?: Annotations; _meta?: MetaObject; };
type ResourceLink = { type: "resource_link"; icons?: Icon[]; name: string; title?: string; uri: string; description?: string; mimeType?: string; annotations?: Annotations; size?: number; _meta?: MetaObject; };
type EmbeddedResource = { type: "resource"; resource: TextResourceContents | BlobResourceContents; annotations?: Annotations; _meta?: MetaObject; };
type ContentBlock = TextContent | ImageContent | AudioContent | ResourceLink | EmbeddedResource;
type CallToolResult<TStructured = { [key: string]: unknown }> = { _meta?: MetaObject; content: ContentBlock[]; isError?: boolean; structuredContent?: TStructured; [key: string]: unknown; };`

const mcpTypeSection = "\n\nShared MCP Types:\n```ts\n" + mcpTypes + "\n```"

func toolSection(tool provider.Tool) string {
	id := executor.Identifier(tool.Name)
	heading := "### `" + id + "`"
	if id != tool.Name {
		heading += " (`" + tool.Name + "`)"
	}
	return heading + "\n" + strings.TrimSpace(toolSample(tool))
}

func inlineCatalog(tools []provider.Tool, budget int) string {
	if len(tools) == 0 {
		return ""
	}
	groups := map[string][]provider.Tool{}
	names := []string{}
	for _, tool := range tools {
		if _, exists := groups[tool.Namespace]; !exists {
			names = append(names, tool.Namespace)
		}
		groups[tool.Namespace] = append(groups[tool.Namespace], tool)
	}
	sort.Strings(names)
	queues := map[string][]provider.Tool{}
	for _, name := range names {
		queues[name] = append([]provider.Tool(nil), groups[name]...)
	}
	shown := map[string]bool{}
	sharedTypes := false
	for {
		added := false
		for _, name := range names {
			queue := queues[name]
			if len(queue) == 0 {
				continue
			}
			best, cost := -1, 0
			for i, tool := range queue {
				candidateCost := catalogCost(tool)
				if _, mcp := mcpStructuredSchema(tool.OutputSchema); mcp && !sharedTypes {
					candidateCost += (utf16Length(mcpTypeSection) + 3) / 4
				}
				if candidateCost <= budget && (best == -1 || candidateCost < cost) {
					best, cost = i, candidateCost
				}
			}
			if best == -1 {
				continue
			}
			budget -= cost
			shown[queue[best].Name] = true
			if _, mcp := mcpStructuredSchema(queue[best].OutputSchema); mcp {
				sharedTypes = true
			}
			queues[name] = append(queue[:best], queue[best+1:]...)
			added = true
		}
		if !added {
			break
		}
	}
	var out strings.Builder
	if sharedTypes {
		out.WriteString(mcpTypeSection)
	}
	out.WriteString("\n\nNested tools:")
	for _, name := range names {
		visible := 0
		for _, tool := range groups[name] {
			if shown[tool.Name] {
				visible++
			}
		}
		if name != "" {
			suffix := ""
			if visible == 0 {
				suffix = " (tools not listed)"
			} else if visible < len(groups[name]) {
				suffix = " (some tools not listed)"
			}
			out.WriteString("\n\n## " + name + suffix)
			if description := strings.TrimSpace(groups[name][0].NamespaceDescription); description != "" {
				out.WriteString("\n" + description)
			}
		}
		for _, tool := range groups[name] {
			if shown[tool.Name] {
				out.WriteString("\n\n" + toolSection(tool))
			}
		}
	}
	return out.String()
}

func catalogCost(tool provider.Tool) int { return (utf16Length(toolSection(tool)) + 3) / 4 }
