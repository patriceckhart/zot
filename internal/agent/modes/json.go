package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// RunJSON runs the agent to completion, writing one JSON object per
// AgentEvent as newline-delimited JSON.
func RunJSON(ctx context.Context, ag *core.Agent, prompt string, images []provider.ImageBlock, out io.Writer) error {
	enc := json.NewEncoder(out)
	write := func(v any) {
		_ = enc.Encode(v)
	}

	var runErr error
	sink := func(ev core.AgentEvent) {
		write(EventToJSON(ev))
	}

	if err := ag.Prompt(ctx, prompt, images, sink); err != nil {
		runErr = err
	}

	if runErr != nil {
		fmt.Fprintln(out, `{"type":"error","message":`+jsonString(runErr.Error())+`}`)
	}
	return runErr
}

// EventToJSON converts an AgentEvent to a JSON-friendly map. The on-wire
// schema is deliberately simple and flat. Exported so the RPC mode can
// reuse the same serialisation as `zot --json`.
func EventToJSON(ev core.AgentEvent) map[string]any {
	m := map[string]any{"type": ev.Type()}
	switch e := ev.(type) {
	case core.EvTurnStart:
		m["step"] = e.Step
	case core.EvUserMessage:
		m["content"] = ContentToJSON(e.Message.Content)
		m["time"] = e.Message.Time
	case core.EvAssistantMessage:
		m["content"] = ContentToJSON(e.Message.Content)
		m["time"] = e.Message.Time
	case core.EvTextDelta:
		m["delta"] = e.Delta
	case core.EvToolUseStart:
		m["id"] = e.ID
		m["name"] = e.Name
	case core.EvToolUseArgs:
		m["id"] = e.ID
		m["delta"] = e.Delta
	case core.EvToolUseEnd:
		m["id"] = e.ID
	case core.EvToolCall:
		m["id"] = e.ID
		m["name"] = e.Name
		var args any
		_ = json.Unmarshal(e.Args, &args)
		m["args"] = args
	case core.EvToolProgress:
		m["id"] = e.ID
		m["text"] = e.Text
	case core.EvToolResult:
		m["id"] = e.ID
		m["is_error"] = e.Result.IsError
		m["content"] = ContentToJSON(e.Result.Content)
	case core.EvUsage:
		m["input"] = e.Usage.InputTokens
		m["output"] = e.Usage.OutputTokens
		m["cache_read"] = e.Usage.CacheReadTokens
		m["cache_write"] = e.Usage.CacheWriteTokens
		m["cost_usd"] = e.Usage.CostUSD
		m["cumulative"] = map[string]any{
			"input":       e.Cumulative.InputTokens,
			"output":      e.Cumulative.OutputTokens,
			"cache_read":  e.Cumulative.CacheReadTokens,
			"cache_write": e.Cumulative.CacheWriteTokens,
			"cost_usd":    e.Cumulative.CostUSD,
		}
	case core.EvTurnEnd:
		m["stop"] = string(e.Stop)
		if e.Err != nil {
			m["error"] = e.Err.Error()
		}
	}
	return m
}

// ContentToJSON serialises a transcript content slice into the same
// shape used in EventToJSON. Exported alongside EventToJSON for the
// RPC mode.
func ContentToJSON(blocks []provider.Content) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case provider.TextBlock:
			out = append(out, map[string]any{"type": "text", "text": v.Text})
		case provider.ImageBlock:
			out = append(out, map[string]any{"type": "image", "mime_type": v.MimeType, "bytes": len(v.Data)})
		case provider.ToolCallBlock:
			var args any
			_ = json.Unmarshal(v.Arguments, &args)
			out = append(out, map[string]any{"type": "tool_call", "id": v.ID, "name": v.Name, "args": args})
		case provider.ToolResultBlock:
			out = append(out, map[string]any{
				"type":     "tool_result",
				"call_id":  v.CallID,
				"is_error": v.IsError,
				"content":  ContentToJSON(v.Content),
			})
		}
	}
	return out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
