package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// modelOperation is host-only. Scripts can select a catalog model, but cannot
// construct operations or bypass the ordinary nested-call guard pipeline.
type modelOperation struct {
	name   string
	caller ModelCaller
	slots  chan struct{}
}

func (o modelOperation) Name() string        { return o.name }
func (o modelOperation) Description() string { return "Run a non-chat model operation" }
func (o modelOperation) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"array","minItems":2}`)
}
func (o modelOperation) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	select {
	case o.slots <- struct{}{}:
		defer func() { <-o.slots }()
	case <-ctx.Done():
		return core.ToolResult{}, ctx.Err()
	}
	value, usage, err := o.caller(ctx, strings.TrimPrefix(o.name, "models."), args)
	if err != nil {
		return core.ToolResult{}, err
	}
	var summary struct {
		Model string `json:"model"`
		Stop  string `json:"stopReason"`
		Error string `json:"errorMessage"`
	}
	if json.Unmarshal(value, &summary) != nil {
		return core.ToolResult{}, fmt.Errorf("model operation returned invalid JSON")
	}
	text := summary.Model + ": " + summary.Stop
	if summary.Error != "" {
		text += "\n" + summary.Error
	}
	result := core.ToolResult{StructuredContent: value, Usage: usage, IsError: summary.Stop == "error" || summary.Stop == "aborted", Content: []provider.Content{provider.TextBlock{Text: text}}}
	if summary.Stop == "aborted" {
		result.Status = "cancelled"
		if ctx.Err() == context.DeadlineExceeded {
			result.Status = "timed_out"
		}
	}
	return result, nil
}
