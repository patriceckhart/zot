package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestStartupResourcesRenderAsCompactSections(t *testing.T) {
	v := View{
		Theme:            Dark,
		StartupAgentName: "zot-maintenance",
		StartupContextPaths: []string{
			"/home/user/AGENTS.md",
			"/repo/AGENTS.md",
		},
		StartupExtensionNames: []string{"todo", "workspaces"},
		StartupSkillNames:     []string{"review", "test"},
	}

	plain := stripANSI(strings.Join(v.Build(80), "\n"))
	for _, want := range []string{
		"[Agent]", "zot-maintenance",
		"[Context]", "/home/user/AGENTS.md, /repo/AGENTS.md",
		"[Extensions]", "todo, workspaces",
		"[Skills]", "review, test",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("startup resources missing %q:\n%s", want, plain)
		}
	}
	if strings.ContainsAny(plain, boxGlyphs) || strings.Contains(plain, "read ") {
		t.Fatalf("startup resources rendered as a tool call:\n%s", plain)
	}
	if len(v.Messages) != 0 {
		t.Fatalf("startup resources added %d transcript messages", len(v.Messages))
	}
}

func TestLiveToolOverlayRemainsAfterAssistantToolUse(t *testing.T) {
	args := json.RawMessage(`{"command":"sleep 1"}`)
	v := View{
		Theme: Dark,
		Messages: []provider.Message{
			{
				Role: provider.RoleAssistant,
				Content: []provider.Content{
					provider.ToolCallBlock{ID: "toolu_1", Name: "bash", Arguments: args},
				},
			},
		},
		ToolCalls: []ToolCallView{
			{ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", args), Done: false},
		},
	}

	plain := stripANSI(strings.Join(v.Build(80), "\n"))
	if !strings.Contains(plain, "bash sleep 1") {
		t.Fatalf("live tool overlay disappeared after assistant tool_use was appended:\n%s", plain)
	}
}

func TestLiveToolOverlayShowsFullBashCommandBeforeResult(t *testing.T) {
	args := json.RawMessage(`{"command":"printf 'start' && sleep 60 && printf 'done with a command long enough to exceed the header truncation limit'"}`)
	v := View{
		Theme: Dark,
		ToolCalls: []ToolCallView{
			{
				ID:         "toolu_1",
				Name:       "bash",
				Args:       ShortArgs("bash", args),
				RawJSONBuf: string(args),
			},
		},
	}

	plain := stripANSI(strings.Join(v.BuildLive(100), "\n"))
	if !strings.Contains(plain, "$ printf 'start' && sleep 60") || !strings.Contains(plain, "header truncation limit") {
		t.Fatalf("bash command was not visible before result arrived:\n%s", plain)
	}
}

func TestLiveToolOverlayShowsPowerShellCommand(t *testing.T) {
	args := json.RawMessage(`{"command":"Get-ChildItem -Force"}`)
	v := View{Theme: Dark, ToolCalls: []ToolCallView{{
		ID: "shell_1", Name: "powershell", RawJSONBuf: string(args),
	}}}
	plain := stripANSI(strings.Join(v.BuildLive(80), "\n"))
	if !strings.Contains(plain, "PS> Get-ChildItem -Force") {
		t.Fatalf("PowerShell command missing from live preview: %s", plain)
	}
}

func TestLiveToolOverlayHeightDoesNotShrinkMidStream(t *testing.T) {
	v := View{Theme: Dark}
	tall := json.RawMessage(`{"command":"line1\nline2\nline3\nline4\nline5"}`)
	v.ToolCalls = []ToolCallView{
		{ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", tall), RawJSONBuf: string(tall)},
	}
	tallRows := len(v.Build(80))
	if tallRows == 0 {
		t.Fatal("expected rows for the tall command")
	}

	// Same call id, but the streamed command shrinks (a transient
	// mid-stream state). The box must keep its high-water height so the
	// editor/status band below doesn't bounce.
	short := json.RawMessage(`{"command":"echo hi"}`)
	v.ToolCalls = []ToolCallView{
		{ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", short), RawJSONBuf: string(short)},
	}
	if got := len(v.Build(80)); got < tallRows {
		t.Fatalf("live box shrank mid-stream from %d to %d rows", tallRows, got)
	}
}

func TestEditConfirmationPreviewRendersDiffInsteadOfNewText(t *testing.T) {
	args := json.RawMessage(`{"path":"sample.go","edits":[{"oldText":"old value","newText":"new value"}]}`)
	v := View{
		Theme: Dark,
		ToolCalls: []ToolCallView{{
			ID:         "toolu_1",
			Name:       "edit",
			RawJSONBuf: string(args),
			LivePath:   "sample.go",
			Preview:    "-old value\n+new value\n",
		}},
	}

	rendered := strings.Join(v.Build(80), "\n")
	plain := stripANSI(rendered)
	for _, want := range []string{"-  1 old value", "+  1 new value"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("confirmation preview missing numbered diff row %q:\n%s", want, plain)
		}
	}
	for _, want := range []string{
		Dark.FG256(Dark.Error, "-  1 "),
		Dark.FG256(Dark.Tool, "+  1 "),
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("confirmation preview missing diff color %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(plain, "edit 1 (streaming)") {
		t.Fatalf("confirmation still showed the newText streaming view:\n%s", plain)
	}
}

func TestLiveToolReservationResetsWhenExpansionChanges(t *testing.T) {
	args := json.RawMessage(`{"path":"sample.ts","content":"line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\nline11\nline12\nline13\nline14\nline15\nline16\nline17\nline18\nline19\nline20"}`)
	v := View{
		Theme: Dark,
		ToolCalls: []ToolCallView{
			{ID: "toolu_1", Name: "write", RawJSONBuf: string(args), LivePath: "sample.ts"},
		},
	}

	collapsedRows := len(v.Build(80))
	v.ExpandAll = true
	expandedRows := len(v.Build(80))
	if expandedRows <= collapsedRows {
		t.Fatalf("expanded live box did not grow: collapsed=%d expanded=%d", collapsedRows, expandedRows)
	}

	v.ExpandAll = false
	if got := len(v.Build(80)); got != collapsedRows {
		t.Fatalf("collapsed live box kept expanded height: got %d rows, want %d", got, collapsedRows)
	}
}

func TestLiveToolReservationDoesNotLeakToNextCall(t *testing.T) {
	v := View{Theme: Dark}
	tall := json.RawMessage(`{"command":"line1\nline2\nline3\nline4\nline5"}`)
	v.ToolCalls = []ToolCallView{
		{ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", tall), RawJSONBuf: string(tall)},
	}
	tallRows := len(v.Build(80))

	// A *different* tool call that is short must NOT inherit the tall
	// call's reservation; it renders at its own natural height. (This
	// is the regression that produced a giant empty box.)
	short := json.RawMessage(`{"command":"echo hi"}`)
	v.ToolCalls = []ToolCallView{
		{ID: "toolu_2", Name: "bash", Args: ShortArgs("bash", short), RawJSONBuf: string(short)},
	}
	if got := len(v.Build(80)); got >= tallRows {
		t.Fatalf("short call inherited prior call's height: %d rows (tall was %d)", got, tallRows)
	}
}

func TestLiveToolReservationDropsOnResult(t *testing.T) {
	v := View{Theme: Dark}
	tall := json.RawMessage(`{"command":"line1\nline2\nline3\nline4\nline5"}`)
	v.ToolCalls = []ToolCallView{
		{ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", tall), RawJSONBuf: string(tall)},
	}
	tallRows := len(v.Build(80))

	// Same id, now finalised with a tiny error result. The box must
	// collapse to the natural result height instead of staying padded.
	v.ToolCalls = []ToolCallView{
		{ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", tall), RawJSONBuf: string(tall), Done: true, Error: true, Result: "boom"},
	}
	if got := len(v.Build(80)); got >= tallRows {
		t.Fatalf("finalised box kept live reservation: %d rows (live was %d)", got, tallRows)
	}
}

func TestLiveToolOverlayKeepsWritePreviewAfterArgsEnd(t *testing.T) {
	args := json.RawMessage(`{"path":"/tmp/sample.ts","content":"export const n = 1\n"}`)
	v := View{
		Theme: Dark,
		Messages: []provider.Message{
			{
				Role: provider.RoleAssistant,
				Content: []provider.Content{
					provider.ToolCallBlock{ID: "toolu_1", Name: "write", Arguments: args},
				},
			},
		},
		ToolCalls: []ToolCallView{
			{
				ID:         "toolu_1",
				Name:       "write",
				Args:       ShortArgs("write", args),
				Streaming:  false,
				RawJSONBuf: string(args),
				LivePath:   "/tmp/sample.ts",
			},
		},
	}

	plain := stripANSI(strings.Join(v.Build(80), "\n"))
	if !strings.Contains(plain, "export const n = 1") {
		t.Fatalf("write preview collapsed after tool args ended but before tool_result arrived:\n%s", plain)
	}
}

func TestLiveToolOverlayHidesAfterToolResult(t *testing.T) {
	args := json.RawMessage(`{"command":"sleep 1"}`)
	v := View{
		Theme: Dark,
		Messages: []provider.Message{
			{
				Role: provider.RoleAssistant,
				Content: []provider.Content{
					provider.ToolCallBlock{ID: "toolu_1", Name: "bash", Arguments: args},
				},
			},
			{
				Role: provider.RoleTool,
				Content: []provider.Content{
					provider.ToolResultBlock{
						CallID:  "toolu_1",
						Content: []provider.Content{provider.TextBlock{Text: "done"}},
					},
				},
			},
		},
		ToolCalls: []ToolCallView{
			{ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", args), Result: "done", Done: true},
		},
	}

	plain := stripANSI(strings.Join(v.BuildLive(80), "\n"))
	if strings.Contains(plain, "bash sleep 1") {
		t.Fatalf("live tool overlay still rendered after tool_result was appended:\n%s", plain)
	}
}
