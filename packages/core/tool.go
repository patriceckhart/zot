// Package core implements the agent loop, tool runtime, and session
// persistence. It is provider-agnostic: it talks to an LLM only through
// the provider.Client interface.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/patriceckhart/zot/packages/provider"
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

// ToolPreviewer is optionally implemented by tools that can describe their
// exact effect without applying it. Confirmation UIs use the preview before
// allowing a side-effecting call to proceed.
type ToolPreviewer interface {
	Preview(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// ToolPolicyError marks a tool-local policy refusal without changing its text.
// It does not imply that other calls in the same batch were rolled back.
type ToolPolicyError struct{ Err error }

func (e *ToolPolicyError) Error() string { return e.Err.Error() }
func (e *ToolPolicyError) Unwrap() error { return e.Err }

// ToolResult is the outcome of Tool.Execute.
type ToolResult struct {
	// Content is sent back to the LLM (text and/or images).
	Content []provider.Content
	// IsError marks this result as an error to the LLM.
	IsError bool
	// Status optionally refines an error result for lifecycle observers.
	// Tools that return cancellation as content rather than an error may set
	// cancelled or timed_out; policy refusals may set blocked.
	Status string
	// ActivateTools names previously deferred tools that become available
	// after this result. Unknown names are ignored by the agent.
	ActivateTools []string
	// StructuredContent is a JSON result for tools declaring an output schema.
	StructuredContent json.RawMessage
	// State contains successful tool-state snapshots, persisted as message metadata.
	// It is never sent as model-visible content.
	State map[string]json.RawMessage
	// NestedCalls are display-only records collected by the runtime. They are
	// persisted as metadata, never as model-visible tool results.
	NestedCalls []provider.NestedToolCall
	// Usage reports non-chat inference performed by this tool.
	Usage *provider.Usage
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
	out := r.AllSpecs()
	for _, spec := range out {
		if presenter, ok := r[spec.Name].(interface {
			PresentTools([]provider.Tool) []provider.Tool
		}); ok {
			out = presenter.PresentTools(out)
		}
	}
	return out
}

// AllSpecs includes callable tools hidden by an orchestration presenter.
func (r Registry) AllSpecs() []provider.Tool {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]provider.Tool, 0, len(r))
	for _, name := range names {
		t := r[name]
		deferred := false
		if d, ok := t.(interface{ Deferred() bool }); ok {
			deferred = d.Deferred()
		}
		exposure := "direct"
		if deferred {
			exposure = "deferred"
		}
		if exposed, ok := t.(interface{ Exposure() string }); ok && exposed.Exposure() != "" {
			exposure = exposed.Exposure()
		}
		deferred = exposure == "deferred" || exposure == "codemode"
		var namespaceDescription, namespaceInstructions string
		if named, ok := t.(interface{ NamespaceDescription() string }); ok {
			namespaceDescription = named.NamespaceDescription()
		}
		if named, ok := t.(interface{ NamespaceInstructions() string }); ok {
			namespaceInstructions = named.NamespaceInstructions()
		}
		var outputSchema json.RawMessage
		if structured, ok := t.(interface{ OutputSchema() json.RawMessage }); ok {
			outputSchema = structured.OutputSchema()
		}
		var namespace string
		if named, ok := t.(interface{ Namespace() string }); ok {
			namespace = named.Namespace()
		}
		var sampling *provider.ConstrainedSampling
		if constrained, ok := t.(interface {
			ConstrainedSampling() *provider.ConstrainedSampling
		}); ok {
			sampling = constrained.ConstrainedSampling()
		}
		out = append(out, provider.Tool{
			ConstrainedSampling:   sampling,
			Exposure:              exposure,
			NamespaceDescription:  namespaceDescription,
			NamespaceInstructions: namespaceInstructions,
			Namespace:             namespace,
			OutputSchema:          outputSchema,
			Name:                  t.Name(),
			Description:           t.Description(),
			Schema:                t.Schema(),
			Deferred:              deferred,
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
