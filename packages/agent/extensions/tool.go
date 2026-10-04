package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
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
	name                  string
	description           string
	schema                json.RawMessage
	outputSchema          json.RawMessage
	namespace             string
	namespaceDescription  string
	namespaceInstructions string
	exposure              string
	extension             string
	manager               *Manager
	timeout               time.Duration
	deferred              bool
	interactive           bool
}

// NewTool returns a core.Tool that round-trips invocations through
// mgr to the extension that registered (name, schema). The default
// per-call timeout is 60 seconds. Interactive tools have no reply deadline.
func NewTool(mgr *Manager, info ToolInfo) core.Tool {
	timeout := 60 * time.Second
	if info.Interactive {
		timeout = 0
	}
	return &extensionTool{
		name:                  info.Name,
		description:           info.Description,
		schema:                info.Schema,
		outputSchema:          info.OutputSchema,
		namespace:             info.Namespace,
		namespaceDescription:  info.NamespaceDescription,
		namespaceInstructions: info.NamespaceInstructions,
		exposure:              info.Exposure,
		extension:             info.Extension,
		manager:               mgr,
		timeout:               timeout,
		deferred:              info.Deferred,
		interactive:           info.Interactive,
	}
}

func (t *extensionTool) Name() string                  { return t.name }
func (t *extensionTool) Description() string           { return t.description }
func (t *extensionTool) Schema() json.RawMessage       { return t.schema }
func (t *extensionTool) Extension() string             { return t.extension }
func (t *extensionTool) Deferred() bool                { return t.deferred }
func (t *extensionTool) OutputSchema() json.RawMessage { return t.outputSchema }
func (t *extensionTool) Exposure() string              { return t.exposure }
func (t *extensionTool) NamespaceDescription() string  { return t.namespaceDescription }
func (t *extensionTool) NamespaceInstructions() string { return t.namespaceInstructions }
func (t *extensionTool) Namespace() string {
	if t.namespace != "" {
		return t.namespace
	}
	return t.extension
}

// Execute is what the agent calls when the LLM invokes the tool. It
// hands args to the owning extension, waits up to t.timeout for the
// reply, and converts the response into a core.ToolResult.
func (t *extensionTool) Execute(ctx context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if t.interactive && !t.manager.supportsInteractiveTools() {
		return core.ToolResult{Status: "failed", IsError: true, Content: []provider.Content{
			provider.TextBlock{Text: "interactive extension tools require an interactive host"},
		}}, nil
	}
	resp, err := t.manager.InvokeTool(ctx, t.name, args, t.timeout)
	if err != nil {
		status := "failed"
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			status = "timed_out"
		}
		return core.ToolResult{
			Status:  status,
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("extension %s/%s failed: %v", t.extension, t.name, err)}},
		}, nil
	}
	out := core.ToolResult{IsError: resp.IsError, ActivateTools: resp.ActivateTools, StructuredContent: resp.StructuredContent}
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out.Content = append(out.Content, provider.TextBlock{Text: b.Text, Format: b.Format})
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
