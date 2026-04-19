// Package core implements the agent loop, tool runtime, and session
// persistence. It is provider-agnostic: it talks to an LLM only through
// the provider.Client interface.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/patriceckhart/zot/internal/provider"
)

// Tool is a capability the agent can invoke.
type Tool interface {
	// Name is the unique tool id shown to the LLM.
	Name() string
	// Description is a one-line summary shown to the LLM.
	Description() string
	// Schema is a JSON Schema object for Execute's args.
	Schema() json.RawMessage
	// Execute runs the tool. progress may be called any number of times
	// with partial textual output (for UIs); it is not sent to the LLM.
	Execute(ctx context.Context, args json.RawMessage, progress func(string)) (ToolResult, error)
}

// ToolResult is the outcome of Tool.Execute.
type ToolResult struct {
	// Content is sent back to the LLM (text and/or images).
	Content []provider.Content
	// IsError marks this result as an error to the LLM.
	IsError bool
	// Details is arbitrary data for UIs and logs; not sent to the LLM.
	Details any
}

// Registry is a name->Tool map.
type Registry map[string]Tool

// NewRegistry builds a Registry from a list of tools.
func NewRegistry(tools ...Tool) Registry {
	r := Registry{}
	for _, t := range tools {
		r[t.Name()] = t
	}
	return r
}

// Specs returns the tool definitions to advertise to the LLM.
// Sorted by tool name so the order is stable across requests. This
// is load-bearing for provider-side prompt caching: providers
// prefix-match tool definitions, and Go's map iteration order is
// randomized per call, which would otherwise bust the cache every
// single turn.
func (r Registry) Specs() []provider.Tool {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]provider.Tool, 0, len(r))
	for _, name := range names {
		t := r[name]
		out = append(out, provider.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return out
}

// Get looks up a tool by name.
func (r Registry) Get(name string) (Tool, error) {
	t, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return t, nil
}
