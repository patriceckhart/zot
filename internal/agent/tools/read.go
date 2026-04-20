// Package tools implements zot's built-in tools: read, write, edit, bash.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

const (
	maxReadLines = 2000
	maxReadBytes = 50 * 1024
)

// ReadTool reads file contents from disk.
type ReadTool struct {
	CWD     string
	Sandbox *Sandbox // when jailed, confines reads to the sandbox root
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

const readSchema = `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}`

func (t *ReadTool) Name() string { return "read" }
func (t *ReadTool) Description() string {
	return "Read a file. Images (png/jpg/gif/webp) return inline."
}
func (t *ReadTool) Schema() json.RawMessage { return json.RawMessage(readSchema) }

func (t *ReadTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a readArgs
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

	info, err := os.Stat(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	if info.IsDir() {
		return core.ToolResult{}, fmt.Errorf("%s is a directory", path)
	}

	// Image handling.
	if mime := imageMIME(path); mime != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return core.ToolResult{}, err
		}
		return core.ToolResult{
			Content: []provider.Content{provider.ImageBlock{MimeType: mime, Data: data}},
		}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer f.Close()

	// Read up to maxReadBytes and also line-limit.
	limited := io.LimitReader(f, int64(maxReadBytes)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return core.ToolResult{}, err
	}
	truncBytes := len(data) > maxReadBytes
	if truncBytes {
		data = data[:maxReadBytes]
	}

	if looksBinary(data) {
		return core.ToolResult{}, fmt.Errorf("%s looks binary; refusing to read as text", a.Path)
	}

	lines := strings.Split(string(data), "\n")
	// Trim trailing empty line from final \n.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	start := 0
	if a.Offset > 0 {
		start = a.Offset - 1
		if start > len(lines) {
			start = len(lines)
		}
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	selected := lines[start:end]

	truncLines := false
	if len(selected) > maxReadLines {
		selected = selected[:maxReadLines]
		truncLines = true
	}

	// Raw file contents go to the model. We deliberately DON'T
	// prepend line numbers here: they'd inflate the token count by
	// ~15-20% on typical source files (7 bytes per line, every
	// line, every time the file gets re-sent as context on later
	// turns) and the model doesn't need them — edit goes through
	// exact-match text replacement, not line ranges.
	//
	// The TUI renders its own gutter using the start offset stored
	// in Details, so the on-screen view still looks like cat -n.
	var sb strings.Builder
	for _, line := range selected {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if truncLines {
		sb.WriteString(fmt.Sprintf("... [truncated at %d lines]\n", maxReadLines))
	}
	if truncBytes {
		sb.WriteString(fmt.Sprintf("... [truncated at %d bytes]\n", maxReadBytes))
	}

	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: sb.String()}},
		Details: map[string]any{
			"path":            path,
			"start_line":      start + 1, // 1-indexed; TUI draws the gutter
			"lines_truncated": truncLines,
			"bytes_truncated": truncBytes,
			"total_lines":     len(lines),
		},
	}, nil
}

func resolvePath(cwd, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return filepath.Join(cwd, p)
}

func imageMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// looksBinary returns true if the buffer contains a NUL byte in its first 8 KiB.
func looksBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}
