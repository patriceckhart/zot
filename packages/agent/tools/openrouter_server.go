package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// OpenRouterServerToolNames lists every OpenRouter server tool that zot
// can advertise on Chat Completions. Shell, bash, apply_patch, and
// tool_search require Responses/Messages and are omitted here.
func OpenRouterServerToolNames() []string {
	return []string{
		"openrouter:web_search",
		"openrouter:web_fetch",
		"openrouter:advisor",
		"openrouter:subagent",
		"openrouter:fusion",
		"openrouter:image_generation",
		"openrouter:datetime",
		"openrouter:experimental__search_models",
	}
}

// IsOpenRouterServerTool reports whether name is an OpenRouter server tool
// that zot may inject into the live registry.
func IsOpenRouterServerTool(name string) bool {
	for _, n := range OpenRouterServerToolNames() {
		if n == name {
			return true
		}
	}
	return false
}

// OpenRouterServerTool is a stub advertised to the model so OpenRouter
// can execute the matching server tool. The agent never runs it locally.
type OpenRouterServerTool struct {
	name        string
	description string
	schema      json.RawMessage
}

func (t *OpenRouterServerTool) Name() string            { return t.name }
func (t *OpenRouterServerTool) Description() string     { return t.description }
func (t *OpenRouterServerTool) Schema() json.RawMessage { return t.schema }

func (t *OpenRouterServerTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("%s is executed by OpenRouter, not locally", t.name)}},
		IsError: true,
	}, nil
}

// OpenRouterServerTools returns stubs for every Chat Completions-capable
// OpenRouter server tool.
func OpenRouterServerTools() []core.Tool {
	return []core.Tool{
		&OpenRouterServerTool{
			name:        "openrouter:web_search",
			description: "Search the web for current information. Executed by OpenRouter.",
		},
		&OpenRouterServerTool{
			name:        "openrouter:web_fetch",
			description: "Fetch and extract content from a URL. Executed by OpenRouter.",
		},
		&OpenRouterServerTool{
			name:        "openrouter:advisor",
			description: "Consult a stronger model for guidance mid-generation. Executed by OpenRouter.",
			schema:      json.RawMessage(`{"model":"openai/gpt-4.1-mini"}`),
		},
		&OpenRouterServerTool{
			name:        "openrouter:subagent",
			description: "Delegate a self-contained task to a smaller worker model. Executed by OpenRouter.",
			schema:      json.RawMessage(`{"model":"openai/gpt-4.1-mini"}`),
		},
		&OpenRouterServerTool{
			name:        "openrouter:fusion",
			description: "Run a panel of models and an analyst for multi-model analysis. Executed by OpenRouter.",
		},
		&OpenRouterServerTool{
			name:        "openrouter:image_generation",
			description: "Generate an image from a text prompt. Executed by OpenRouter.",
		},
		&OpenRouterServerTool{
			name:        "openrouter:datetime",
			description: "Get the current date and time. Executed by OpenRouter.",
		},
		&OpenRouterServerTool{
			name:        "openrouter:experimental__search_models",
			description: "Search and filter the OpenRouter model catalog. Executed by OpenRouter.",
			schema:      json.RawMessage(`{"max_results":5}`),
		},
	}
}
