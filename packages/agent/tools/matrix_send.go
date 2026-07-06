package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// MatrixSendImageTool exposes the matrix bridge's image-send
// affordance to the model. Only registered while /matrix is
// connected; deregistered on disconnect.
type MatrixSendImageTool struct {
	CWD     string
	Sandbox *Sandbox
	Sender  MediaSender
}

type matrixSendArgs struct {
	Path    string `json:"path"`
	Caption string `json:"caption,omitempty"`
}

const matrixSendImageSchema = `{"type":"object","properties":{"path":{"type":"string","description":"absolute or cwd-relative path to a local image file (png/jpg/gif/webp)"},"caption":{"type":"string","description":"optional caption sent alongside the image"}},"required":["path"]}`

func (t *MatrixSendImageTool) Name() string { return "matrix_send_image" }
func (t *MatrixSendImageTool) Description() string {
	return "Send a local image file to the paired Matrix room as an inline image. Use when the user (over Matrix) asks to see an image rather than have it described."
}
func (t *MatrixSendImageTool) Schema() json.RawMessage {
	return json.RawMessage(matrixSendImageSchema)
}

func (t *MatrixSendImageTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var a matrixSendArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return core.ToolResult{}, fmt.Errorf("path is required")
	}
	if t.Sender == nil || !t.Sender.Active() {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "matrix bridge is not connected; cannot send image"}},
		}, nil
	}
	path := resolvePath(t.CWD, a.Path)
	if err := t.Sandbox.CheckPath(path); err != nil {
		return core.ToolResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	if info.IsDir() {
		return core.ToolResult{}, fmt.Errorf("%s is a directory", path)
	}
	if mime := imageMIME(path); mime == "" {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("%s is not a recognised image format (png/jpg/gif/webp); use matrix_send_file for arbitrary attachments", path)}},
		}, nil
	}
	if err := t.Sender.SendImage(ctx, path, a.Caption); err != nil {
		return core.ToolResult{}, fmt.Errorf("send: %w", err)
	}
	kb := info.Size() / 1024
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("sent %s to matrix (%d KB)", path, kb)}},
	}, nil
}

// MatrixSendFileTool uploads any local file to the paired room as a
// document attachment (original bytes, no compression).
type MatrixSendFileTool struct {
	CWD     string
	Sandbox *Sandbox
	Sender  MediaSender
}

const matrixSendFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"absolute or cwd-relative path to any local file"},"caption":{"type":"string","description":"optional caption sent alongside the file"}},"required":["path"]}`

func (t *MatrixSendFileTool) Name() string { return "matrix_send_file" }
func (t *MatrixSendFileTool) Description() string {
	return "Send a local file to the paired Matrix room as a file attachment. Use for non-image files or when the recipient needs the original bytes."
}
func (t *MatrixSendFileTool) Schema() json.RawMessage {
	return json.RawMessage(matrixSendFileSchema)
}

func (t *MatrixSendFileTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var a matrixSendArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return core.ToolResult{}, fmt.Errorf("path is required")
	}
	if t.Sender == nil || !t.Sender.Active() {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "matrix bridge is not connected; cannot send file"}},
		}, nil
	}
	path := resolvePath(t.CWD, a.Path)
	if err := t.Sandbox.CheckPath(path); err != nil {
		return core.ToolResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	if info.IsDir() {
		return core.ToolResult{}, fmt.Errorf("%s is a directory", path)
	}
	if err := t.Sender.SendDocument(ctx, path, a.Caption); err != nil {
		return core.ToolResult{}, fmt.Errorf("send: %w", err)
	}
	kb := info.Size() / 1024
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("sent %s to matrix (%d KB)", path, kb)}},
	}, nil
}
