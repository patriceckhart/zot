package provider

import "encoding/json"

// NestedToolCallsMetaKey contains display-only tool execution records grouped
// by their outer tool-call ID. Provider requests must not send this metadata.
const NestedToolCallsMetaKey = "nested_tool_calls"

// NestedToolCall records a completed nested invocation for session displays.
// Result contains text and image captions, never image payloads.
type NestedToolCall struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Args     json.RawMessage `json:"args"`
	Result   string          `json:"result"`
	IsError  bool            `json:"is_error,omitempty"`
	Status   string          `json:"status"`
	Executed bool            `json:"executed"`
}

// NestedToolCalls reads optional display metadata. Old sessions and malformed
// metadata have no nested display records. This does not change Content.
func (m Message) NestedToolCalls() map[string][]NestedToolCall {
	var calls map[string][]NestedToolCall
	if json.Unmarshal([]byte(m.Meta[NestedToolCallsMetaKey]), &calls) != nil {
		return nil
	}
	return calls
}
