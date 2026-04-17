package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// EditTool applies one or more exact-match replacements to a file.
type EditTool struct {
	CWD     string
	Sandbox *Sandbox
}

type editOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type editArgs struct {
	Path  string   `json:"path"`
	Edits []editOp `json:"edits"`
}

const editSchema = `{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"Path to the file to edit (relative or absolute)"},
    "edits":{
      "type":"array",
      "description":"One or more targeted replacements. Each oldText must match exactly once in the original file.",
      "items":{
        "type":"object",
        "properties":{
          "oldText":{"type":"string","description":"Exact text to replace. Must be unique in the file."},
          "newText":{"type":"string","description":"Replacement text."}
        },
        "required":["oldText","newText"],
        "additionalProperties":false
      }
    }
  },
  "required":["path","edits"],
  "additionalProperties":false
}`

func (t *EditTool) Name() string { return "edit" }
func (t *EditTool) Description() string {
	return "Edit an existing file via exact-match replacements. Preserves line endings and BOM."
}
func (t *EditTool) Schema() json.RawMessage { return json.RawMessage(editSchema) }

func (t *EditTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a editArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return core.ToolResult{}, fmt.Errorf("path is required")
	}
	if len(a.Edits) == 0 {
		return core.ToolResult{}, fmt.Errorf("at least one edit is required")
	}
	path := resolvePath(t.CWD, a.Path)
	if err := t.Sandbox.CheckPath(path); err != nil {
		return core.ToolResult{}, err
	}

	orig, err := os.ReadFile(path)
	if err != nil {
		return core.ToolResult{}, err
	}

	// Detect BOM and line endings.
	var bom []byte
	if bytes.HasPrefix(orig, []byte{0xEF, 0xBB, 0xBF}) {
		bom = orig[:3]
		orig = orig[3:]
	}
	nl := detectLineEnding(orig)
	// Normalize to \n for matching.
	body := string(bytes.ReplaceAll(orig, []byte("\r\n"), []byte("\n")))

	// Validate all edits first (against original content, not sequentially).
	for i, e := range a.Edits {
		if e.OldText == "" {
			return core.ToolResult{}, fmt.Errorf("edit %d: oldText must not be empty", i+1)
		}
		if e.OldText == e.NewText {
			return core.ToolResult{}, fmt.Errorf("edit %d: oldText equals newText", i+1)
		}
		count := strings.Count(body, e.OldText)
		if count == 0 {
			return core.ToolResult{}, fmt.Errorf("edit %d: oldText not found in %s", i+1, a.Path)
		}
		if count > 1 {
			return core.ToolResult{}, fmt.Errorf("edit %d: oldText matches %d times (must be unique) in %s", i+1, count, a.Path)
		}
	}

	// Apply atomically (sorted by position so offsets stay valid as we splice).
	type span struct {
		start, end  int
		replacement string
	}
	spans := make([]span, 0, len(a.Edits))
	for _, e := range a.Edits {
		idx := strings.Index(body, e.OldText)
		spans = append(spans, span{start: idx, end: idx + len(e.OldText), replacement: e.NewText})
	}
	// Check for overlaps.
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].start < spans[j].end && spans[j].start < spans[i].end {
				return core.ToolResult{}, fmt.Errorf("edits overlap; merge them into one edit")
			}
		}
	}
	// Sort ascending.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j-1].start > spans[j].start; j-- {
			spans[j-1], spans[j] = spans[j], spans[j-1]
		}
	}
	var out strings.Builder
	prev := 0
	for _, s := range spans {
		out.WriteString(body[prev:s.start])
		out.WriteString(s.replacement)
		prev = s.end
	}
	out.WriteString(body[prev:])
	newBody := out.String()

	// Restore line endings.
	if nl == "\r\n" {
		newBody = strings.ReplaceAll(newBody, "\n", "\r\n")
	}
	final := append([]byte{}, bom...)
	final = append(final, []byte(newBody)...)

	if err := os.WriteFile(path, final, 0o644); err != nil {
		return core.ToolResult{}, err
	}

	diff := unifiedDiff(a.Path, string(orig), strings.ReplaceAll(newBody, "\r\n", "\n"))
	msg := fmt.Sprintf("applied %d edit(s) to %s", len(a.Edits), a.Path)
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: msg + "\n" + diff}},
		Details: map[string]any{"path": path, "edits": len(a.Edits), "diff": diff},
	}, nil
}

func detectLineEnding(b []byte) string {
	if bytes.Contains(b, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// unifiedDiff is a minimal unified diff good enough for tool output.
func unifiedDiff(name, a, b string) string {
	if a == b {
		return ""
	}
	aLines := strings.Split(a, "\n")
	bLines := strings.Split(b, "\n")
	// Use simple LCS-based diff.
	ops := diffLines(aLines, bLines)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", name, name)
	for _, op := range ops {
		switch op.kind {
		case ' ':
			fmt.Fprintf(&sb, " %s\n", op.line)
		case '-':
			fmt.Fprintf(&sb, "-%s\n", op.line)
		case '+':
			fmt.Fprintf(&sb, "+%s\n", op.line)
		}
	}
	return sb.String()
}

type diffOp struct {
	kind byte
	line string
}

func diffLines(a, b []string) []diffOp {
	// LCS table.
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 0; i < m; i++ {
		for j := 0; j < n; j++ {
			if a[i] == b[j] {
				dp[i+1][j+1] = dp[i][j] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i+1][j+1] = dp[i+1][j]
			} else {
				dp[i+1][j+1] = dp[i][j+1]
			}
		}
	}
	// Backtrack.
	var ops []diffOp
	i, j := m, n
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			ops = append([]diffOp{{' ', a[i-1]}}, ops...)
			i--
			j--
		} else if dp[i][j-1] >= dp[i-1][j] {
			ops = append([]diffOp{{'+', b[j-1]}}, ops...)
			j--
		} else {
			ops = append([]diffOp{{'-', a[i-1]}}, ops...)
			i--
		}
	}
	for i > 0 {
		ops = append([]diffOp{{'-', a[i-1]}}, ops...)
		i--
	}
	for j > 0 {
		ops = append([]diffOp{{'+', b[j-1]}}, ops...)
		j--
	}
	return ops
}
