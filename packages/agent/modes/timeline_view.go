package modes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

const timelineTabCount = 5

var timelineTabNames = [timelineTabCount]string{"summary", "payload", "result", "schema", "timing"}

type timelineEntry struct {
	Kind     string    `json:"kind"`
	Turn     int       `json:"turn,omitempty"`
	Label    string    `json:"label"`
	Summary  string    `json:"summary"`
	Time     time.Time `json:"time,omitempty"`
	Duration string    `json:"duration,omitempty"`
	Payload  string    `json:"payload,omitempty"`
	Result   string    `json:"result,omitempty"`
	Schema   string    `json:"schema,omitempty"`
	IsError  bool      `json:"is_error,omitempty"`
	ToolCall string    `json:"tool_call_id,omitempty"`
}

type timelineData struct {
	Session     sessionIdentity
	System      string
	Tools       []provider.Tool
	Messages    []provider.Message
	ContextUsed int
	ContextMax  int
}

type timelineView struct {
	active    bool
	cursor    int
	tab       int
	detailTop int
	searching bool
	filter    string
	notice    string
}

type timelineAction struct {
	Export bool
	Close  bool
}

func newTimelineView() *timelineView { return &timelineView{} }

func (v *timelineView) Active() bool { return v != nil && v.active }

func (v *timelineView) Open(data timelineData) {
	v.active = true
	v.searching = false
	v.filter = ""
	v.notice = ""
	v.tab = 0
	v.detailTop = 0
	v.cursor = len(buildTimelineEntries(data)) - 1
	if v.cursor < 0 {
		v.cursor = 0
	}
}

func (v *timelineView) Close() {
	v.active = false
	v.searching = false
	v.filter = ""
	v.notice = ""
	v.detailTop = 0
}

func (v *timelineView) SetNotice(s string) { v.notice = s }

func (v *timelineView) HandleKey(k tui.Key, data timelineData) timelineAction {
	entries := filterTimelineEntries(buildTimelineEntries(data), v.filter)
	clampTimelineCursor(v, len(entries))

	previousCursor, previousTab := v.cursor, v.tab
	switch k.Kind {
	case tui.KeyLeft:
		if v.detailTop > 0 {
			v.detailTop--
		}
	case tui.KeyRight:
		v.detailTop++
	case tui.KeyHome:
		v.detailTop = 0
	case tui.KeyEnd:
		v.detailTop = int(^uint(0) >> 1)
	case tui.KeyUp:
		if v.cursor > 0 {
			v.cursor--
		}
	case tui.KeyDown:
		if v.cursor < len(entries)-1 {
			v.cursor++
		}
	case tui.KeyPageUp:
		v.cursor -= 8
		if v.cursor < 0 {
			v.cursor = 0
		}
	case tui.KeyPageDown:
		v.cursor += 8
		clampTimelineCursor(v, len(entries))
	case tui.KeyTab:
		v.tab = (v.tab + 1) % timelineTabCount
	case tui.KeyShiftTab:
		v.tab = (v.tab + timelineTabCount - 1) % timelineTabCount
	case tui.KeyCtrlE:
		return timelineAction{Export: true}
	case tui.KeyBackspace:
		if v.searching && v.filter != "" {
			r := []rune(v.filter)
			v.filter = string(r[:len(r)-1])
			v.cursor = 0
			v.detailTop = 0
		}
	case tui.KeyPaste:
		if v.searching {
			v.filter += singleLinePaste(k.Paste)
			v.cursor = 0
			v.detailTop = 0
		}
	case tui.KeyRune:
		if !v.searching && k.Rune == '/' {
			v.searching = true
			v.notice = ""
		} else if v.searching && !k.Ctrl && !k.Alt && !k.Super {
			v.filter += string(k.Rune)
			v.cursor = 0
			v.detailTop = 0
		}
	case tui.KeyEsc, tui.KeyCtrlC:
		if v.searching {
			if v.filter != "" {
				v.filter = ""
				v.cursor = 0
				v.detailTop = 0
			} else {
				v.searching = false
			}
			break
		}
		v.Close()
		return timelineAction{Close: true}
	}
	if v.cursor != previousCursor || v.tab != previousTab {
		v.detailTop = 0
	}
	return timelineAction{}
}

func clampTimelineCursor(v *timelineView, n int) {
	if n == 0 {
		v.cursor = 0
		return
	}
	if v.cursor >= n {
		v.cursor = n - 1
	}
	if v.cursor < 0 {
		v.cursor = 0
	}
}

func (v *timelineView) Render(th tui.Theme, width, height int, data timelineData) []string {
	if !v.Active() {
		return nil
	}
	if width < 30 {
		width = 30
	}
	entries := filterTimelineEntries(buildTimelineEntries(data), v.filter)
	clampTimelineCursor(v, len(entries))

	lines := []string{frameHeader(th, "timeline", width)}
	lines = append(lines, renderSessionIdentity(th, width, data.Session)...)
	lines = append(lines, renderTimelineContext(th, width, data)...)

	hint := "↑/↓ select  tab details  ←/→ scroll detail  home/end  / search  ctrl+e export  esc chat"
	if v.searching {
		hint = "search: " + v.filter + "_  (esc clears)"
	}
	lines = append(lines, th.FG256(th.Muted, fitTimelineLine(hint, width)))

	listRows := (height - 14) / 2
	if listRows < 3 {
		listRows = 3
	}
	if listRows > 12 {
		listRows = 12
	}
	if len(entries) == 0 {
		lines = append(lines, th.FG256(th.Muted, "  no matching events"))
	} else {
		start := v.cursor - listRows/2
		if start < 0 {
			start = 0
		}
		end := start + listRows
		if end > len(entries) {
			end = len(entries)
			start = end - listRows
			if start < 0 {
				start = 0
			}
		}
		for idx := start; idx < end; idx++ {
			line := formatTimelineRow(entries[idx], width)
			if idx == v.cursor {
				lines = append(lines, th.PadHighlight(line, width))
			} else {
				lines = append(lines, timelineColor(th, entries[idx], line))
			}
		}
	}

	lines = append(lines, renderTimelineTabs(th, width, v.tab))
	if len(entries) > 0 {
		detailRows := height - len(lines) - 7
		if detailRows < 3 {
			detailRows = 3
		}
		detail := timelineDetailRows(th, width, entries[v.cursor], v.tab)
		maxTop := len(detail) - detailRows
		if maxTop < 0 {
			maxTop = 0
		}
		if v.detailTop > maxTop {
			v.detailTop = maxTop
		}
		lines = append(lines, detail[v.detailTop:min(v.detailTop+detailRows, len(detail))]...)
	}
	if v.notice != "" {
		lines = append(lines, th.FG256(th.Tool, fitTimelineLine(v.notice, width)))
	}
	lines = append(lines, frameRule(th, width))
	return lines
}

func buildTimelineEntries(data timelineData) []timelineEntry {
	toolSchemas := make(map[string]string, len(data.Tools))
	for _, tool := range data.Tools {
		toolSchemas[tool.Name] = prettyTimelineJSON(tool.Schema)
	}

	results := map[string]provider.ToolResultBlock{}
	resultTimes := map[string]time.Time{}
	for _, message := range data.Messages {
		for _, content := range message.Content {
			if result, ok := content.(provider.ToolResultBlock); ok {
				results[result.CallID] = result
				resultTimes[result.CallID] = message.Time
			}
		}
	}

	entries := make([]timelineEntry, 0, len(data.Messages)+1)
	if strings.TrimSpace(data.System) != "" {
		entries = append(entries, timelineEntry{
			Kind: "system", Label: "SYSTEM", Summary: firstTimelineLine(data.System), Payload: data.System,
		})
	}
	turn := 0
	var previousTime time.Time
	for _, message := range data.Messages {
		if message.Role == provider.RoleUser {
			turn++
		}
		for _, content := range message.Content {
			if block, ok := content.(provider.ReasoningBlock); ok && strings.TrimSpace(block.Summary) != "" {
				entries = append(entries, timelineEntry{
					Kind: "reasoning", Turn: turn, Label: "REASONING", Summary: firstTimelineLine(block.Summary),
					Time: message.Time, Payload: block.Summary,
				})
			}
		}
		textParts := timelineMessageText(message)
		if len(textParts) > 0 {
			entry := timelineEntry{
				Kind: string(message.Role), Turn: turn, Label: strings.ToUpper(string(message.Role)),
				Summary: firstTimelineLine(strings.Join(textParts, "\n")), Time: message.Time,
				Payload: prettyTimelineValue(timelineMessagePayload(message)),
			}
			if !previousTime.IsZero() && !message.Time.IsZero() && message.Time.After(previousTime) {
				entry.Duration = message.Time.Sub(previousTime).Round(time.Millisecond).String()
			}
			entries = append(entries, entry)
			if !message.Time.IsZero() {
				previousTime = message.Time
			}
		}
		for _, content := range message.Content {
			call, ok := content.(provider.ToolCallBlock)
			if !ok {
				continue
			}
			entry := timelineEntry{
				Kind: "tool", Turn: turn, Label: "TOOL " + call.Name, Summary: summarizeToolArguments(call.Arguments),
				Time: message.Time, Payload: prettyTimelineJSON(call.Arguments), Schema: toolSchemas[call.Name], ToolCall: call.ID,
			}
			if result, found := results[call.ID]; found {
				entry.Result = prettyTimelineValue(timelineResultPayload(result))
				entry.IsError = result.IsError
				if ended := resultTimes[call.ID]; !message.Time.IsZero() && ended.After(message.Time) {
					entry.Duration = ended.Sub(message.Time).Round(time.Millisecond).String()
				}
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

func timelineMessageText(message provider.Message) []string {
	var out []string
	for _, content := range message.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			if strings.TrimSpace(block.Text) != "" {
				out = append(out, block.Text)
			}
		case provider.ImageBlock:
			out = append(out, fmt.Sprintf("[image %s, %d bytes]", block.MimeType, len(block.Data)))
		}
	}
	return out
}

func timelineMessagePayload(message provider.Message) any {
	blocks := make([]any, 0, len(message.Content))
	for _, content := range message.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			blocks = append(blocks, map[string]any{"type": "text", "text": block.Text})
		case provider.ReasoningBlock:
			blocks = append(blocks, map[string]any{"type": "reasoning", "summary": block.Summary})
		case provider.ImageBlock:
			blocks = append(blocks, map[string]any{"type": "image", "mime_type": block.MimeType, "bytes": len(block.Data)})
		}
	}
	return map[string]any{"role": message.Role, "content": blocks, "meta": message.Meta}
}

func timelineResultPayload(result provider.ToolResultBlock) any {
	parts := make([]any, 0, len(result.Content))
	for _, content := range result.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case provider.ImageBlock:
			parts = append(parts, map[string]any{"type": "image", "mime_type": block.MimeType, "bytes": len(block.Data)})
		default:
			parts = append(parts, fmt.Sprintf("%T", content))
		}
	}
	return map[string]any{"content": parts, "is_error": result.IsError}
}

func filterTimelineEntries(entries []timelineEntry, filter string) []timelineEntry {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return entries
	}
	out := make([]timelineEntry, 0, len(entries))
	for _, entry := range entries {
		haystack := strings.ToLower(strings.Join([]string{entry.Label, entry.Summary, entry.Payload, entry.Result}, "\n"))
		if strings.Contains(haystack, filter) {
			out = append(out, entry)
		}
	}
	return out
}

func renderTimelineContext(th tui.Theme, width int, data timelineData) []string {
	system := estimateTimelineTokens(data.System)
	toolBytes, _ := json.Marshal(data.Tools)
	tools := estimateTimelineTokens(string(toolBytes))
	messages := estimateTimelineMessageTokens(data.Messages)

	usage := fmt.Sprintf("context %s", compactTimelineNumber(data.ContextUsed))
	if data.ContextMax > 0 {
		pct := float64(data.ContextUsed) * 100 / float64(data.ContextMax)
		usage = fmt.Sprintf("context %s / %s (%.1f%%)", compactTimelineNumber(data.ContextUsed), compactTimelineNumber(data.ContextMax), pct)
	}
	composition := fmt.Sprintf("estimated composition: system %s  tools %s  messages %s", compactTimelineNumber(system), compactTimelineNumber(tools), compactTimelineNumber(messages))
	return []string{tui.Bold(usage), th.FG256(th.Muted, fitTimelineLine(composition, width))}
}

func formatTimelineRow(entry timelineEntry, width int) string {
	stamp := "        "
	if !entry.Time.IsZero() {
		stamp = entry.Time.Format("15:04:05")
	}
	turn := "   "
	if entry.Turn > 0 {
		turn = fmt.Sprintf("%3d", entry.Turn)
	}
	line := fmt.Sprintf("%s  %s  %-16s  %s", stamp, turn, entry.Label, entry.Summary)
	return fitTimelineLine(line, width)
}

func timelineColor(th tui.Theme, entry timelineEntry, line string) string {
	color := th.Muted
	switch entry.Kind {
	case "user":
		color = th.User
	case "assistant", "reasoning":
		color = th.Assistant
	case "tool":
		color = th.Tool
	case "system":
		color = th.Warning
	}
	if entry.IsError {
		color = th.Error
	}
	return th.FG256(color, line)
}

func renderTimelineTabs(th tui.Theme, width, selected int) string {
	if selected < 0 || selected >= len(timelineTabNames) || width < 1 {
		return ""
	}
	start, end := selected, selected+1
	for start > 0 || end < len(timelineTabNames) {
		grew := false
		if start > 0 && timelineTabsWidth(start-1, end) <= width {
			start--
			grew = true
		}
		if end < len(timelineTabNames) && timelineTabsWidth(start, end+1) <= width {
			end++
			grew = true
		}
		if !grew {
			break
		}
	}

	tabs := make([]string, 0, end-start)
	for idx := start; idx < end; idx++ {
		name := timelineTabNames[idx]
		if idx == selected {
			tabs = append(tabs, tui.Bold(th.FG256(th.Accent, "["+name+"]")))
		} else {
			tabs = append(tabs, th.FG256(th.Muted, " "+name+" "))
		}
	}
	return strings.Join(tabs, "  ")
}

func timelineTabsWidth(start, end int) int {
	width := 2 * (end - start - 1)
	for idx := start; idx < end; idx++ {
		width += runewidth.StringWidth(timelineTabNames[idx]) + 2
	}
	return width
}

func timelineDetailRows(th tui.Theme, width int, entry timelineEntry, tab int) []string {
	var detail string
	switch tab {
	case 0:
		detail = fmt.Sprintf("type: %s\nturn: %d\nstatus: %s\nsummary: %s", entry.Kind, entry.Turn, timelineStatus(entry), entry.Summary)
	case 1:
		detail = entry.Payload
	case 2:
		detail = entry.Result
	case 3:
		detail = entry.Schema
	case 4:
		started := "not recorded"
		if !entry.Time.IsZero() {
			started = entry.Time.Format(time.RFC3339Nano)
		}
		duration := entry.Duration
		if duration == "" {
			duration = "not recorded"
		}
		detail = "started: " + started + "\nduration: " + duration + "\nsource: transcript timestamps"
	}
	if strings.TrimSpace(detail) == "" {
		detail = "not available for this event"
	}
	if entry.Kind == "reasoning" && (tab == 0 || tab == 1) {
		return renderDialogMarkdownRows(entry.Payload, th, width)
	}
	var rows []string
	for _, line := range strings.Split(detail, "\n") {
		for _, wrapped := range tui.WrapANSILine(line, width-2) {
			rows = append(rows, "  "+th.FG256(th.ToolOut, wrapped))
		}
	}
	return rows
}

func timelineStatus(entry timelineEntry) string {
	if entry.IsError {
		return "error"
	}
	if entry.Kind == "tool" && entry.Result == "" {
		return "result unavailable"
	}
	return "completed"
}

func (v *timelineView) Export(dir string, data timelineData) (string, error) {
	if dir == "" {
		dir = defaultExportDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create export directory: %w", err)
	}
	payload := struct {
		ExportedAt  time.Time       `json:"exported_at"`
		ContextUsed int             `json:"context_used"`
		ContextMax  int             `json:"context_max"`
		Events      []timelineEntry `json:"events"`
	}{time.Now(), data.ContextUsed, data.ContextMax, buildTimelineEntries(data)}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode timeline: %w", err)
	}
	name := "zot-timeline-" + time.Now().Format("20060102-150405.000000000") + ".json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write timeline: %w", err)
	}
	return path, nil
}

func firstTimelineLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return fitTimelineLine(line, 120)
		}
	}
	return "(empty)"
}

func summarizeToolArguments(raw json.RawMessage) string {
	var object map[string]any
	if json.Unmarshal(raw, &object) == nil {
		for _, key := range []string{"command", "path", "query", "pattern"} {
			if value, ok := object[key]; ok {
				return firstTimelineLine(fmt.Sprint(value))
			}
		}
	}
	return firstTimelineLine(string(raw))
}

func prettyTimelineJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	return prettyTimelineValue(value)
}

func prettyTimelineValue(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func estimateTimelineTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len([]byte(s)) + 3) / 4
}

func estimateTimelineMessageTokens(messages []provider.Message) int {
	bytes := 0
	for _, message := range messages {
		bytes += len(message.Role) + 12
		for _, content := range message.Content {
			switch block := content.(type) {
			case provider.TextBlock:
				bytes += len(block.Text) + len(block.ThoughtSignature)
			case provider.ReasoningBlock:
				bytes += len(block.Summary) + len(block.Encrypted)
			case provider.ImageBlock:
				// JSON transports image bytes as base64, which expands by roughly 4/3.
				bytes += (len(block.Data)*4 + 2) / 3
			case provider.ToolCallBlock:
				bytes += len(block.ID) + len(block.Name) + len(block.Arguments)
			case provider.ToolResultBlock:
				bytes += len(block.CallID) + estimateTimelineResultBytes(block.Content)
			}
		}
	}
	return (bytes + 3) / 4
}

func estimateTimelineResultBytes(contents []provider.Content) int {
	bytes := 0
	for _, content := range contents {
		switch block := content.(type) {
		case provider.TextBlock:
			bytes += len(block.Text)
		case provider.ImageBlock:
			bytes += (len(block.Data)*4 + 2) / 3
		}
	}
	return bytes
}

func compactTimelineNumber(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func fitTimelineLine(s string, width int) string {
	if width < 1 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	return runewidth.Truncate(s, width, "...")
}
