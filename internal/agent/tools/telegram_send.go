package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// TelegramSender is the small affordance the telegram-send tools call
// into. The real implementation lives in the interactive runtime and
// forwards to the active *telegram.Bridge; tests can pass any stub.
//
// The kind argument distinguishes "photo" (compressed inline image
// preview) from "document" (raw file attachment, no compression). For
// images Telegram resizes to its preview format, which loses detail
// but renders inline; documents preserve the original bytes but show
// up as a file the recipient downloads.
type TelegramSender interface {
	// SendImage uploads path as an inline-rendered photo with an
	// optional caption. Returns an error if the bridge is not
	// active or the upload fails.
	SendImage(ctx context.Context, path, caption string) error
	// SendDocument uploads path as a raw attachment.
	SendDocument(ctx context.Context, path, caption string) error
	// Active reports whether a paired Telegram chat is currently
	// reachable. Tools surface a clear error to the model when it
	// tries to send without a connected bridge.
	Active() bool
}

// TelegramSendImageTool exposes the bridge's photo-send affordance to
// the model so a turn that comes in over Telegram can produce a real
// image reply (a screenshot, a generated chart, a downloaded asset)
// instead of a textual description of one. Only registered while the
// bridge is connected; deregistered on disconnect.
type TelegramSendImageTool struct {
	CWD     string
	Sandbox *Sandbox
	Sender  TelegramSender
}

type telegramSendImageArgs struct {
	Path    string `json:"path"`
	Caption string `json:"caption,omitempty"`
}

const telegramSendImageSchema = `{"type":"object","properties":{"path":{"type":"string","description":"absolute or cwd-relative path to a local image file (png/jpg/gif/webp)"},"caption":{"type":"string","description":"optional caption sent alongside the image"}},"required":["path"]}`

func (t *TelegramSendImageTool) Name() string { return "telegram_send_image" }
func (t *TelegramSendImageTool) Description() string {
	return "Send a local image file to the paired Telegram chat as an inline photo. Use when the user (over Telegram) asks to see an image rather than have it described."
}
func (t *TelegramSendImageTool) Schema() json.RawMessage {
	return json.RawMessage(telegramSendImageSchema)
}

func (t *TelegramSendImageTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var a telegramSendImageArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return core.ToolResult{}, fmt.Errorf("path is required")
	}
	if t.Sender == nil || !t.Sender.Active() {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "telegram bridge is not connected; cannot send image"}},
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
			Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("%s is not a recognised image format (png/jpg/gif/webp); use telegram_send_file for arbitrary attachments", path)}},
		}, nil
	}
	if err := t.Sender.SendImage(ctx, path, a.Caption); err != nil {
		return core.ToolResult{}, fmt.Errorf("send: %w", err)
	}
	kb := info.Size() / 1024
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("sent %s to telegram (%d KB)", path, kb)}},
	}, nil
}

// TelegramSendFileTool uploads any local file to the paired chat as a
// document attachment. Use this for non-image files or when the model
// needs the recipient to receive the original bytes (no Telegram
// compression). For images you usually want telegram_send_image.
type TelegramSendFileTool struct {
	CWD     string
	Sandbox *Sandbox
	Sender  TelegramSender
}

type telegramSendFileArgs struct {
	Path    string `json:"path"`
	Caption string `json:"caption,omitempty"`
}

const telegramSendFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"absolute or cwd-relative path to any local file"},"caption":{"type":"string","description":"optional caption sent alongside the file"}},"required":["path"]}`

func (t *TelegramSendFileTool) Name() string { return "telegram_send_file" }
func (t *TelegramSendFileTool) Description() string {
	return "Send a local file to the paired Telegram chat as a document attachment (no compression). Use for non-image files or when the recipient needs the original bytes."
}
func (t *TelegramSendFileTool) Schema() json.RawMessage {
	return json.RawMessage(telegramSendFileSchema)
}

func (t *TelegramSendFileTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	var a telegramSendFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return core.ToolResult{}, fmt.Errorf("path is required")
	}
	if t.Sender == nil || !t.Sender.Active() {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "telegram bridge is not connected; cannot send file"}},
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
		Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("sent %s to telegram (%d KB)", path, kb)}},
	}, nil
}
