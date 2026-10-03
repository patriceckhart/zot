package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestNestedToolsRenderOnceInEveryDisplayMode(t *testing.T) {
	calls := map[string][]provider.NestedToolCall{"parent": {
		{ID: "parent/1", Name: "read", Args: json.RawMessage(`{"path":"nested.txt","offset":7,"limit":1}`), Result: "nested result", Status: "completed", Executed: true},
		{ID: "parent/2", Name: "write", Args: json.RawMessage(`{"path":"blocked.txt"}`), Result: "permission refused", IsError: true, Status: "blocked"},
	}}
	raw, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"boxes", "flat", "compact", "collapsed"} {
		t.Run(mode, func(t *testing.T) {
			v := View{Theme: Dark, FlatTools: mode == "flat", CompactMode: mode == "compact", CollapseToolCall: mode == "collapsed", Messages: []provider.Message{
				{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "parent", Name: "batch", Arguments: json.RawMessage(`{}`)}}},
				{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "parent", Content: []provider.Content{provider.TextBlock{Text: "outer result"}}}}, Meta: map[string]string{provider.NestedToolCallsMetaKey: string(raw)}},
				{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "final answer"}}},
			}, ToolCalls: []ToolCallView{{ID: "parent/1", Name: "read", Args: "nested.txt", Result: "nested result", Done: true}, {ID: "parent/2", Name: "write", Result: "permission refused", Error: true, Done: true}}}
			for _, expanded := range []bool{false, true} {
				v.ExpandAll = expanded
				chat := stripANSI(strings.Join(v.Build(100), "\n"))
				for _, want := range []string{"read nested.txt:7-7", "nested result", "write blocked.txt", "permission refused"} {
					if strings.Count(chat, want) != 1 {
						t.Fatalf("nested display missing or duplicated %q:\n%s", want, chat)
					}
				}
				if strings.Index(chat, "nested result") < strings.Index(chat, "outer result") || strings.Index(chat, "permission refused") > strings.Index(chat, "final answer") {
					t.Fatalf("nested calls detached from parent result:\n%s", chat)
				}
				if live := v.BuildLive(100); len(live) != 0 {
					t.Fatalf("persisted nested calls remained in live overlay: %v", live)
				}
			}
			// Adding display metadata must invalidate a cached parent render.
			v.Messages[1].Meta = nil
			if chat := stripANSI(strings.Join(v.Build(100), "\n")); strings.Contains(chat, "read nested.txt:7-7") {
				t.Fatal("render cache retained removed display metadata")
			}
		})
	}
}
