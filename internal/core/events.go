package core

import (
	"encoding/json"

	"github.com/patriceckhart/zot/internal/provider"
)

// AgentEvent is the superset of events emitted by an Agent run.
// Consumers discriminate via a type switch. Each concrete type has a
// Type() method for JSON serialization.
type AgentEvent interface {
	Type() string
}

type EvTurnStart struct {
	Step int
}

func (EvTurnStart) Type() string { return "turn_start" }

type EvUserMessage struct {
	Message provider.Message
}

func (EvUserMessage) Type() string { return "user_message" }

type EvAssistantStart struct{}

func (EvAssistantStart) Type() string { return "assistant_start" }

type EvTextDelta struct {
	Delta string
}

func (EvTextDelta) Type() string { return "text_delta" }

type EvToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

func (EvToolCall) Type() string { return "tool_call" }

type EvToolProgress struct {
	ID   string
	Text string
}

func (EvToolProgress) Type() string { return "tool_progress" }

type EvToolResult struct {
	ID     string
	Result ToolResult
}

func (EvToolResult) Type() string { return "tool_result" }

type EvUsage struct {
	Usage      provider.Usage
	Cumulative provider.Usage
}

func (EvUsage) Type() string { return "usage" }

type EvAssistantMessage struct {
	Message provider.Message
}

func (EvAssistantMessage) Type() string { return "assistant_message" }

type EvTurnEnd struct {
	Stop provider.StopReason
	Err  error
}

func (EvTurnEnd) Type() string { return "turn_end" }

type EvDone struct{}

func (EvDone) Type() string { return "done" }

type EvError struct {
	Err error
}

func (EvError) Type() string { return "error" }
