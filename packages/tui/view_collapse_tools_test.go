package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestCollapseToolCallPreservesLayout(t *testing.T) {
	for _, layout := range []string{"box", "flat", "compact"} {
		for _, tool := range []struct {
			name, args, preview string
		}{
			{"bash", `{"command":"printf hello"}`, "$ printf hello"},
			{"write", `{"path":"test.txt","content":"hello"}`, "hello"},
			{"edit", `{"path":"test.txt","edits":[{"oldText":"old","newText":"hello"}]}`, "hello"},
		} {
			t.Run(layout+"/"+tool.name, func(t *testing.T) {
				v := View{Theme: Dark, CollapseToolCall: true, FlatTools: layout == "flat", CompactMode: layout == "compact"}
				for _, expanded := range []bool{false, true} {
					v.ExpandAll = expanded
					rows := v.RenderToolCall(ToolCallView{Name: tool.name, RawJSONBuf: tool.args}, 80)
					found := false
					for _, row := range rows {
						plain := stripANSI(row)
						if strings.Contains(plain, tool.preview) {
							found = true
							want := 0
							if layout == "box" {
								want = 2
							}
							if got := strings.Count(plain, "│"); got != want {
								t.Errorf("expanded=%v: got %d borders, want %d: %s", expanded, got, want, plain)
							}
						}
						if layout != "box" && strings.ContainsAny(plain, "┌┐└┘│") {
							t.Errorf("expanded=%v: unexpected frame: %s", expanded, plain)
						}
						if visibleWidth(row) > 80 {
							t.Errorf("expanded=%v: row exceeds terminal width: %s", expanded, plain)
						}
					}
					if !found {
						t.Fatalf("expanded=%v: missing preview: %v", expanded, rows)
					}
				}
			})
		}
	}
}

func TestCollapseToolCallPreservesErrors(t *testing.T) {
	for _, layout := range []string{"box", "flat", "compact"} {
		for _, text := range []string{"", "failed", strings.Repeat("output\n", ToolCollapseLines+1)} {
			for _, live := range []bool{false, true} {
				v := View{Theme: Dark, CollapseToolCall: true, FlatTools: layout == "flat", CompactMode: layout == "compact"}
				var rows []string
				if live {
					rows = v.RenderToolCall(ToolCallView{Name: "bash", Result: text, Error: true}, 80)
				} else {
					rows = v.renderMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{
						provider.ToolResultBlock{IsError: true, Content: []provider.Content{provider.TextBlock{Text: text}}},
					}}, 80, false)
				}
				rendered := strings.Join(rows, "\n")
				if !strings.Contains(rendered, v.Theme.FG256(v.Theme.Error, "error")) {
					t.Errorf("%s live=%v: missing error indicator: %s", layout, live, stripANSI(rendered))
				}
				if text == "failed" && !strings.Contains(rendered, v.Theme.FG256(v.Theme.Error, text)) {
					t.Errorf("%s live=%v: missing error styling: %q", layout, live, rendered)
				}
				if layout != "box" && strings.ContainsAny(stripANSI(rendered), "┌┐└┘│") {
					t.Errorf("%s live=%v: unexpected frame: %s", layout, live, stripANSI(rendered))
				}
			}
		}
	}
}

func TestCollapseToolCallShowsLastBoxPreviewLine(t *testing.T) {
	args := json.RawMessage(`{"command":"echo preview"}`)
	v := View{Theme: Dark, CollapseToolCall: true, ToolCalls: []ToolCallView{{
		ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", args), Result: "first-output\nlast-output\n",
	}}}
	plain := stripANSI(strings.Join(v.BuildLive(80), "\n"))
	if !strings.Contains(plain, "bash") {
		t.Fatalf("collapsed render lost tool header:\n%s", plain)
	}
	if !strings.Contains(plain, "last-output") || strings.Contains(plain, "first-output") {
		t.Fatalf("collapsed render did not show only the last result line:\n%s", plain)
	}
	if !strings.ContainsAny(plain, "┌┐") || !strings.ContainsAny(plain, "└┘") || !strings.Contains(plain, "│") {
		t.Fatalf("collapsed box render did not retain the complete empty frame:\n%s", plain)
	}
	v.ExpandAll = true
	expanded := stripANSI(strings.Join(v.BuildLive(80), "\n"))
	if !strings.Contains(expanded, "first-output") {
		t.Fatalf("ctrl-o expansion did not reveal non-compact collapsed tool result:\n%s", expanded)
	}
}

func TestCollapseToolCallShowsLastCompactPreviewLine(t *testing.T) {
	args := json.RawMessage(`{"command":"echo preview"}`)
	v := View{
		Theme:            Dark,
		CompactMode:      true,
		CollapseToolCall: true,
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []provider.Content{
				provider.ToolCallBlock{ID: "toolu_1", Name: "bash", Arguments: args},
			}},
			{Role: provider.RoleTool, Content: []provider.Content{
				provider.ToolResultBlock{CallID: "toolu_1", Content: []provider.Content{provider.TextBlock{Text: "first-output\nlast-output"}}},
			}},
		},
	}
	plain := stripANSI(strings.Join(v.Build(80), "\n"))
	if !strings.Contains(plain, "bash") {
		t.Fatalf("collapsed compact render lost tool header:\n%s", plain)
	}
	if !strings.Contains(plain, "last-output") || strings.Contains(plain, "first-output") {
		t.Fatalf("collapsed compact render did not show only the last result line:\n%s", plain)
	}
	v.ExpandAll = true
	expanded := stripANSI(strings.Join(v.Build(80), "\n"))
	if !strings.Contains(expanded, "first-output") {
		t.Fatalf("ctrl-o expansion did not reveal collapsed tool result:\n%s", expanded)
	}
}

func TestCollapseToolCallShowsLastLivePreviewLine(t *testing.T) {
	args := json.RawMessage(`{"command":"echo preview"}`)
	v := View{Theme: Dark, CollapseToolCall: true, ToolCalls: []ToolCallView{{
		ID: "toolu_1", Name: "bash", Args: ShortArgs("bash", args), RawJSONBuf: string(args),
	}}}
	plain := stripANSI(strings.Join(v.BuildLive(80), "\n"))
	if !strings.Contains(plain, "$ echo") {
		t.Fatalf("collapsed live render lost its one-line preview:\n%s", plain)
	}
	if !strings.Contains(plain, "bash") {
		t.Fatalf("collapsed live render lost tool header:\n%s", plain)
	}
}
