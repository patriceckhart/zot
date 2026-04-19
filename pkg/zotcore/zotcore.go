// Package zotcore is the public Go SDK for embedding the zot agent
// runtime in third-party programs. It is the only stable, importable
// surface the project exposes; everything under internal/ is subject
// to change without notice.
//
// Lifecycle:
//
//	rt, err := zotcore.New(zotcore.Config{Provider: "anthropic", Model: "claude-sonnet-4-5"})
//	if err != nil { ... }
//	defer rt.Close()
//
//	events, err := rt.Prompt(ctx, "fix the failing test", nil)
//	if err != nil { ... }
//	for ev := range events {
//	    fmt.Printf("%s: %+v\n", ev.Type(), ev)
//	}
//
// Concurrency model: one prompt at a time per Runtime. Spawn one
// Runtime per project / cwd. The Cancel call interrupts the active
// prompt; subsequent prompts work normally.
//
// For a non-Go consumer, run `zot rpc` and speak the same JSON
// schema over stdin/stdout. See docs/rpc.md.
package zotcore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/patriceckhart/zot/internal/agent"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// Config configures a Runtime. All fields are optional; sensible
// defaults are read from $ZOT_HOME/config.json, env vars, and the
// resolver chain (the same one the cli uses).
type Config struct {
	// Provider is "anthropic" or "openai". Empty = use the user's
	// default from config.json or env.
	Provider string

	// Model is the model id. Empty = use the provider's default.
	Model string

	// CWD is the working directory the agent operates in. Tools are
	// confined to this directory when Lock is true. Empty = current
	// process cwd.
	CWD string

	// SystemPrompt overrides the built-in system prompt.
	SystemPrompt string

	// AppendSystemPrompt is appended to the built-in (or overridden)
	// system prompt. Useful for project-specific instructions.
	AppendSystemPrompt []string

	// Reasoning sets the reasoning effort for models that support it
	// ("low", "medium", "high"). Empty = no reasoning.
	Reasoning string

	// MaxSteps caps the agent loop iterations per Prompt call.
	// 0 uses the default (50).
	MaxSteps int

	// APIKey overrides the credential lookup chain.
	APIKey string

	// BaseURL overrides the provider base url (for tests / proxies).
	BaseURL string

	// Tools is the list of tools to enable. Nil/empty = all four
	// (read, write, edit, bash). Pass an empty-but-non-nil slice
	// (e.g. []string{}) plus NoTools=true to disable everything.
	Tools []string

	// NoTools disables every tool. Useful for chat-only embeddings.
	NoTools bool

	// Lock confines tools to CWD. Same effect as the /lock command.
	Lock bool
}

// Runtime is one zot agent session. Safe for use from one goroutine
// at a time per Runtime; create separate Runtimes for parallel work.
type Runtime struct {
	mu       sync.Mutex
	agent    *core.Agent
	provider string
	model    string
	cwd      string

	// activeCancel is set while a Prompt is streaming.
	activeCancel context.CancelFunc

	// closed signals that Close has been called.
	closed bool
}

// New constructs a Runtime from cfg. Returns an error if no
// credential is available for the requested provider.
func New(cfg Config) (*Runtime, error) {
	args := agent.Args{
		Mode:               agent.ModeJSON, // headless
		Provider:           cfg.Provider,
		Model:              cfg.Model,
		CWD:                cfg.CWD,
		APIKey:             cfg.APIKey,
		BaseURL:            cfg.BaseURL,
		SystemPrompt:       cfg.SystemPrompt,
		AppendSystemPrompt: cfg.AppendSystemPrompt,
		Reasoning:          cfg.Reasoning,
		MaxSteps:           cfg.MaxSteps,
		Tools:              cfg.Tools,
		NoTools:            cfg.NoTools,
		NoSess:             true, // SDK callers manage persistence themselves
	}
	if args.MaxSteps == 0 {
		args.MaxSteps = 50
	}
	r, err := agent.Resolve(args, true)
	if err != nil {
		return nil, err
	}
	if cfg.Lock && r.Sandbox != nil {
		r.Sandbox.Lock()
	}
	ag := r.NewAgent()
	return &Runtime{
		agent:    ag,
		provider: r.Provider,
		model:    r.Model,
		cwd:      r.CWD,
	}, nil
}

// Provider returns the active provider id.
func (r *Runtime) Provider() string { r.mu.Lock(); defer r.mu.Unlock(); return r.provider }

// Model returns the active model id.
func (r *Runtime) Model() string { r.mu.Lock(); defer r.mu.Unlock(); return r.model }

// CWD returns the working directory the agent operates in.
func (r *Runtime) CWD() string { r.mu.Lock(); defer r.mu.Unlock(); return r.cwd }

// Messages returns a copy of the current transcript.
func (r *Runtime) Messages() []Message {
	if r.agent == nil {
		return nil
	}
	src := r.agent.Messages()
	out := make([]Message, len(src))
	for i, m := range src {
		out[i] = Message{
			Role:    string(m.Role),
			Content: convertContent(m.Content),
			Time:    m.Time.Format("2006-01-02T15:04:05Z"),
		}
	}
	return out
}

// SetMessages replaces the transcript. Use to seed history from a
// session file or to clear with nil.
func (r *Runtime) SetMessages(msgs []Message) {
	if r.agent == nil {
		return
	}
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, provider.Message{
			Role:    provider.Role(m.Role),
			Content: rebuildContent(m.Content),
		})
	}
	r.agent.SetMessages(out)
}

// Cost returns the cumulative usage and dollar cost for this Runtime.
func (r *Runtime) Cost() Usage {
	if r.agent == nil {
		return Usage{}
	}
	u := r.agent.Cost()
	return Usage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheWriteTokens,
		CostUSD:    u.CostUSD,
	}
}

// Prompt sends a user message and runs the agent loop. Returns a
// channel that emits one Event per agent action, closed when the
// turn finishes (cleanly or with error). Only one Prompt may be
// active at a time per Runtime; concurrent calls return ErrBusy.
func (r *Runtime) Prompt(ctx context.Context, text string, images []Image) (<-chan Event, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	if r.activeCancel != nil {
		r.mu.Unlock()
		return nil, ErrBusy
	}
	if r.agent == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("zotcore: no agent (login first via /login or set credentials)")
	}
	subCtx, cancel := context.WithCancel(ctx)
	r.activeCancel = cancel
	r.mu.Unlock()

	imgBlocks := make([]provider.ImageBlock, 0, len(images))
	for _, img := range images {
		imgBlocks = append(imgBlocks, provider.ImageBlock{MimeType: img.MimeType, Data: img.Data})
	}

	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer func() {
			r.mu.Lock()
			r.activeCancel = nil
			r.mu.Unlock()
		}()
		err := r.agent.Prompt(subCtx, text, imgBlocks, func(ev core.AgentEvent) {
			out <- toEvent(ev)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			out <- Event{Type: "error", Error: err.Error()}
		}
	}()
	return out, nil
}

// Cancel interrupts the active Prompt, if any. Safe to call when no
// prompt is in flight (no-op).
func (r *Runtime) Cancel() {
	r.mu.Lock()
	cancel := r.activeCancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Compact summarises the current transcript into a single synthetic
// user message, freeing up context. Blocks until done.
func (r *Runtime) Compact(ctx context.Context, customInstructions string) (CompactResult, error) {
	if r.agent == nil {
		return CompactResult{}, fmt.Errorf("zotcore: no agent")
	}
	r.mu.Lock()
	if r.activeCancel != nil {
		r.mu.Unlock()
		return CompactResult{}, ErrBusy
	}
	subCtx, cancel := context.WithCancel(ctx)
	r.activeCancel = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.activeCancel = nil
		r.mu.Unlock()
	}()

	summary, err := r.agent.Compact(subCtx, 4, nil)
	if err != nil {
		return CompactResult{}, err
	}
	return CompactResult{Summary: summary, Messages: r.Messages()}, nil
}

// SetModel switches the active model. Same provider only; for cross-
// provider switches, create a new Runtime.
func (r *Runtime) SetModel(model string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agent == nil {
		return fmt.Errorf("zotcore: no agent")
	}
	if _, err := provider.FindModel(r.provider, model); err != nil {
		return err
	}
	r.agent.Model = model
	r.model = model
	return nil
}

// State returns a snapshot of the runtime's current state. Useful for
// driving UIs.
func (r *Runtime) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return State{
		Provider:     r.provider,
		Model:        r.model,
		CWD:          r.cwd,
		Busy:         r.activeCancel != nil,
		MessageCount: len(r.Messages()),
	}
}

// Close releases resources held by the Runtime. Cancels any active
// prompt. Subsequent calls return ErrClosed.
func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	cancel := r.activeCancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// ListModels returns every model known to the runtime for the
// current provider (catalog + live discovery if cached).
func (r *Runtime) ListModels() []ModelInfo {
	models := provider.ModelsForProvider(r.Provider())
	out := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, ModelInfo{
			ID:            m.ID,
			Provider:      m.Provider,
			ContextWindow: m.ContextWindow,
			MaxOutput:     m.MaxOutput,
			Reasoning:     m.Reasoning,
		})
	}
	return out
}

// ---- public errors ----

// ErrBusy is returned when a Prompt or Compact is started while
// another is in flight on the same Runtime.
var ErrBusy = errors.New("zotcore: runtime is busy")

// ErrClosed is returned by methods on a Runtime after Close.
var ErrClosed = errors.New("zotcore: runtime is closed")
