package tui

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func wipTestMessage(role provider.Role, text string, draft bool, since string) provider.Message {
	return provider.Message{Role: role, WIP: draft, Meta: map[string]string{"wip_since": since}, Content: []provider.Content{provider.TextBlock{Text: text}}}
}

func TestWIPPhaseBoundaries(t *testing.T) {
	draft := wipTestMessage(provider.RoleUser, "draft prompt", true, "phase-1")
	answer := wipTestMessage(provider.RoleAssistant, "draft answer", false, "")
	normal := wipTestMessage(provider.RoleUser, "regular prompt", false, "")
	restarted := wipTestMessage(provider.RoleUser, "second phase", true, "phase-2")
	for _, tc := range []struct {
		name         string
		messages     []provider.Message
		active       bool
		since        string
		tail         int
		starts, ends int
	}{
		{name: "inactive"},
		{name: "enabled before prompting", active: true, since: "phase-1", starts: 1},
		{name: "ongoing phase", messages: []provider.Message{draft, answer, draft, answer}, active: true, since: "phase-1", starts: 1},
		{name: "disabled without another prompt", messages: []provider.Message{draft, answer, draft, answer}, starts: 1, ends: 1},
		{name: "regular conversation", messages: []provider.Message{normal, answer}},
		{name: "next regular prompt", messages: []provider.Message{draft, answer, normal, answer}, starts: 1, ends: 1},
		{name: "restart without regular input", messages: []provider.Message{draft, answer, restarted, answer}, active: true, since: "phase-2", starts: 2, ends: 1},
		{name: "restart before prompting", messages: []provider.Message{draft, answer}, active: true, since: "phase-2", starts: 2, ends: 1},
		{name: "trimmed resume tail", messages: []provider.Message{draft, answer}, active: true, since: "phase-1", tail: 1, starts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, compact := range []bool{false, true} {
				v := &View{Theme: Dark, Messages: tc.messages, WIPActive: tc.active, WIPSince: tc.since, TailLimit: tc.tail, CompactUser: compact}
				lines, anchors := v.BuildWithAnchors(80)
				text := strings.Join(lines, "\n")
				if strings.Count(text, "wip start") != tc.starts || strings.Count(text, "wip end") != tc.ends {
					t.Fatalf("unexpected boundaries: %s", text)
				}
				if len(anchors) != len(tc.messages) {
					t.Fatal("message anchors lost")
				}
				for idx, line := range lines {
					if strings.Contains(line, "wip start") || strings.Contains(line, "wip end") {
						if visibleWidth(line) != 80 || idx+1 >= len(lines) || lines[idx+1] != "" {
							t.Fatal("boundary lacks full-width rule or blank line")
						}
					}
				}
			}
		})
	}
}

func TestWIPBoundaryPlacementAndCache(t *testing.T) {
	v := &View{Theme: Dark, WIPActive: true, WIPSince: "phase-1", Messages: []provider.Message{
		wipTestMessage(provider.RoleUser, "draft prompt", true, "phase-1"),
		wipTestMessage(provider.RoleAssistant, "draft answer", false, ""),
	}}
	v.Build(80)
	v.WIPActive = false
	text := strings.Join(v.Build(80), "\n")
	if strings.Count(text, "wip start") != 1 || strings.Count(text, "wip end") != 1 || strings.Index(text, "wip end") < strings.Index(text, "draft answer") {
		t.Fatal("cached messages lost the end marker or placed it before the response")
	}
	v.Messages = append(v.Messages, wipTestMessage(provider.RoleUser, "regular prompt", false, ""))
	text = strings.Join(v.Build(80), "\n")
	if strings.Index(text, "wip end") > strings.Index(text, "regular prompt") {
		t.Fatal("end marker follows the regular prompt")
	}
}

func TestWIPBoundaryFollowsLiveOutput(t *testing.T) {
	v := &View{Theme: Dark, Messages: []provider.Message{wipTestMessage(provider.RoleUser, "draft prompt", true, "phase-1")}, StreamingActive: true, Streaming: "streaming answer"}
	text := strings.Join(v.Build(80), "\n")
	if strings.Index(text, "wip end") < strings.Index(text, "streaming answer") {
		t.Fatal("end marker precedes the live response")
	}
}

func TestWIPBoundaryWidth(t *testing.T) {
	v := &View{Theme: Dark}
	for _, width := range []int{1, 4, 8, 15, 20, 80} {
		lines := v.appendWIPMarkers(nil, []string{"wip start", "wip end"}, width)
		if len(lines) != 4 || visibleWidth(lines[0]) != width || visibleWidth(lines[2]) != width || lines[1] != "" || lines[3] != "" {
			t.Fatalf("invalid marker layout at width %d: %q", width, lines)
		}
	}
}

func TestWIPStatusBadge(t *testing.T) {
	for _, cols := range []int{8, 40, 120} {
		theme := Dark
		theme.Warning = 123
		lines := StatusBar(StatusBarParams{Theme: theme, Model: "test", WIP: true, Cols: cols})
		if !strings.Contains(strings.Join(lines, "\n"), theme.FG256(theme.Warning, "WIP")) {
			t.Fatalf("missing warning badge at %d columns", cols)
		}
		color := 42
		theme.WIP = &color
		lines = StatusBar(StatusBarParams{Theme: theme, Model: "test", WIP: true, Cols: cols})
		if !strings.Contains(strings.Join(lines, "\n"), theme.FG256(color, "WIP")) {
			t.Fatal("custom color ignored")
		}
		lines = StatusBar(StatusBarParams{Theme: theme, Model: "test", Cols: cols})
		if strings.Contains(strings.Join(lines, "\n"), "WIP") {
			t.Fatal("inactive badge")
		}
	}
}
