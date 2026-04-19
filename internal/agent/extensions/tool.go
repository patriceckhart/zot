package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// extensionTool wraps a single extension-registered tool as a
// core.Tool. The agent's tool registry contains one of these per
// extension tool; Execute round-trips through the manager to the
// owning subprocess.
//
// One concrete type instead of a closure-driven anonymous tool
// keeps the schema, name, and ownership inspectable for logs and
// dialogs.
type extensionTool struct {
	name        string
	description string
	schema      json.RawMessage
	extension   string
	manager     *Manager
	timeout     time.Duration
}

// NewTool returns a core.Tool that round-trips invocations through
// mgr to the extension that registered (name, schema). The default
// per-call timeout is 60 seconds; callers can override.
func NewTool(mgr *Manager, info ToolInfo) core.Tool {
	return &extensionTool{
		name:        info.Name,
		description: info.Description,
		schema:      info.Schema,
		extension:   info.Extension,
		manager:     mgr,
		timeout:     60 * time.Second,
	}
}

func (t *extensionTool) Name() string            { return t.name }
func (t *extensionTool) Description() string     { return t.description }
func (t *extensionTool) Schema() json.RawMessage { return t.schema }
func (t *extensionTool) Extension() string       { return t.extension }

// Execute is what the agent calls when the LLM invokes the tool. It
// hands args to the owning extension, waits up to t.timeout for the
// reply, and converts the response into a core.ToolResult.
func (t *extensionTool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	resp, err := t.manager.InvokeTool(ctx, t.name, args, t.timeout)
	if err != nil {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("extension %s/%s failed: %v", t.extension, t.name, err)}},
		}, nil
	}
	out := core.ToolResult{IsError: resp.IsError}
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out.Content = append(out.Content, provider.TextBlock{Text: b.Text})
			}
		case "image":
			data, dErr := decodeBase64(b.Data)
			if dErr != nil {
				out.IsError = true
				out.Content = append(out.Content, provider.TextBlock{Text: fmt.Sprintf("extension %s/%s returned undecodable image: %v", t.extension, t.name, dErr)})
				continue
			}
			out.Content = append(out.Content, provider.ImageBlock{
				MimeType: b.MimeType,
				Data:     data,
			})
		default:
			// Unknown block type: stringify for debug visibility.
			out.Content = append(out.Content, provider.TextBlock{Text: fmt.Sprintf("[unknown block type %q from extension %s]", b.Type, t.extension)})
		}
	}
	if len(out.Content) == 0 {
		// Defensive: an empty content slice would confuse the model.
		out.Content = []provider.Content{provider.TextBlock{Text: "(extension returned no content)"}}
	}
	out.Details = map[string]any{"extension": t.extension, "tool": t.name}
	return out, nil
}

// decodeBase64 is a tiny wrapper around encoding/base64 so we can
// validate the extension's image data in one place.
func decodeBase64(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return base64DecodeStd(s)
}
