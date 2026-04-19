package zotcore

import (
	"encoding/json"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// Event is the unit emitted by Runtime.Prompt's channel. Type
// identifies the event kind; remaining fields are populated based on
// kind. The wire schema is the same as the JSON-RPC stream notifications
// emitted by `zot rpc`, so consumers can share parsing code.
type Event struct {
	Type string `json:"type"`

	// Common fields.
	Step int    `json:"step,omitempty"`
	Time string `json:"time,omitempty"`

	// text_delta
	Delta string `json:"delta,omitempty"`

	// tool_call
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`

	// tool_progress
	Text string `json:"text,omitempty"`

	// tool_result
	IsError bool           `json:"is_error,omitempty"`
	Result  []ContentBlock `json:"content,omitempty"`

	// usage
	Usage      *Usage `json:"usage,omitempty"`
	Cumulative *Usage `json:"cumulative,omitempty"`

	// user_message / assistant_message
	Message *Message `json:"message,omitempty"`

	// turn_end
	Stop string `json:"stop,omitempty"`

	// error
	Error string `json:"error,omitempty"`
}

// Message is one transcript entry — a user prompt, assistant reply,
// or tool result.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
	Time    string         `json:"time,omitempty"`
}

// ContentBlock is one piece of message content. Discriminate on Type:
//   - "text"        → Text
//   - "image"       → MimeType + Data (or Bytes for size-only)
//   - "tool_call"   → ID + Name + Args
//   - "tool_result" → CallID + IsError + Content (recursive)
type ContentBlock struct {
	Type string `json:"type"`

	Text     string          `json:"text,omitempty"`
	MimeType string          `json:"mime_type,omitempty"`
	Data     []byte          `json:"data,omitempty"`
	Bytes    int             `json:"bytes,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	CallID   string          `json:"call_id,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
	Content  []ContentBlock  `json:"content,omitempty"`
}

// Image is a single user-attached image for Prompt.
type Image struct {
	MimeType string
	Data     []byte
}

// Usage is per-turn or cumulative token / cost counts.
type Usage struct {
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cache_read"`
	CacheWrite int     `json:"cache_write"`
	CostUSD    float64 `json:"cost_usd"`
}

// State is a snapshot of the runtime's current state.
type State struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	CWD          string `json:"cwd"`
	Busy         bool   `json:"busy"`
	MessageCount int    `json:"message_count"`
}

// CompactResult describes the outcome of Compact.
type CompactResult struct {
	Summary  string    `json:"summary"`
	Messages []Message `json:"messages"`
}

// ModelInfo describes one model known to the runtime.
type ModelInfo struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ContextWindow int    `json:"context_window"`
	MaxOutput     int    `json:"max_output"`
	Reasoning     bool   `json:"reasoning"`
}

// ---- internal converters ----

func toEvent(ev core.AgentEvent) Event {
	out := Event{Type: ev.Type()}
	switch e := ev.(type) {
	case core.EvTurnStart:
		out.Step = e.Step
	case core.EvUserMessage:
		out.Message = &Message{Role: string(e.Message.Role), Content: convertContent(e.Message.Content)}
	case core.EvAssistantMessage:
		out.Message = &Message{Role: string(e.Message.Role), Content: convertContent(e.Message.Content)}
	case core.EvTextDelta:
		out.Delta = e.Delta
	case core.EvToolCall:
		out.ID = e.ID
		out.Name = e.Name
		out.Args = e.Args
	case core.EvToolProgress:
		out.ID = e.ID
		out.Text = e.Text
	case core.EvToolResult:
		out.ID = e.ID
		out.IsError = e.Result.IsError
		out.Result = convertContent(e.Result.Content)
	case core.EvUsage:
		out.Usage = &Usage{
			Input:      e.Usage.InputTokens,
			Output:     e.Usage.OutputTokens,
			CacheRead:  e.Usage.CacheReadTokens,
			CacheWrite: e.Usage.CacheWriteTokens,
			CostUSD:    e.Usage.CostUSD,
		}
		out.Cumulative = &Usage{
			Input:      e.Cumulative.InputTokens,
			Output:     e.Cumulative.OutputTokens,
			CacheRead:  e.Cumulative.CacheReadTokens,
			CacheWrite: e.Cumulative.CacheWriteTokens,
			CostUSD:    e.Cumulative.CostUSD,
		}
	case core.EvTurnEnd:
		out.Stop = string(e.Stop)
		if e.Err != nil {
			out.Error = e.Err.Error()
		}
	case core.EvError:
		if e.Err != nil {
			out.Error = e.Err.Error()
		}
	}
	return out
}

func convertContent(blocks []provider.Content) []ContentBlock {
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case provider.TextBlock:
			out = append(out, ContentBlock{Type: "text", Text: v.Text})
		case provider.ImageBlock:
			out = append(out, ContentBlock{Type: "image", MimeType: v.MimeType, Bytes: len(v.Data)})
		case provider.ToolCallBlock:
			out = append(out, ContentBlock{Type: "tool_call", ID: v.ID, Name: v.Name, Args: v.Arguments})
		case provider.ToolResultBlock:
			out = append(out, ContentBlock{
				Type:    "tool_result",
				CallID:  v.CallID,
				IsError: v.IsError,
				Content: convertContent(v.Content),
			})
		}
	}
	return out
}

func rebuildContent(blocks []ContentBlock) []provider.Content {
	out := make([]provider.Content, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, provider.TextBlock{Text: b.Text})
		case "image":
			out = append(out, provider.ImageBlock{MimeType: b.MimeType, Data: b.Data})
		case "tool_call":
			args := b.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out = append(out, provider.ToolCallBlock{ID: b.ID, Name: b.Name, Arguments: args})
		case "tool_result":
			out = append(out, provider.ToolResultBlock{
				CallID:  b.CallID,
				IsError: b.IsError,
				Content: rebuildContent(b.Content),
			})
		}
	}
	return out
}
