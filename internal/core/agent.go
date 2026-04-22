package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/provider"
)

// Agent is a stateful conversation bound to a provider client, a model,
// and a set of tools.
type Agent struct {
	Client    provider.Client
	Model     string
	System    string
	Tools     Registry
	MaxSteps  int
	Reasoning string

	// BeforeToolExecute, if set, is called immediately before each
	// tool runs. Returning (allowed=false, reason) short-circuits
	// the call with an error result containing reason. Optionally,
	// returning a non-nil modifiedArgs replaces the JSON args the
	// tool will see, which lets guards redact / augment / patch the
	// model's request without rewriting the transcript. Empty or
	// malformed modifiedArgs is ignored.
	BeforeToolExecute func(call provider.ToolCallBlock) (allowed bool, reason string, modifiedArgs json.RawMessage)

	// BeforeTurn, if set, is called before each turn's model call.
	// Returning (allowed=false, reason) aborts the turn; reason is
	// surfaced as an assistant-like status line. Used for rate-
	// limiting, business-hour gates, and deny-by-default setups.
	BeforeTurn func(step int) (allowed bool, reason string)

	// BeforeAssistantMessage, if set, is called after the model's
	// final assistant message is assembled but before it's appended
	// to the transcript. Returning (allowed=false) suppresses both
	// the transcript append and the UI event. A non-empty
	// replacement rewrites the visible text for the user while
	// leaving the model's original text in the transcript (so the
	// model can still see what it said in subsequent turns).
	BeforeAssistantMessage func(text string) (allowed bool, reason, replacement string)

	// OnEvent, if set, mirrors every AgentEvent the loop emits to
	// this callback in addition to the per-Prompt sink. Used by the
	// extension manager to fan events out to subscribed extensions
	// without each caller having to compose sinks manually.
	OnEvent func(AgentEvent)

	mu       sync.Mutex
	messages []provider.Message
	cost     CostTracker
}

// NewAgent returns an Agent with sensible defaults.
func NewAgent(client provider.Client, model, system string, tools Registry) *Agent {
	return &Agent{
		Client:   client,
		Model:    model,
		System:   system,
		Tools:    tools,
		MaxSteps: 50,
	}
}

// Messages returns a copy of the current transcript.
func (a *Agent) Messages() []provider.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]provider.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// SetTools swaps the tool registry. Used by /reload-ext to hand
// the agent a fresh registry after extension subprocesses have been
// respawned (and their freshly-registered tools merged in).
func (a *Agent) SetTools(reg Registry) {
	a.mu.Lock()
	a.Tools = reg
	a.mu.Unlock()
}

// SetMessages replaces the transcript (used when resuming a session).
func (a *Agent) SetMessages(msgs []provider.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages[:0], msgs...)
}

// Cost returns the cumulative usage.
func (a *Agent) Cost() provider.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cost.Total
}

// SeedCost sets the cumulative usage as a baseline before the first
// turn runs. Used when transferring state from another agent (model
// or provider switch) so the running cost meter doesn't reset to 0.
func (a *Agent) SeedCost(u provider.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cost.Total = u
}

// Prompt sends a user message and runs the agent loop until the model
// stops or an error occurs. Events are delivered via sink in order.
// sink must not block the caller for long; buffer as needed.
func (a *Agent) Prompt(ctx context.Context, text string, images []provider.ImageBlock, sink func(AgentEvent)) error {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink = a.wrapSink(sink)
	content := []provider.Content{}
	if text != "" {
		content = append(content, provider.TextBlock{Text: text})
	}
	for _, img := range images {
		content = append(content, img)
	}
	user := provider.Message{Role: provider.RoleUser, Content: content, Time: time.Now()}

	a.mu.Lock()
	a.messages = append(a.messages, user)
	a.mu.Unlock()
	sink(EvUserMessage{Message: user})

	return a.runLoop(ctx, sink)
}

// Continue runs the agent loop against the existing transcript. Used
// after appending tool results manually or to retry.
func (a *Agent) Continue(ctx context.Context, sink func(AgentEvent)) error {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink = a.wrapSink(sink)
	return a.runLoop(ctx, sink)
}

// wrapSink composes the per-call sink with a.OnEvent (if set) so the
// extension manager (or any other observer) sees every AgentEvent
// without having to thread itself through every Prompt callsite.
func (a *Agent) wrapSink(sink func(AgentEvent)) func(AgentEvent) {
	if a.OnEvent == nil {
		return sink
	}
	obs := a.OnEvent
	return func(ev AgentEvent) {
		obs(ev)
		sink(ev)
	}
}

func (a *Agent) runLoop(ctx context.Context, sink func(AgentEvent)) error {
	for step := 1; step <= a.MaxSteps; step++ {
		sink(EvTurnStart{Step: step})
		if a.BeforeTurn != nil {
			if allowed, reason := a.BeforeTurn(step); !allowed {
				if reason == "" {
					reason = "turn blocked by extension guard"
				}
				sink(EvTurnEnd{Stop: provider.StopError, Err: fmt.Errorf("%s", reason)})
				sink(EvDone{})
				return nil
			}
		}
		stop, assistantMsg, err := a.oneTurn(ctx, sink)
		sink(EvTurnEnd{Stop: stop, Err: err})
		if err != nil {
			return err
		}

		if stop == provider.StopToolUse {
			// Execute each tool call, append a single tool-results message, continue.
			toolMsg, hadError := a.executeTools(ctx, assistantMsg, sink)
			a.mu.Lock()
			a.messages = append(a.messages, toolMsg)
			// OpenAI's chat-completions tool message shape is text-centric.
			// Vision models reliably consume images when they arrive as user
			// content, so when a tool result contains images we mirror them
			// into a synthetic user message immediately after the tool result.
			// This keeps the transcript self-contained for providers that can
			// see image blocks in tool messages while making OpenAI vision
			// models actually receive the image bytes.
			if a.Client != nil && a.Client.Name() == "openai" {
				if mirror := mirrorToolImagesAsUser(toolMsg); len(mirror.Content) > 0 {
					a.messages = append(a.messages, mirror)
				}
			}
			a.mu.Unlock()
			// If context was cancelled during tool execution, bail out.
			if err := ctx.Err(); err != nil {
				sink(EvDone{})
				return err
			}
			_ = hadError
			continue
		}

		// Terminal stop (end, length, error, aborted).
		sink(EvDone{})
		return nil
	}
	sink(EvDone{})
	return fmt.Errorf("max steps (%d) exceeded", a.MaxSteps)
}

// oneTurn calls the LLM once, forwards events, returns the stop reason
// and the assembled assistant message (already appended to the transcript).
func (a *Agent) oneTurn(ctx context.Context, sink func(AgentEvent)) (provider.StopReason, provider.Message, error) {
	req := provider.Request{
		Model:     a.Model,
		System:    a.System,
		Messages:  a.Messages(),
		Tools:     a.Tools.Specs(),
		Reasoning: a.Reasoning,
	}
	stream, err := a.Client.Stream(ctx, req)
	if err != nil {
		return provider.StopError, provider.Message{}, err
	}

	sink(EvAssistantStart{})

	var (
		stop     provider.StopReason
		finalErr error
		finalMsg provider.Message
	)

	for ev := range stream {
		switch e := ev.(type) {
		case provider.EventStart:
			// nothing
		case provider.EventTextDelta:
			sink(EvTextDelta{Delta: e.Delta})
		case provider.EventToolStart:
			sink(EvToolUseStart{ID: e.ID, Name: e.Name})
		case provider.EventToolArgs:
			sink(EvToolUseArgs{ID: e.ID, Delta: e.Delta})
		case provider.EventToolEnd:
			sink(EvToolUseEnd{ID: e.ID})
		case provider.EventUsage:
			cum := a.cost.Add(e.Usage)
			sink(EvUsage{Usage: e.Usage, Cumulative: cum})
		case provider.EventDone:
			stop = e.Stop
			finalErr = e.Err
			finalMsg = e.Message
		}
	}

	// Append assistant message to transcript. Aborted turns (Esc / Ctrl+C)
	// produce partial content. When the partial message is text only we
	// keep whatever was streamed up to the cancel so the user does not
	// lose visible work (a cut-off summary is still useful). If the
	// partial message already contained tool-call blocks we drop the
	// whole thing, because an unmatched tool_use would fail the next
	// turn with a tool_result mismatch error.
	keep := len(finalMsg.Content) > 0
	if stop == provider.StopAborted && keep {
		hasToolCall := false
		for _, c := range finalMsg.Content {
			if _, ok := c.(provider.ToolCallBlock); ok {
				hasToolCall = true
				break
			}
		}
		if hasToolCall {
			keep = false
		}
	}
	if keep {
		emit := finalMsg
		suppress := false

		// BeforeAssistantMessage hook: extensions can suppress or
		// rewrite the visible text. The transcript keeps the
		// model's original output so the model still sees what it
		// said on subsequent turns.
		if a.BeforeAssistantMessage != nil {
			orig := extractText(finalMsg)
			if orig != "" {
				allowed, _, replacement := a.BeforeAssistantMessage(orig)
				if !allowed {
					suppress = true
				} else if replacement != "" && replacement != orig {
					emit = replaceText(finalMsg, replacement)
				}
			}
		}

		a.mu.Lock()
		a.messages = append(a.messages, finalMsg)
		a.mu.Unlock()
		if !suppress {
			sink(EvAssistantMessage{Message: emit})
		}
		// Now surface tool calls as EvToolCall events so UIs can render them
		// in order before the tool results arrive.
		for _, c := range finalMsg.Content {
			if tc, ok := c.(provider.ToolCallBlock); ok {
				sink(EvToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Arguments})
			}
		}
	}

	return stop, finalMsg, finalErr
}

// executeTools runs every tool call in the assistant message and returns
// a single tool-role message carrying all results.
func (a *Agent) executeTools(ctx context.Context, msg provider.Message, sink func(AgentEvent)) (provider.Message, bool) {
	var results []provider.Content
	hadError := false

	for _, c := range msg.Content {
		tc, ok := c.(provider.ToolCallBlock)
		if !ok {
			continue
		}
		res := a.runOneTool(ctx, tc, sink)
		if res.IsError {
			hadError = true
		}
		results = append(results, provider.ToolResultBlock{
			CallID:  tc.ID,
			Content: res.Content,
			IsError: res.IsError,
		})
		sink(EvToolResult{ID: tc.ID, Result: res})
	}

	return provider.Message{
		Role:    provider.RoleTool,
		Content: results,
		Time:    time.Now(),
	}, hadError
}

func (a *Agent) runOneTool(ctx context.Context, tc provider.ToolCallBlock, sink func(AgentEvent)) ToolResult {
	tool, err := a.Tools.Get(tc.Name)
	if err != nil {
		return ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
			IsError: true,
		}
	}

	args := tc.Arguments

	// Intercept hook: an extension or other guard can refuse the
	// call before any side effect happens, OR rewrite the args
	// seen by the tool. The model sees the reason as the tool
	// error, learns from it, and (typically) proposes a different
	// action; rewrites are invisible to the model (they apply only
	// to the execution).
	if a.BeforeToolExecute != nil {
		allowed, reason, modified := a.BeforeToolExecute(tc)
		if !allowed {
			if reason == "" {
				reason = "tool call refused by extension guard"
			}
			return ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: reason}},
				IsError: true,
			}
		}
		if len(modified) > 0 && json.Valid(modified) {
			args = modified
		}
	}

	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	// Recover panics so a buggy tool does not crash the agent.
	var res ToolResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				res = ToolResult{
					Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("panic: %v", r)}},
					IsError: true,
				}
			}
		}()
		out, err := tool.Execute(ctx, args, func(text string) {
			sink(EvToolProgress{ID: tc.ID, Text: text})
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				res = ToolResult{
					Content: []provider.Content{provider.TextBlock{Text: "aborted: " + err.Error()}},
					IsError: true,
				}
				return
			}
			res = ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
				IsError: true,
			}
			return
		}
		res = out
	}()
	return res
}

// extractText concatenates all TextBlock content in a message. Used
// by BeforeAssistantMessage so guards see a single string instead of
// having to walk provider.Content themselves.
func mirrorToolImagesAsUser(msg provider.Message) provider.Message {
	var content []provider.Content
	for _, c := range msg.Content {
		tr, ok := c.(provider.ToolResultBlock)
		if !ok {
			continue
		}
		for _, inner := range tr.Content {
			switch v := inner.(type) {
			case provider.TextBlock:
				// Keep short textual context so the model understands why
				// the images appeared, but don't duplicate giant read
				// outputs verbatim.
				if len(v.Text) > 0 && len(v.Text) <= 500 {
					content = append(content, v)
				}
			case provider.ImageBlock:
				content = append(content, v)
			}
		}
	}
	if len(content) == 0 {
		return provider.Message{}
	}
	prefix := provider.TextBlock{Text: "Tool output included the following image content:"}
	content = append([]provider.Content{prefix}, content...)
	return provider.Message{Role: provider.RoleUser, Content: content, Time: time.Now()}
}

func extractText(msg provider.Message) string {
	var out string
	for _, c := range msg.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if out != "" {
				out += "\n"
			}
			out += tb.Text
		}
	}
	return out
}

// replaceText returns a copy of msg with every TextBlock replaced by
// a single TextBlock containing replacement. Non-text content (tool
// calls, etc.) is preserved in order.
func replaceText(msg provider.Message, replacement string) provider.Message {
	out := provider.Message{Role: msg.Role}
	out.Content = make([]provider.Content, 0, len(msg.Content))
	replaced := false
	for _, c := range msg.Content {
		if _, ok := c.(provider.TextBlock); ok {
			if !replaced {
				out.Content = append(out.Content, provider.TextBlock{Text: replacement})
				replaced = true
			}
			continue
		}
		out.Content = append(out.Content, c)
	}
	if !replaced {
		out.Content = append(out.Content, provider.TextBlock{Text: replacement})
	}
	return out
}
