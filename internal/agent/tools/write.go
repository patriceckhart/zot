package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// WriteTool writes content to a file, creating parent directories.
type WriteTool struct {
	CWD     string
	Sandbox *Sandbox
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

const writeSchema = `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`

func (t *WriteTool) Name() string { return "write" }
func (t *WriteTool) Description() string {
	return "Write a file. Creates parent dirs. Overwrites."
}
func (t *WriteTool) Schema() json.RawMessage { return json.RawMessage(writeSchema) }

func (t *WriteTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a writeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return core.ToolResult{}, fmt.Errorf("path is required")
	}
	path := resolvePath(t.CWD, a.Path)
	if err := t.Sandbox.CheckPath(path); err != nil {
		return core.ToolResult{}, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return core.ToolResult{}, err
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return core.ToolResult{}, err
	}

	// Return the file content as the result body, just like `read`
	// does. The TUI renders it with a syntax-highlighted gutter so
	// the on-screen view after a `write` matches the pre-write
	// streaming preview seamlessly. The model also sees the written
	// content in its tool_result, which is useful on follow-up turns
	// where it wants to reference what it just wrote without a
	// second `read` call.
	totalLines := strings.Count(a.Content, "\n")
	if len(a.Content) > 0 && !strings.HasSuffix(a.Content, "\n") {
		totalLines++ // count the last unterminated line
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: a.Content}},
		Details: map[string]any{
			"path":        path,
			"bytes":       len(a.Content),
			"total_lines": totalLines,
			"start_line":  1,
		},
	}, nil
}
