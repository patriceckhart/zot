package modes

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func timelineTestData() timelineData {
	started := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	finished := started.Add(1250 * time.Millisecond)
	return timelineData{
		System: "You are zot.",
		Tools: []provider.Tool{{
			Name: "bash", Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		}},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Time: started.Add(-time.Second), Content: []provider.Content{provider.TextBlock{Text: "list files"}}},
			{Role: provider.RoleAssistant, Time: started, Content: []provider.Content{
				provider.TextBlock{Text: "I will inspect them."},
				provider.ToolCallBlock{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls -la"}`)},
			}},
			{Role: provider.RoleTool, Time: finished, Content: []provider.Content{
				provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: "README.md"}}},
			}},
		},
		ContextUsed: 1200,
		ContextMax:  200000,
	}
}

func TestSessionTimelineOpensAlternateView(t *testing.T) {
	interactive := &Interactive{timeline: newTimelineView()}
	interactive.runSlash(context.Background(), "/session timeline")
	if !interactive.timeline.Active() {
		t.Fatal("/session timeline did not open the timeline view")
	}
}

func TestSessionPickerIncludesTimeline(t *testing.T) {
	interactive := &Interactive{sessionOpsDialog: newSessionOpsDialog()}
	interactive.openSessionOpsDialog()
	if len(interactive.sessionOpsDialog.items) == 0 || interactive.sessionOpsDialog.items[0].action != "timeline" {
		t.Fatalf("session actions = %+v, want timeline first", interactive.sessionOpsDialog.items)
	}
}

func TestBuildTimelineEntriesPairsToolDetails(t *testing.T) {
	entries := buildTimelineEntries(timelineTestData())
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want system, user, assistant, and tool", len(entries))
	}
	tool := entries[3]
	if tool.Kind != "tool" || tool.ToolCall != "call-1" {
		t.Fatalf("tool entry = %+v", tool)
	}
	if !strings.Contains(tool.Payload, "ls -la") || !strings.Contains(tool.Result, "README.md") {
		t.Fatalf("tool details missing: %+v", tool)
	}
	if !strings.Contains(tool.Schema, "command") {
		t.Fatalf("tool schema missing: %q", tool.Schema)
	}
	if tool.Duration != "1.25s" {
		t.Fatalf("duration = %q, want 1.25s", tool.Duration)
	}
}

func TestTimelineCompactionPayloadRendersMarkdown(t *testing.T) {
	const summary = "## Context Summary\n\n**Goal:** " +
		"A long description that wraps across several lines."
	message := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: summary}},
		Meta:    map[string]string{"compaction": "true"},
	}
	entries := buildTimelineEntries(timelineData{Messages: []provider.Message{message, {
		Role: provider.RoleUser, Content: message.Content,
	}}})
	th := tui.Dark
	rows := timelineDetailRows(th, 32, entries[0], 1)
	plain := stripANSIBytes(strings.Join(rows, "\n"))
	if !strings.Contains(rows[0], th.FG256(th.Accent, "## Context Summary")) ||
		!strings.Contains(plain, "Goal:") || strings.Contains(plain, "**") || strings.Contains(plain, `"content"`) {
		t.Fatalf("expected rendered Markdown, got %q", plain)
	}
	for _, row := range rows {
		if runewidth.StringWidth(stripANSIBytes(row)) > 32 {
			t.Fatalf("row exceeds width: %q", row)
		}
	}
	for tab, want := range map[int]string{0: "type: user", 1: "{"} {
		if got := timelineDetailRows(th, 80, entries[1], tab)[0]; got != "  "+th.FG256(th.FG, want) {
			t.Errorf("tab %d first row = %q", tab, got)
		}
	}
	encoded, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var exported map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &exported); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := json.Unmarshal(exported["payload"], &payload); err != nil || !json.Valid([]byte(payload)) || !strings.Contains(payload, "**Goal:**") {
		t.Fatalf("export payload is not the original JSON: %q, %v", payload, err)
	}
}

func TestTimelineReasoningIsReadableAndScrollable(t *testing.T) {
	data := timelineTestData()
	data.Messages[1].Content = append([]provider.Content{provider.ReasoningBlock{
		Summary: "**First** step\n" + strings.Repeat("A long line of thought. ", 25) + "\nFinal insight",
	}}, data.Messages[1].Content...)
	entries := buildTimelineEntries(data)
	if len(entries) != 5 || entries[2].Kind != "reasoning" || entries[3].Kind != "assistant" {
		t.Fatalf("reasoning entries = %+v", entries)
	}
	if strings.Contains(entries[3].Summary, "First") {
		t.Fatalf("reasoning duplicated in assistant: %+v", entries[3])
	}
	view := newTimelineView()
	view.Open(data)
	view.cursor = 2
	rows := timelineDetailRows(tui.Dark, 40, entries[2], 0)
	if len(rows) < 4 || !strings.Contains(stripANSIBytes(strings.Join(rows, "\n")), "Final insight") {
		t.Fatalf("reasoning detail not complete: %q", rows)
	}
	for _, row := range rows {
		if runewidth.StringWidth(stripANSIBytes(row)) > 40 {
			t.Fatalf("row exceeds width: %q", row)
		}
	}
	first := strings.Join(view.Render(tui.Dark, 40, 17, data), "\n")
	view.HandleKey(tui.Key{Kind: tui.KeyEnd}, data)
	last := strings.Join(view.Render(tui.Dark, 40, 17, data), "\n")
	if view.detailTop == 0 || first == last || !strings.Contains(stripANSIBytes(last), "Final insight") {
		t.Fatalf("detail did not scroll to end: top=%d, output=%q", view.detailTop, last)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyLeft}, data)
	if view.detailTop == 0 {
		t.Fatal("left did not scroll detail up")
	}
	view.HandleKey(tui.Key{Kind: tui.KeyTab}, data)
	if view.detailTop != 0 {
		t.Fatal("changing tab did not reset detail scroll")
	}
}

func TestTimelineNavigationKeysMoveSelection(t *testing.T) {
	data := timelineTestData()
	view := newTimelineView()
	view.Open(data)
	last := len(buildTimelineEntries(data)) - 1
	if view.cursor != last {
		t.Fatalf("initial cursor = %d, want %d", view.cursor, last)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyUp}, data)
	if view.cursor != last-1 {
		t.Fatalf("cursor after up = %d, want %d", view.cursor, last-1)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyDown}, data)
	if view.cursor != last {
		t.Fatalf("cursor after down = %d, want %d", view.cursor, last)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyPageUp}, data)
	if view.cursor != 0 {
		t.Fatalf("cursor after page up = %d, want 0", view.cursor)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyPageDown}, data)
	if view.cursor != last {
		t.Fatalf("cursor after page down = %d, want %d", view.cursor, last)
	}
}

func TestTimelineSearchAndTabs(t *testing.T) {
	data := timelineTestData()
	view := newTimelineView()
	view.Open(data)
	view.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '/'}, data)
	for _, r := range "readme" {
		view.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r}, data)
	}
	matches := filterTimelineEntries(buildTimelineEntries(data), view.filter)
	if len(matches) != 1 || matches[0].Kind != "tool" {
		t.Fatalf("matches = %+v, want tool result", matches)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyTab}, data)
	if view.tab != 1 {
		t.Fatalf("tab = %d, want payload tab", view.tab)
	}
	view.HandleKey(tui.Key{Kind: tui.KeyEsc}, data)
	if view.filter != "" || !view.Active() {
		t.Fatal("first esc should clear search without closing timeline")
	}
}

func TestTimelineTabsKeepSelectedTabVisible(t *testing.T) {
	const width = 30
	for selected, name := range timelineTabNames {
		rendered := renderTimelineTabs(tui.Theme{}, width, selected)
		plain := stripANSIBytes(rendered)
		if !strings.Contains(plain, "["+name+"]") {
			t.Errorf("selected tab %q not visible in %q", name, plain)
		}
		if got := runewidth.StringWidth(plain); got > width {
			t.Errorf("tabs width = %d, want <= %d: %q", got, width, plain)
		}
	}
}

func TestTimelineRenderShowsContextAndControls(t *testing.T) {
	data := timelineTestData()
	if contextLines := renderTimelineContext(tui.Theme{}, 100, data); len(contextLines) != 2 {
		t.Fatalf("context rows = %d, want usage and composition only", len(contextLines))
	}
	view := newTimelineView()
	view.Open(data)
	rendered := strings.Join(view.Render(tui.Theme{}, 100, 40, data), "\n")
	for _, want := range []string{"timeline", "context 1.2k / 200.0k", "estimated composition", "ctrl+e export", "TOOL bash"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

func TestTimelineExportOmitsImageDataAndUsesPrivatePermissions(t *testing.T) {
	data := timelineTestData()
	data.Messages = append(data.Messages, provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.ImageBlock{MimeType: "image/png", Data: []byte("secret-image-bytes")}},
	})
	view := newTimelineView()
	path, err := view.Export(t.TempDir(), data)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "secret-image-bytes") {
		t.Fatal("export embedded image data")
	}
	if !strings.Contains(string(contents), "image/png") {
		t.Fatal("export omitted image metadata")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o, want 600", info.Mode().Perm())
	}
}
