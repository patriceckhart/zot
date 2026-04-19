package tui

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/internal/provider"
)

// TestStatusBarAlwaysTwoLines verifies the status bar always emits
// two lines when a cwd is present, regardless of terminal width, and
// that the cwd is indented with the 2-space pad so it lines up under
// the "(provider)" column on line 1.
func TestStatusBarAlwaysTwoLines(t *testing.T) {
	// Wide terminal that would previously combine into one line.
	lines := StatusBar(StatusBarParams{
		Theme:    Dark,
		Provider: "anthropic",
		Model:    "claude-opus-4-7",
		CWD:      "/tmp/x",
		Usage: provider.Usage{
			InputTokens:  476_000,
			OutputTokens: 3_400,
			CostUSD:      1.242,
		},
		Subscription: true,
		ContextUsed:  55_000,
		ContextMax:   1_000_000,
		Cols:         500, // very wide
	})
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "claude-opus-4-7") {
		t.Errorf("line 1 should contain model, got %q", lines[0])
	}
	// Line 2 must start with 2-space indent.
	if !strings.HasPrefix(lines[1], "  ") {
		t.Errorf("line 2 should start with 2-space indent, got %q", lines[1])
	}
	if !strings.Contains(lines[1], "/tmp/x") {
		t.Errorf("line 2 should contain cwd, got %q", lines[1])
	}
}

// TestStatusBarNoCWD verifies an empty cwd stays single-line.
func TestStatusBarNoCWD(t *testing.T) {
	lines := StatusBar(StatusBarParams{
		Theme:    Dark,
		Provider: "openai",
		Model:    "gpt-5.4",
		CWD:      "",
		Cols:     200,
	})
	if len(lines) != 1 {
		t.Fatalf("empty cwd: want 1 line, got %d: %q", len(lines), lines)
	}
}
