package tui

import (
	"strings"
	"testing"
)

func TestRenderMarkdownTableWrapsToWidth(t *testing.T) {
	in := strings.Join([]string{
		"| Area | What’s visible | Summary |",
		"| --- | --- | --- |",
		"| Overall UI | Dark terminal/TUI-style interface | A screenshot of an AI/coding-agent session, likely inside zot or a similar terminal app. |",
		"| Main content | Markdown-formatted response | The assistant is describing what was seen in a previous screenshot. |",
	}, "\n")
	out := RenderMarkdown(in, Dark, 80)
	plain := stripANSI(out)
	lines := strings.Split(plain, "\n")
	if len(lines) <= 4 {
		t.Fatalf("expected wrapped table rows, got:\n%s", plain)
	}
	for i, line := range lines {
		if visibleWidth(line) > 80 {
			t.Fatalf("line %d width %d > 80: %q\n%s", i, visibleWidth(line), line, plain)
		}
	}
	want := pipePositions(lines[0])
	for i, line := range lines[1:] {
		if got := pipePositions(line); !equalInts(got, want) {
			t.Fatalf("line %d pipe columns %v, want %v: %q\n%s", i+1, got, want, line, plain)
		}
	}
}

func TestRenderMarkdownTableAlignsColumns(t *testing.T) {
	in := strings.Join([]string{
		"| Tool | Core-Sprache | Runtime/Distribution |",
		"| --- | --- | --- |",
		"| Claude Code | TypeScript | Node.js |",
		"| Codex CLI | Rust | Natives Binary (npm-Wrapper) |",
		"| OpenCode | TypeScript (+ Go für TUI) | Bun, kompiliertes Binary |",
		"| zot | Go | Natives Binary |",
	}, "\n")
	out := RenderMarkdown(in, Dark, 80)
	plain := stripANSI(out)
	lines := strings.Split(plain, "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d lines:\n%s", len(lines), plain)
	}
	counts := make([]int, len(lines))
	for i, line := range lines {
		counts[i] = strings.Count(line, "|")
		if counts[i] != 4 {
			t.Fatalf("line %d has %d pipes, want 4: %q\n%s", i, counts[i], line, plain)
		}
	}
	// Every pipe column should line up across all rows.
	want := pipePositions(lines[0])
	for i, line := range lines[1:] {
		if got := pipePositions(line); !equalInts(got, want) {
			t.Fatalf("line %d pipe columns %v, want %v: %q\n%s", i+1, got, want, line, plain)
		}
	}
}

func pipePositions(s string) []int {
	var out []int
	for i, r := range []rune(s) {
		if r == '|' {
			out = append(out, i)
		}
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
