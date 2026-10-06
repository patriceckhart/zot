package continuous

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Extension contributes capabilities to the conversations that select it by
// name (AgentConfig.Extensions). It is code registered by the host, not data
// in the store: the store records only names, and every model request and
// tool call resolves the names against the registry at that moment, so a
// reload replaces an extension for the next use without touching work that
// already started.
type Extension struct {
	// Name is the stable identity conversations select.
	Name string
	// Tools are added to the host's tools. A name the host already offers
	// is rejected at use rather than silently shadowing it.
	Tools []core.Tool
	// System is appended to the system prompt, after the host prompt and
	// before the conversation's instructions.
	System string
	// Hooks observe and steer the built-in generation and tool tasks.
	Hooks Hooks
	// WrapTool, when set, wraps every tool the conversation can call,
	// including other extensions' tools. Wrappers apply in selection order.
	WrapTool func(core.Tool) core.Tool
	// Tasks are task definitions run by the same scheduler. A kind must be
	// unique across extensions and the built-in kinds.
	Tasks []TaskDefinition
}

// Hooks are the extension points of the built-in tasks. Each runs inside the
// task phase it belongs to, before that phase's commit, so whatever a hook
// decides commits atomically with the phase or not at all. Hooks must be
// deterministic with respect to committed state: after a crash a phase can
// run again and its hooks with it.
type Hooks struct {
	// BeforeRequest may change the system prompt and message context of a
	// model request. It runs after compaction and placement decisions.
	BeforeRequest func(ctx context.Context, req *HookRequest) error
	// AfterResponse observes the committed shape of an accepted response
	// before its commit. It may not change the response.
	AfterResponse func(ctx context.Context, c Conversation, msg provider.Message) error
	// OnYield runs when a response ends the run without tool calls.
	// Returning a non-empty prompt continues the run with it as the next
	// user message instead of answering.
	OnYield func(ctx context.Context, c Conversation, msg provider.Message) (string, error)
	// AfterTools runs after every result of a round committed. Returning
	// true ends the run without another model request.
	AfterTools func(ctx context.Context, c Conversation, results []ToolRoundResult) (bool, error)
	// BeforeTool runs before authorization. It may block the call with a
	// reason shown to the model, or rewrite its arguments.
	BeforeTool func(ctx context.Context, c Conversation, call provider.ToolCallBlock) (ToolDecision, error)
	// AfterTool may replace the result of an executed call before it
	// commits. It does not run for blocked or recovered calls.
	AfterTool func(ctx context.Context, c Conversation, call provider.ToolCallBlock, result core.ToolResult) (core.ToolResult, error)
}

// HookRequest is the mutable view of a model request for BeforeRequest.
type HookRequest struct {
	Conversation Conversation
	System       string
	Messages     []provider.Message
}

// ToolDecision is what BeforeTool decides. The zero value allows the call
// with its arguments unchanged.
type ToolDecision struct {
	Block  bool
	Reason string
	// Args, when set, replace the call's arguments. They must be valid
	// JSON; the effective arguments commit with the call's intent.
	Args json.RawMessage
}

// ToolRoundResult is one committed result of a round, for AfterTools.
type ToolRoundResult struct {
	CallID string
	Name   string
	Status string
}

// ExtensionRegistry holds the host's named extensions. It is safe for
// concurrent use; Register replaces an extension of the same name in place.
type ExtensionRegistry struct {
	mu   sync.RWMutex
	exts map[string]Extension
	// generation counts registrations, for diagnostics.
	generation uint64
}

// NewExtensionRegistry returns an empty registry.
func NewExtensionRegistry() *ExtensionRegistry {
	return &ExtensionRegistry{exts: map[string]Extension{}}
}

// Register adds or replaces an extension. Replacement takes effect at the
// next resolution; invocations that already resolved keep the old one.
func (r *ExtensionRegistry) Register(ext Extension) error {
	if strings.TrimSpace(ext.Name) == "" || strings.ContainsAny(ext.Name, "/\x00") {
		return fmt.Errorf("extension requires a name without slashes")
	}
	for _, def := range ext.Tasks {
		if err := def.check(); err != nil {
			return fmt.Errorf("extension %s: %w", ext.Name, err)
		}
		if strings.HasPrefix(def.Kind, "zot.") {
			return fmt.Errorf("extension %s: task kind %s uses the reserved zot. prefix", ext.Name, def.Kind)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, other := range r.exts {
		if name == ext.Name {
			continue
		}
		for _, def := range ext.Tasks {
			for _, o := range other.Tasks {
				if o.Kind == def.Kind {
					return fmt.Errorf("extension %s: task kind %s already registered by %s", ext.Name, def.Kind, name)
				}
			}
		}
	}
	r.exts[ext.Name] = ext
	r.generation++
	return nil
}

// Remove unregisters an extension. Conversations that still select it fail
// their next request or tool call with ErrExtensionMissing.
func (r *ExtensionRegistry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.exts, name)
	r.generation++
}

// Names lists registered extensions in name order.
func (r *ExtensionRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.exts))
	for name := range r.exts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ErrExtensionMissing reports a conversation selecting an extension the host
// does not register. The task fails closed instead of running without it.
var ErrExtensionMissing = fmt.Errorf("%w: extension not registered", ErrTaskPermanent)

// resolve returns the selected extensions in selection order.
func (r *ExtensionRegistry) resolve(names []string) ([]Extension, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrExtensionMissing, names[0])
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Extension, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		ext, ok := r.exts[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrExtensionMissing, name)
		}
		out = append(out, ext)
	}
	return out, nil
}

// task resolves a task kind registered by any extension.
func (r *ExtensionRegistry) task(kind string) (TaskDefinition, bool) {
	if r == nil {
		return TaskDefinition{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, ext := range r.exts {
		for _, def := range ext.Tasks {
			if def.Kind == kind {
				return def, true
			}
		}
	}
	return TaskDefinition{}, false
}

// resolvedExtensions is the set of extensions one invocation uses.
type resolvedExtensions []Extension

// apply adds the extensions' tools, prompt sections, and wrappers to an
// agent prepared for a conversation. It runs before the conversation's tool
// narrowing, so allow and deny lists apply to extension tools too.
func (exts resolvedExtensions) apply(agent *core.Agent) error {
	if len(exts) == 0 {
		return nil
	}
	tools := core.Registry{}
	for name, tool := range agent.Tools {
		tools[name] = tool
	}
	var sections []string
	for _, ext := range exts {
		for _, tool := range ext.Tools {
			if _, exists := tools[tool.Name()]; exists {
				return fmt.Errorf("%w: extension %s tool %s collides with an existing tool", ErrTaskPermanent, ext.Name, tool.Name())
			}
			tools[tool.Name()] = tool
		}
		if s := strings.TrimSpace(ext.System); s != "" {
			sections = append(sections, s)
		}
	}
	for _, ext := range exts {
		if ext.WrapTool == nil {
			continue
		}
		for name, tool := range tools {
			if wrapped := ext.WrapTool(tool); wrapped != nil {
				tools[name] = wrapped
			}
		}
	}
	agent.SetTools(tools)
	if len(sections) > 0 {
		agent.System = strings.TrimSpace(agent.System + "\n\n" + strings.Join(sections, "\n\n"))
	}
	return nil
}

func (exts resolvedExtensions) beforeRequest(ctx context.Context, req *HookRequest) error {
	for _, ext := range exts {
		if ext.Hooks.BeforeRequest != nil {
			if err := ext.Hooks.BeforeRequest(ctx, req); err != nil {
				return fmt.Errorf("extension %s before request: %w", ext.Name, err)
			}
		}
	}
	return nil
}

func (exts resolvedExtensions) afterResponse(ctx context.Context, c Conversation, msg provider.Message) error {
	for _, ext := range exts {
		if ext.Hooks.AfterResponse != nil {
			if err := ext.Hooks.AfterResponse(ctx, c, msg); err != nil {
				return fmt.Errorf("extension %s after response: %w", ext.Name, err)
			}
		}
	}
	return nil
}

// onYield returns the first continuation prompt any extension asks for.
func (exts resolvedExtensions) onYield(ctx context.Context, c Conversation, msg provider.Message) (string, error) {
	for _, ext := range exts {
		if ext.Hooks.OnYield != nil {
			prompt, err := ext.Hooks.OnYield(ctx, c, msg)
			if err != nil {
				return "", fmt.Errorf("extension %s on yield: %w", ext.Name, err)
			}
			if strings.TrimSpace(prompt) != "" {
				return prompt, nil
			}
		}
	}
	return "", nil
}

// afterTools reports whether any extension ends the run after the round.
func (exts resolvedExtensions) afterTools(ctx context.Context, c Conversation, results []ToolRoundResult) (bool, error) {
	for _, ext := range exts {
		if ext.Hooks.AfterTools != nil {
			stop, err := ext.Hooks.AfterTools(ctx, c, results)
			if err != nil {
				return false, fmt.Errorf("extension %s after tools: %w", ext.Name, err)
			}
			if stop {
				return true, nil
			}
		}
	}
	return false, nil
}

// beforeTool applies every extension's decision in order. The first block
// wins; argument rewrites chain.
func (exts resolvedExtensions) beforeTool(ctx context.Context, c Conversation, call provider.ToolCallBlock) (provider.ToolCallBlock, ToolDecision, error) {
	for _, ext := range exts {
		if ext.Hooks.BeforeTool == nil {
			continue
		}
		d, err := ext.Hooks.BeforeTool(ctx, c, call)
		if err != nil {
			return call, ToolDecision{}, fmt.Errorf("extension %s before tool: %w", ext.Name, err)
		}
		if d.Block {
			if d.Reason == "" {
				d.Reason = "tool call blocked by extension " + ext.Name
			}
			return call, d, nil
		}
		if len(d.Args) > 0 {
			if !json.Valid(d.Args) {
				return call, ToolDecision{}, fmt.Errorf("%w: extension %s rewrote arguments to invalid JSON", ErrTaskPermanent, ext.Name)
			}
			call.Arguments = append(json.RawMessage(nil), d.Args...)
		}
	}
	return call, ToolDecision{}, nil
}

func (exts resolvedExtensions) afterTool(ctx context.Context, c Conversation, call provider.ToolCallBlock, result core.ToolResult) (core.ToolResult, error) {
	for _, ext := range exts {
		if ext.Hooks.AfterTool != nil {
			replaced, err := ext.Hooks.AfterTool(ctx, c, call, result)
			if err != nil {
				return result, fmt.Errorf("extension %s after tool: %w", ext.Name, err)
			}
			result = replaced
		}
	}
	return result, nil
}
