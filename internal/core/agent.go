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

// Prompt sends a user message and runs the agent loop until the model
// stops or an error occurs. Events are delivered via sink in order.
// sink must not block the caller for long; buffer as needed.
func (a *Agent) Prompt(ctx context.Context, text string, images []provider.ImageBlock, sink func(AgentEvent)) error {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
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
	return a.runLoop(ctx, sink)
}

func (a *Agent) runLoop(ctx context.Context, sink func(AgentEvent)) error {
	for step := 1; step <= a.MaxSteps; step++ {
		sink(EvTurnStart{Step: step})
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
		case provider.EventToolStart, provider.EventToolArgs, provider.EventToolEnd:
			// The provider emits these to support partial-JSON UIs.
			// We surface the complete tool call once per block at EvToolCall
			// below (after EventDone), so for now we drop them.
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
	// produce partial, mid-sentence content that would confuse subsequent
	// turns if it stayed in the transcript — drop it instead.
	if len(finalMsg.Content) > 0 && stop != provider.StopAborted {
		a.mu.Lock()
		a.messages = append(a.messages, finalMsg)
		a.mu.Unlock()
		sink(EvAssistantMessage{Message: finalMsg})
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
