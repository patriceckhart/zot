package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestAttachedToolProgressRendersOnlyWhenAttached(t *testing.T) {
	embedded := NewInteractive(InteractiveConfig{})
	embedded.handleEvent(core.EvToolCall{ID: "c1", Name: "bash"})
	embedded.handleEvent(core.EvToolProgress{ID: "c1", Text: "out"})
	if got := embedded.toolCalls["c1"].Progress; got != "" {
		t.Fatalf("embedded progress changed rendering: %q", got)
	}

	driver := func(context.Context, *core.Agent, string, func(core.AgentEvent)) error { return nil }
	attached := NewInteractive(InteractiveConfig{PromptDriver: driver, ExecutionLabel: "attached: host.sock"})
	attached.handleEvent(core.EvToolCall{ID: "c1", Name: "bash"})
	attached.handleEvent(core.EvToolProgress{ID: "c1", Text: "line one\n"})
	attached.handleEvent(core.EvToolProgress{ID: "c1", Text: "line two\n"})
	if got := attached.toolCalls["c1"].Progress; got != "line one\nline two\n" {
		t.Fatalf("attached progress: %q", got)
	}
	attached.handleEvent(core.EvToolResult{ID: "c1", Status: "completed"})
	if got := attached.toolCalls["c1"].Progress; got != "" {
		t.Fatalf("progress survived the result: %q", got)
	}
	// Late progress after the result is ignored.
	attached.handleEvent(core.EvToolProgress{ID: "c1", Text: "late"})
	if got := attached.toolCalls["c1"].Progress; got != "" {
		t.Fatalf("late progress: %q", got)
	}

	attached.SetExecutionStatus("awaiting approval: bash")
	if got := attached.executionTag(); got != "attached: host.sock, awaiting approval: bash" {
		t.Fatalf("tag: %q", got)
	}
	attached.SetExecutionStatus("")
	if got := attached.executionTag(); got != "attached: host.sock" {
		t.Fatalf("cleared tag: %q", got)
	}
}

func TestTailBytesKeepsRuneBoundary(t *testing.T) {
	s := strings.Repeat("a", 10) + "ü" + "bc"
	if got := tailBytes(s, 4); got != "übc" {
		t.Fatalf("tail: %q", got)
	}
	// Cutting inside the two-byte rune drops its continuation byte.
	if got := tailBytes(s, 3); got != "bc" {
		t.Fatalf("mid-rune tail: %q", got)
	}
	if got := tailBytes("short", 10); got != "short" {
		t.Fatalf("short: %q", got)
	}
}
