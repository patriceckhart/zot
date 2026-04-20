package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/patriceckhart/zot/internal/provider"
)

// pathFromToolArgs returns the "path" argument from a tool_call's
// JSON arguments, or "" if the args aren't a JSON object or don't
// include one. Used to pick a syntax language for rendering the
// corresponding tool_result.
func pathFromToolArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"path", "file_path"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// offsetFromToolArgs returns the read tool's 1-indexed `offset`
// arg (the first line of the slice the tool was asked to return),
// or 0 when the call didn't specify one. Used by the tui to draw
// the line-number gutter aligned to the right starting row, even
// though the tool's text content itself no longer carries line
// numbers.
func offsetFromToolArgs(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	switch v := m["offset"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// osUserHomeDir is aliased so the test file can swap it.
var osUserHomeDir = os.UserHomeDir

// View turns a transcript + live state into a slice of styled lines,
// already wrapped to width.
type View struct {
	Theme      Theme
	ImageProto ImageProtocol // how to render inline images in this terminal
	Messages   []provider.Message
	// toolPaths maps tool_use_id to the "path" argument of the call, if
	// any, so tool_result rendering can pick the right syntax language.
	// Rebuilt on each Build().
	toolPaths map[string]string
	// toolStartLines maps tool_use_id to the 1-indexed first line
	// number of a `read` result, pulled from the call's offset arg.
	// Used by renderNumberedFile to draw a line-number gutter over
	// raw (unnumbered) file content the model receives. Rebuilt on
	// each Build().
	toolStartLines  map[string]int
	Streaming       string // current assistant text delta
	StreamingActive bool
	ToolCalls       []ToolCallView // tool calls in flight or completed
	StatusLine      string
	Err             string

	// ExpandAll forces every long tool result to render in full.
	// Toggled from the tui by ctrl+o. When false, results longer than
	// ToolCollapseLines collapse to ToolCollapsePreview lines plus a
	// "... (N more lines, M total, ctrl+o to expand)" footer.
	ExpandAll bool

	// renderCache holds the per-message rendered line slices so Build
	// doesn't re-markdown every message on every frame. Keyed by a
	// struct of (content hash, width, expandAll) — any of those
	// changing invalidates the entry. Messages are append-only after
	// they finalise so keeping the cache across turns is safe.
	//
	// Streaming/in-flight work (v.Streaming, v.ToolCalls) is never
	// cached because it changes every delta.
	renderCache map[msgCacheKey][]string
}

// msgCacheKey identifies a cached message render. hash is a 64-bit
// FNV-1a of the message's content, which is cheap to compute and
// unambiguous enough for the cache (collisions produce a stale frame,
// not wrong data, and we recompute on invalidation anyway).
type msgCacheKey struct {
	hash      uint64
	width     int
	expandAll bool
}

// InvalidateRenderCache drops all cached message renders. The tui
// calls this when the transcript is replaced wholesale (/compact,
// /clear, session swap) since messages can be replaced in place and
// a content-hash miss alone doesn't reclaim the old entries.
func (v *View) InvalidateRenderCache() {
	v.renderCache = nil
}

// ToolCollapsePreview is the number of lines shown before a long tool
// result is replaced with a "... ctrl+o to expand" footer. Tool
// results shorter than ToolCollapseLines always render in full.
const (
	ToolCollapsePreview = 10
	ToolCollapseLines   = 12
)

// ToolCallView is a pending tool invocation plus optional result.
type ToolCallView struct {
	ID     string
	Name   string
	Args   string // rendered argument summary
	Result string // rendered result preview (truncated)
	Error  bool
	Done   bool

	// Streaming is true while the model is still typing the tool
	// call's JSON arguments. The TUI renders a live preview of any
	// interesting string fields (for `write`, the `content`; for
	// `bash`, the `command`) so the user can watch the file being
	// composed. Set to false as soon as EvToolUseEnd arrives.
	Streaming bool

	// RawJSONBuf is the accumulator of every EvToolUseArgs delta
	// the stream has delivered for this tool call. Used by the
	// partial-JSON extractor to peel off the live string value
	// of one named field on each render.
	RawJSONBuf string

	// LivePath is the `path` arg extracted as soon as it parses
	// out of RawJSONBuf. Shown next to the tool name in the header
	// so the user can see which file is being written to.
	LivePath string
}

// MessageAnchor records where a rendered message starts in the chat
// line slice. Used by /jump so the dialog can scroll the viewport to
// the row where a turn's user prompt begins.
type MessageAnchor struct {
	MessageIdx int // index into v.Messages
	Row        int // first row of that message in the Build() output
}

// Build returns the chat log lines for the given width.
func (v *View) Build(width int) []string {
	lines, _ := v.BuildWithAnchors(width)
	return lines
}

// BuildWithAnchors is like Build but additionally reports the first
// row occupied by each message in v.Messages. Callers that need to
// scroll to a specific turn (the /jump dialog) use the anchor slice
// to map a message index back to a row offset.
func (v *View) BuildWithAnchors(width int) ([]string, []MessageAnchor) {
	v.refreshToolPaths()
	if v.renderCache == nil {
		v.renderCache = make(map[msgCacheKey][]string)
	}

	// Pre-render every message (hits the cache for unchanged ones) so
	// we can allocate `out` in a single shot with the exact capacity.
	// Growing via append on a long transcript copies the backing array
	// log2(N) times; for a 2000-line scrollback that's enough memcpy
	// to visibly stutter while typing.
	rendered := make([][]string, len(v.Messages))
	total := 0
	for idx, m := range v.Messages {
		lines := v.renderMessageCached(m, width)
		rendered[idx] = lines
		total += len(lines) + 1 // +1 for the blank separator row
	}

	out := make([]string, 0, total+16)
	anchors := make([]MessageAnchor, 0, len(v.Messages))
	for idx := range v.Messages {
		anchors = append(anchors, MessageAnchor{MessageIdx: idx, Row: len(out)})
		out = append(out, rendered[idx]...)
		out = append(out, "")
	}
	// Only render the streaming header/body when there's actual
	// text to show. An empty streaming block (streamOn=true,
	// Streaming="") appears when a turn starts with a tool_use
	// block instead of text — in that case the live tool-call
	// overlay below is the real content and a naked "zot" bar
	// above it reads as a stray empty message.
	if v.StreamingActive && strings.TrimSpace(v.Streaming) != "" {
		out = append(out, v.Theme.FG256(v.Theme.Assistant, "▍ zot"))
		// Stream the partial assistant text through the same markdown
		// renderer used for finalised messages so code fences, diffs,
		// lists, and inline styles look the same while streaming and
		// don't suddenly reflow when the turn ends. Indent matches the
		// finalised assistant body in renderMessage so the column
		// stays consistent across the stream/finalise transition.
		// Width is capped so ultra-wide terminals don't produce
		// edge-to-edge rules / unreadably long prose lines.
		const indent = "    "
		inner := assistantBodyWidth(width - len(indent))
		md := RenderMarkdown(v.Streaming, v.Theme, inner)
		for _, l := range strings.Split(md, "\n") {
			for _, w := range wrapLine(l, inner, "") {
				out = append(out, indent+w)
			}
		}
		out = append(out, "")
	}
	// Live tool-call overlay: skip any entry whose assistant
	// tool_use block OR tool_result has already made it into the
	// transcript. The EvAssistantMessage for a tool-use turn
	// lands BEFORE executeTools runs, so between that moment and
	// the tool-result being appended the overlay and the
	// finalised transcript both render the same call. Checking
	// for either side of the pair suppresses the duplicate in
	// both windows.
	finalised := map[string]bool{}
	for _, m := range v.Messages {
		for _, c := range m.Content {
			switch b := c.(type) {
			case provider.ToolCallBlock:
				finalised[b.ID] = true
			case provider.ToolResultBlock:
				finalised[b.CallID] = true
			}
		}
	}
	for _, tc := range v.ToolCalls {
		if finalised[tc.ID] {
			continue
		}
		out = append(out, v.renderToolCall(tc, width)...)
		out = append(out, "")
	}
	if v.Err != "" {
		out = append(out, v.Theme.FG256(v.Theme.Error, "✖ "+v.Err))
		out = append(out, "")
	}
	return out, anchors
}

// refreshToolPaths rebuilds the tool_use_id -> path map from the
// current transcript. Called once per Build() so tool result blocks
// (which may be cached) can look up their syntax language when they
// were originally rendered. Walking the transcript here is O(N) but
// cheap compared to markdown/chroma work it enables.
func (v *View) refreshToolPaths() {
	v.toolPaths = map[string]string{}
	v.toolStartLines = map[string]int{}
	for _, m := range v.Messages {
		for _, c := range m.Content {
			if tc, ok := c.(provider.ToolCallBlock); ok {
				if p := pathFromToolArgs(tc.Arguments); p != "" {
					v.toolPaths[tc.ID] = p
				}
				if off := offsetFromToolArgs(tc.Arguments); off >= 1 {
					v.toolStartLines[tc.ID] = off
				}
			}
		}
	}
}

// renderMessageCached returns the rendered line slice for m, using the
// cache if the same (content hash, width, expandAll) combination has
// been rendered before. The slice returned is shared — callers must
// not mutate it; Build() only ever appends to its own `out` so the
// shared slice is safe.
func (v *View) renderMessageCached(m provider.Message, width int) []string {
	key := msgCacheKey{
		hash:      hashMessage(m),
		width:     width,
		expandAll: v.ExpandAll,
	}
	if v.renderCache != nil {
		if lines, ok := v.renderCache[key]; ok {
			return lines
		}
	}
	lines := v.renderMessage(m, width)
	if v.renderCache != nil {
		// Bound the cache: 4x the current message count is enough to
		// survive /compact churn without leaking memory across a very
		// long session.
		max := len(v.Messages) * 4
		if max < 32 {
			max = 32
		}
		if len(v.renderCache) > max {
			// Drop half the entries. map iteration order gives us a
			// pseudo-LRU for free.
			dropped := 0
			target := len(v.renderCache) / 2
			for k := range v.renderCache {
				if dropped >= target {
					break
				}
				delete(v.renderCache, k)
				dropped++
			}
		}
		v.renderCache[key] = lines
	}
	return lines
}

// hashMessage returns a 64-bit FNV-1a over the role + content blocks
// of m. Serialising each block to its salient bytes is enough: two
// messages with the same role and same content render identically.
func hashMessage(m provider.Message) uint64 {
	h := fnv64aInit
	h = fnv64aWrite(h, []byte(m.Role))
	h = fnv64aWriteByte(h, 0)
	for _, c := range m.Content {
		switch b := c.(type) {
		case provider.TextBlock:
			h = fnv64aWriteByte(h, 't')
			h = fnv64aWrite(h, []byte(b.Text))
		case provider.ImageBlock:
			h = fnv64aWriteByte(h, 'i')
			h = fnv64aWrite(h, []byte(b.MimeType))
			h = fnv64aWrite(h, b.Data)
		case provider.ToolCallBlock:
			h = fnv64aWriteByte(h, 'c')
			h = fnv64aWrite(h, []byte(b.ID))
			h = fnv64aWrite(h, []byte(b.Name))
			h = fnv64aWrite(h, []byte(b.Arguments))
		case provider.ToolResultBlock:
			h = fnv64aWriteByte(h, 'r')
			h = fnv64aWrite(h, []byte(b.CallID))
			if b.IsError {
				h = fnv64aWriteByte(h, 'E')
			}
			for _, inner := range b.Content {
				switch ib := inner.(type) {
				case provider.TextBlock:
					h = fnv64aWrite(h, []byte(ib.Text))
				case provider.ImageBlock:
					h = fnv64aWrite(h, []byte(ib.MimeType))
					h = fnv64aWrite(h, ib.Data)
				}
			}
		}
		h = fnv64aWriteByte(h, 0)
	}
	return h
}

// FNV-1a implementation inlined so we don't pay the interface cost of
// hash.Hash64 on every Build(). The whole point here is speed.
const (
	fnv64aInit  uint64 = 0xcbf29ce484222325
	fnv64aPrime uint64 = 0x100000001b3
)

// assistantBodyRightPad is the blank gutter kept on the right
// side of every assistant prose line so text doesn't kiss the
// terminal edge. Matches the 4-cell left indent, so a line of
// fully-wrapped prose sits in a symmetric column.
const assistantBodyRightPad = 4

// assistantBodyWidth returns the usable width for the assistant
// message body (markdown prose + code fences). Uses the full
// column width passed in minus assistantBodyRightPad, clamped
// at 1 so wrap helpers don't divide by zero on absurdly narrow
// terminals. The right-side padding keeps a small breathing
// column to the terminal edge that mirrors the left indent.
func assistantBodyWidth(outer int) int {
	outer -= assistantBodyRightPad
	if outer < 1 {
		return 1
	}
	return outer
}

func fnv64aWriteByte(h uint64, b byte) uint64 {
	h ^= uint64(b)
	h *= fnv64aPrime
	return h
}

func fnv64aWrite(h uint64, p []byte) uint64 {
	for _, b := range p {
		h ^= uint64(b)
		h *= fnv64aPrime
	}
	return h
}

func (v *View) renderMessage(m provider.Message, width int) []string {
	var lines []string
	switch m.Role {
	case provider.RoleUser:
		header := v.Theme.FG256(v.Theme.User, "▍ you")
		lines = append(lines, header)
		for _, c := range m.Content {
			switch b := c.(type) {
			case provider.TextBlock:
				for _, l := range strings.Split(b.Text, "\n") {
					for _, w := range wrapLine(l, width-2, "    ") {
						lines = append(lines, "    "+v.Theme.FG256(v.Theme.Muted, w))
					}
				}
			case provider.ImageBlock:
				lines = append(lines, "    "+v.Theme.FG256(v.Theme.Muted, fmt.Sprintf("[image %s, %d bytes]", b.MimeType, len(b.Data))))
			}
		}
	case provider.RoleAssistant:
		header := v.Theme.FG256(v.Theme.Assistant, "▍ zot")
		lines = append(lines, header)
		// Indent assistant body the same 4 cells the user body uses,
		// so the conversation column lines up vertically. The width
		// passed into the markdown renderer / wrap is reduced by the
		// indent so long lines wrap inside the indented column, and
		// capped so ultra-wide terminals don't produce edge-to-edge
		// code-fence rules or unreadably long prose lines.
		const indent = "    "
		inner := assistantBodyWidth(width - len(indent))
		for _, c := range m.Content {
			switch b := c.(type) {
			case provider.TextBlock:
				md := RenderMarkdown(b.Text, v.Theme, inner)
				for _, l := range strings.Split(md, "\n") {
					for _, w := range wrapLine(l, inner, "") {
						lines = append(lines, indent+w)
					}
				}
			case provider.ToolCallBlock:
				// Rule above the tool header frames the call as a
				// self-contained block separating it from the
				// assistant prose above. The matching closing rule
				// is emitted at the end of the tool-role message.
				lines = append(lines, toolBlockRule(v.Theme, width))
				lines = append(lines, indent+v.Theme.FG256(v.Theme.Tool, "▸ "+b.Name+" "+ShortArgs(b.Name, b.Arguments)))
			}
		}
	case provider.RoleTool:
		for _, c := range m.Content {
			if tr, ok := c.(provider.ToolResultBlock); ok {
				color := v.Theme.ToolOut
				if tr.IsError {
					color = v.Theme.Error
				}
				path := ""
				if v.toolPaths != nil {
					path = v.toolPaths[tr.CallID]
				}
				startLine := 1
				if v.toolStartLines != nil {
					if s := v.toolStartLines[tr.CallID]; s > 0 {
						startLine = s
					}
				}
				// Render the body directly under the tool-call
				// header (no "result" sub-header). Errors keep a
				// one-line header so they're distinguishable from
				// successful output. A closing rule below the body
				// pairs with the opening rule emitted above the
				// tool-call header in the assistant message,
				// framing the whole tool block.
				if tr.IsError {
					lines = append(lines, v.Theme.FG256(color, "  error"))
				}
				lines = append(lines, v.renderToolResultContent(tr.Content, width, color, path, startLine)...)
				lines = append(lines, toolBlockRule(v.Theme, width))
			}
		}
	}
	return lines
}

func (v *View) renderToolCall(tc ToolCallView, width int) []string {
	var lines []string

	// Header. While the call is still streaming, prefer the live path
	// extracted from the partial args so the user sees the target
	// file as soon as it's known, even before the full JSON arrived.
	arg := tc.Args
	if arg == "" && tc.LivePath != "" {
		arg = tc.LivePath
	}
	head := v.Theme.FG256(v.Theme.Tool, "▸ "+tc.Name+" "+arg)

	// Live streaming body: pulled out of the partial JSON buffer for
	// tools whose interesting content is a string field (currently
	// write's `content` and edit's `new_text` chunks). The body is
	// already framed by wrapLiveBody with top+bottom rules, so we
	// don't add the extra toolBlockRule around it — that would
	// produce four rules per streaming block, with a visible doubled
	// line at the bottom.
	if tc.Streaming && tc.Result == "" {
		lines = append(lines, head)
		if body := v.renderLiveToolBody(tc, width); len(body) > 0 {
			lines = append(lines, body...)
		}
		return lines
	}

	// Finished tool call: frame the whole block with opening +
	// closing rules so it stands apart from surrounding assistant
	// prose. Matches the transcript-side framing in renderMessage.
	lines = append(lines, toolBlockRule(v.Theme, width))
	lines = append(lines, head)
	if tc.Result != "" {
		color := v.Theme.ToolOut
		if tc.Error {
			color = v.Theme.Error
		}
		body := toolResultBlock(v.Theme, tc.Result, width, color)
		lines = append(lines, v.collapseToolBody(body, false)...)
	}
	lines = append(lines, toolBlockRule(v.Theme, width))
	return lines
}

// renderLiveToolBody renders the in-flight preview of a streaming
// tool call. Supported tools:
//
//   - write: shows the partial `content` field, syntax-highlighted
//     by the target path's language.
//   - edit:  shows the partial `newText` of the edit currently being
//     streamed, prefixed with a "editing foo.ts (edit 2)" header so
//     the user can see which of a multi-edit batch is in progress.
//
// Anything else returns nil and only the tool-call header shows.
func (v *View) renderLiveToolBody(tc ToolCallView, width int) []string {
	switch tc.Name {
	case "write", "Write":
		partial, ok, _ := ExtractPartialStringField(tc.RawJSONBuf, "content")
		if !ok || partial == "" {
			return nil
		}
		return v.wrapLiveBody(v.renderRawFile(partial, tc.LivePath, 1), width)
	case "edit", "Edit":
		partial, ok, _, idx := ExtractLastNewText(tc.RawJSONBuf)
		if !ok || partial == "" {
			return nil
		}
		// Header line hints which edit is streaming and, when more
		// than one has landed, how many the model is doing.
		hint := fmt.Sprintf("  edit %d (streaming)", idx)
		body := []string{v.Theme.FG256(v.Theme.Muted, hint), ""}
		body = append(body, v.renderRawFile(partial, tc.LivePath, 1)...)
		return v.wrapLiveBody(body, width)
	}
	return nil
}

// wrapLiveBody wraps a list of content lines with the standard
// wrapLiveBody wraps a list of content lines with the standard
// tool-result rules (top + bottom), collapsing to the preview
// height if the body is tall. Shared between write and edit
// streaming.
func (v *View) wrapLiveBody(body []string, width int) []string {
	body = v.collapseToolBody(body, false)
	rule := v.Theme.FG256(v.Theme.Muted, strings.Repeat("─", width))
	out := make([]string, 0, len(body)+2)
	out = append(out, rule)
	out = append(out, body...)
	out = append(out, rule)
	return out
}

// toolResultBlock wraps text in thin horizontal rules (top + bottom),
// indenting the body with four spaces. The rules span the content column.
// toolBlockRule renders the muted horizontal separator drawn
// above and below a tool call block. Spans the full content
// width so it reads as a real section break in the chat
// regardless of terminal size.
func toolBlockRule(th Theme, width int) string {
	w := width
	if w < 8 {
		w = 8
	}
	return th.FG256(th.Muted, strings.Repeat("─", w))
}

// renderToolResultContent renders the body of a tool result block.
// Text blocks get the usual rules-wrapped treatment; text that looks
// like a unified diff gets +/- coloring. Image blocks are rendered
// inline when the terminal supports a protocol, else as a text
// placeholder with dimensions.
func (v *View) renderToolResultContent(blocks []provider.Content, width, color int, sourcePath string, startLine int) []string {
	var body []string
	hasImage := false
	for _, b := range blocks {
		switch bb := b.(type) {
		case provider.TextBlock:
			body = append(body, v.renderToolText(bb.Text, width, color, sourcePath, startLine)...)
		case provider.ImageBlock:
			hasImage = true
			body = append(body, v.renderImageBlock(bb, width)...)
		}
	}
	return v.collapseToolBody(body, hasImage)
}

// collapseToolBody trims lines to the configured preview size when the
// view is not in ExpandAll mode, appending a muted "... ctrl+o to
// expand" footer. Image blocks never collapse — they're short in text
// rows but represent real content the user wants to see.
func (v *View) collapseToolBody(lines []string, hasImage bool) []string {
	if v.ExpandAll || hasImage {
		return lines
	}
	if len(lines) <= ToolCollapseLines {
		return lines
	}
	kept := lines[:ToolCollapsePreview]
	hidden := len(lines) - ToolCollapsePreview
	total := len(lines)
	footer := fmt.Sprintf("    ... (%d more lines, %d total, ctrl+o to expand)", hidden, total)
	footer = v.Theme.FG256(v.Theme.Muted, footer)
	return append(append([]string(nil), kept...), footer)
}

// renderToolText renders a text block inside a tool result. If the
// text contains a unified-diff section (lines starting with "--- " /
// "+++ " / "+" / "-"/" "), those rows are styled with add/remove
// colors matching git diff conventions.
func (v *View) renderToolText(text string, width, defaultColor int, sourcePath string, startLine int) []string {
	// Legacy path: transcripts saved before we dropped line numbers
	// from the read tool still carry "     1\t…" prefixes. Detect and
	// strip them, then fall through to the highlighter.
	if looksLikeNumberedFile(text) {
		return v.renderNumberedFile(text, sourcePath)
	}
	// If the result embeds a unified diff (the edit tool's output
	// starts with a short "applied N edit(s)" line and then a
	// standard --- / +++ / +/- patch), render the patch with
	// add/remove coloring. This takes priority over the file-like
	// detector below because a diff technically has many lines of
	// "file content" but what the user cares about is what changed,
	// not a dump of the post-edit file.
	if looksLikeUnifiedDiff(text) {
		return v.renderUnifiedDiff(text, width, sourcePath)
	}

	// Current path: text came from `read` as raw file bytes. When a
	// source path is known (the call had a `path` arg), render with
	// a synthetic line-number gutter starting at startLine so the
	// on-screen view still looks like cat -n. Doesn't apply to non-
	// file tool outputs (bash stdout, display notes, etc.).
	if sourcePath != "" && looksLikeFileContent(text) {
		return v.renderRawFile(text, sourcePath, startLine)
	}

	// No truncation — the full tool output is rendered into chat and
	// becomes part of the scrollback you can page back through.
	lines := strings.Split(text, "\n")

	// Bash-result styling: when the first row looks like a shell
	// prompt line ("$ ...") emitted by the bash tool, style the
	// prompt line in accent and the trailing "[exit N]  Took X.Ys"
	// line in muted type so the command + timing read at a glance.
	// Everything between is left on the default tool-output color.
	if len(lines) > 0 && strings.HasPrefix(lines[0], "$ ") {
		return v.renderBashResult(lines, width, defaultColor)
	}

	inDiff := false
	oldLine, newLine := 1, 1
	var out []string
	for _, l := range lines {
		// Detect diff header: "--- name" followed somewhere by "+++ name".
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
			inDiff = true
			out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, l))
			continue
		}
		// Hunk header "@@ -a,b +c,d @@" resets the counters so patches
		// that skip around in the file still get correct numbering.
		if inDiff && strings.HasPrefix(l, "@@") {
			if o, n, ok := parseHunkHeader(l); ok {
				oldLine, newLine = o, n
			}
			out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, l))
			continue
		}
		if inDiff && len(l) > 0 {
			switch l[0] {
			case '+':
				out = append(out, v.renderDiffRow(l, width, v.Theme.Tool, newLine, '+', sourcePath))
				newLine++
				continue
			case '-':
				out = append(out, v.renderDiffRow(l, width, v.Theme.Error, oldLine, '-', sourcePath))
				oldLine++
				continue
			case ' ':
				out = append(out, v.renderDiffRow(l, width, v.Theme.Muted, newLine, ' ', sourcePath))
				oldLine++
				newLine++
				continue
			}
		}
		// Regular line.
		for _, w := range wrapLine(l, width-4, "    ") {
			out = append(out, "    "+v.Theme.FG256(defaultColor, w))
		}
	}
	return out
}

// parseHunkHeader extracts the starting old/new line from a unified
// diff hunk header ("@@ -12,5 +12,7 @@ ..."). Returns ok=false if the
// header is malformed or missing numbers.
func parseHunkHeader(l string) (oldStart, newStart int, ok bool) {
	// Skip "@@ "
	rest := strings.TrimPrefix(l, "@@")
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "-") {
		return 0, 0, false
	}
	rest = rest[1:]
	space := strings.IndexByte(rest, ' ')
	if space < 0 {
		return 0, 0, false
	}
	oldPart := rest[:space]
	rest = strings.TrimSpace(rest[space+1:])
	if !strings.HasPrefix(rest, "+") {
		return 0, 0, false
	}
	rest = rest[1:]
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		rest = rest[:sp]
	}
	parseStart := func(s string) (int, bool) {
		if c := strings.IndexByte(s, ','); c >= 0 {
			s = s[:c]
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return 0, false
		}
		return n, true
	}
	o, ok1 := parseStart(oldPart)
	n, ok2 := parseStart(rest)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return o, n, true
}

// renderDiffRow renders one unified-diff line with a read-style gutter
// (6-cell right-aligned line number, muted) followed by the +/-/space
// marker and the code. Code is syntax-highlighted if sourcePath hints
// at a known language; falls back to the plain diff color otherwise.
func (v *View) renderDiffRow(line string, width, color int, lineNo int, mark byte, sourcePath string) string {
	if len(line) == 0 {
		return ""
	}
	code := line[1:] // strip the leading marker; we re-emit it in colour

	// Syntax-highlight the code half when we know the language. Use
	// the same HighlightCode pipeline as renderNumberedFile so the
	// palette matches.
	lang := LanguageFromPath(sourcePath)
	var codeRendered string
	if lang != "" {
		if h := HighlightCode(code, lang); len(h) == 1 {
			codeRendered = h[0]
		}
	}
	if codeRendered == "" {
		if mark == ' ' {
			codeRendered = v.Theme.FG256(v.Theme.Muted, code)
		} else {
			codeRendered = v.Theme.FG256(color, code)
		}
	}

	// Gutter shape: sign + number share a color so they read as one
	// visual token ("+123") instead of a neutral line number next to
	// a stray marker. Unchanged context lines get a muted gutter and
	// a leading space so column alignment stays consistent with +/-
	// rows.
	var gutterText string
	switch mark {
	case '+':
		gutterText = fmt.Sprintf("+%5d\t", lineNo)
	case '-':
		gutterText = fmt.Sprintf("-%5d\t", lineNo)
	default:
		gutterText = fmt.Sprintf(" %5d\t", lineNo)
	}
	var gutter string
	if mark == ' ' {
		gutter = v.Theme.FG256(v.Theme.Muted, gutterText)
	} else {
		gutter = v.Theme.FG256(color, gutterText)
	}
	row := "    " + gutter + codeRendered

	// Cheap width clamp: truncate visible text if the raw code is too
	// long. We work on the pre-ANSI code string because measuring ansi
	// output is unreliable.
	maxCode := width - 4 /* indent */ - 7 /* gutter (sign+5 digits+tab) */
	if maxCode > 0 && len(code) > maxCode {
		trunc := code[:maxCode-1] + "…"
		if lang != "" {
			if h := HighlightCode(trunc, lang); len(h) == 1 {
				codeRendered = h[0]
			} else if mark == ' ' {
				codeRendered = v.Theme.FG256(v.Theme.Muted, trunc)
			} else {
				codeRendered = v.Theme.FG256(color, trunc)
			}
		} else if mark == ' ' {
			codeRendered = v.Theme.FG256(v.Theme.Muted, trunc)
		} else {
			codeRendered = v.Theme.FG256(color, trunc)
		}
		row = "    " + gutter + codeRendered
	}
	return row
}

// renderImageBlock returns the lines for one image, inline if possible.
//
// Inline image escapes paint into multiple terminal rows but the zot
// renderer treats each slice entry as a single row. To prevent chat
// content from being drawn on top of the image, we pad with blank rows
// so the image's real footprint is reflected in the frame height.
func (v *View) renderImageBlock(b provider.ImageBlock, width int) []string {
	w, h := ImageDimensions(b.Data)
	kb := len(b.Data) / 1024
	info := fmt.Sprintf("  image · %s · %dx%d · %d KB", b.MimeType, w, h, kb)

	if v.ImageProto != ImageProtocolNone {
		// Clamp rendered width so the image never overflows the chat
		// column. Subtract a 4-cell indent for the tool result block,
		// cap at 60 cells, floor at 10.
		cells := width - 4
		if cells > 60 {
			cells = 60
		}
		if cells < 10 {
			cells = 10
		}
		const maxRows = 20
		if seq := RenderInlineImageScaled(v.ImageProto, b.Data, b.MimeType, cells, maxRows); seq != "" {
			rows := RowsForInlineImage(b.Data, cells, maxRows)
			if rows < 1 {
				rows = 1
			}
			out := make([]string, 0, rows+1)
			out = append(out, "    "+seq)
			// Reserve blank rows matching the image's on-screen height.
			for i := 1; i < rows; i++ {
				out = append(out, "")
			}
			out = append(out, v.Theme.FG256(v.Theme.Muted, info))
			return out
		}
	}
	// Text fallback.
	return []string{v.Theme.FG256(v.Theme.Muted, info)}
}

// looksLikeNumberedFile returns true when text matches the `read`
// tool's "     N\tcontent" format for most of its lines.
func looksLikeNumberedFile(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return false
	}
	hits := 0
	scanned := 0
	for _, l := range lines {
		if l == "" {
			continue
		}
		scanned++
		if scanned > 20 {
			break
		}
		if numberedLineRE.MatchString(l) {
			hits++
		}
	}
	if scanned == 0 {
		return false
	}
	return hits*2 >= scanned // majority of non-empty lines match
}

var numberedLineRE = regexp.MustCompile(`^\s*\d+\t`)

// renderNumberedFile strips line numbers, highlights the code, and
// re-attaches the line numbers in muted color.
func (v *View) renderNumberedFile(text, sourcePath string) []string {
	lines := strings.Split(text, "\n")
	gutters := make([]string, 0, len(lines))
	codes := make([]string, 0, len(lines))
	for _, l := range lines {
		idx := strings.IndexByte(l, '\t')
		if idx < 0 || !numberedLineRE.MatchString(l) {
			// Non-code footer (e.g. "[truncated at 2000 lines]").
			gutters = append(gutters, "")
			codes = append(codes, l)
			continue
		}
		gutter := l[:idx+1] // keep the tab so alignment is preserved
		code := l[idx+1:]
		gutters = append(gutters, gutter)
		codes = append(codes, code)
	}
	lang := LanguageFromPath(sourcePath)
	var highlighted []string
	if lang != "" {
		highlighted = HighlightCode(strings.Join(codes, "\n"), lang)
		// Chroma sometimes collapses the trailing empty line. Pad to
		// align with the gutter slice so per-line zipping works.
		for len(highlighted) < len(codes) {
			highlighted = append(highlighted, "")
		}
		if len(highlighted) > len(codes) {
			highlighted = highlighted[:len(codes)]
		}
	} else {
		// No lexer for this file type — render in the ToolOut color so
		// code is visually distinct from the muted gutter.
		highlighted = make([]string, len(codes))
		for i, c := range codes {
			highlighted[i] = v.Theme.FG256(v.Theme.ToolOut, c)
		}
	}
	out := make([]string, 0, len(codes))
	for i, code := range highlighted {
		g := gutters[i]
		if g == "" {
			out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, code))
			continue
		}
		out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, g)+code)
	}
	return out
}

// looksLikeFileContent is a cheap guard to distinguish a read-tool
// result from bash stdout or a status message. File content usually
// contains characters that status messages don't (code punctuation,
// longer lines, multiple lines) and rarely starts with the "  >"-
// or "error:"-style prefixes tools emit. False positives are OK,
// the worst case is a line-number gutter on something that isn't
// really code.
// renderBashResult styles a bash tool result: the "$ command" first
// line in the accent color, the trailing "[exit N]  Took X.Ys" line
// in muted type, everything else on the default tool-output color.
// Called from renderToolText when the first line starts with "$ ".
func (v *View) renderBashResult(lines []string, width, defaultColor int) []string {
	// Identify the footer line (exit + timing). The bash tool writes
	// it as the last non-empty line of the result.
	footerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "[exit ") {
			footerIdx = i
		}
		break
	}

	var out []string
	for i, l := range lines {
		switch {
		case i == 0 && strings.HasPrefix(l, "$ "):
			// Style the $ and the command text in accent. No
			// further processing / wrapping so the shell-style
			// prompt reads at a glance.
			for _, w := range wrapLine(l, width-4, "    ") {
				out = append(out, "    "+v.Theme.FG256(v.Theme.Accent, w))
			}
		case i == footerIdx:
			out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, l))
		default:
			for _, w := range wrapLine(l, width-4, "    ") {
				out = append(out, "    "+v.Theme.FG256(defaultColor, w))
			}
		}
	}
	return out
}

// looksLikeUnifiedDiff reports whether text is a context diff as
// emitted by the edit tool: rows start with '+', '-', ' ', or
// literal "..." (context-break marker). The presence of at least
// one '+' or '-' row distinguishes a real diff from an ordinary
// file whose lines happen to begin with a space.
func looksLikeUnifiedDiff(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return false
	}
	sawChange := false
	for _, l := range lines {
		if l == "" {
			continue
		}
		if l == "..." {
			continue
		}
		switch l[0] {
		case '+', '-':
			sawChange = true
		case ' ':
			// context, ok
		default:
			return false
		}
	}
	return sawChange
}

// renderUnifiedDiff renders the edit tool's context diff. Each
// kept row shows a line-number gutter plus a marker column: '+'
// for additions (colored like add), '-' for deletions (colored
// like remove), and unmarked context in muted type. A literal
// "..." line between hunks renders as an ellipsis in muted type,
// indicating skipped unchanged rows. The old and new line
// counters advance so each row carries its actual position in
// the pre- or post-edit file. Heuristic: when we hit a "...", we
// can't know where the next hunk starts, so we don't reset the
// counters — they stay approximate in the rare multi-hunk case.
func (v *View) renderUnifiedDiff(text string, width int, sourcePath string) []string {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	oldLine, newLine := 1, 1
	var out []string
	for _, l := range lines {
		if l == "" {
			out = append(out, "")
			continue
		}
		if l == "..." {
			out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, "…"))
			continue
		}
		switch l[0] {
		case '+':
			out = append(out, v.renderDiffRow(l, width, v.Theme.Tool, newLine, '+', sourcePath))
			newLine++
		case '-':
			out = append(out, v.renderDiffRow(l, width, v.Theme.Error, oldLine, '-', sourcePath))
			oldLine++
		case ' ':
			out = append(out, v.renderDiffRow(l, width, v.Theme.Muted, newLine, ' ', sourcePath))
			oldLine++
			newLine++
		default:
			for _, w := range wrapLine(l, width-4, "    ") {
				out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, w))
			}
		}
	}
	return out
}

func looksLikeFileContent(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lines := strings.Split(text, "\n")
	return len(lines) >= 2
}

// renderRawFile renders file content received without embedded line
// numbers (the current read-tool output). Draws a muted gutter like
// "%6d \t" starting at startLine, highlights the code using the
// source path's language, and returns the formatted lines.
func (v *View) renderRawFile(text, sourcePath string, startLine int) []string {
	lines := strings.Split(text, "\n")
	// Drop the trailing empty line that Split produces when text ends
	// in "\n" so the gutter doesn't show a phantom last number.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	// Split code from trailing footer lines ("... [truncated ...]")
	// so we don't number the footer.
	codeEnd := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "...") {
			codeEnd = i
			continue
		}
		break
	}
	code := lines[:codeEnd]
	footer := lines[codeEnd:]

	lang := LanguageFromPath(sourcePath)
	var highlighted []string
	if lang != "" {
		highlighted = HighlightCode(strings.Join(code, "\n"), lang)
		for len(highlighted) < len(code) {
			highlighted = append(highlighted, "")
		}
		if len(highlighted) > len(code) {
			highlighted = highlighted[:len(code)]
		}
	} else {
		highlighted = make([]string, len(code))
		for i, c := range code {
			highlighted[i] = v.Theme.FG256(v.Theme.ToolOut, c)
		}
	}

	out := make([]string, 0, len(lines))
	for i, c := range highlighted {
		gutter := fmt.Sprintf("%6d\t", startLine+i)
		out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, gutter)+c)
	}
	for _, f := range footer {
		out = append(out, "    "+v.Theme.FG256(v.Theme.Muted, f))
	}
	return out
}

// toolResultBlock renders the live tool-call result body (shown
// while the turn is still in flight). The rules that used to
// bracket this block have been dropped so the live path looks
// identical to the transcript rendering that replaces it when
// the turn ends.
func toolResultBlock(th Theme, text string, width int, color int) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		for _, w := range wrapLine(l, width-4, "    ") {
			out = append(out, "    "+th.FG256(color, w))
		}
	}
	return out
}

// ShortArgs renders a tool call's arguments into a one-line
// suffix for the "tool name <args>" header. tool is the tool
// name so we can add shape-specific decorations: for read we
// append the requested line range (e.g. "path:1-200") pulled
// from the offset/limit args, which is useful context at a
// glance without expanding the result body. Other tools keep
// the legacy "path or command, truncated at 60 cells" shape.
//
// Exported because the interactive mode pre-populates the
// ToolCallView.Args field with this value as soon as the tool
// call is announced, so the live overlay's header matches what
// the finalised transcript will later render.
func ShortArgs(tool string, raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	x, ok := v.(map[string]any)
	if !ok {
		b, _ := json.Marshal(v)
		s := string(b)
		if len(s) > 60 {
			s = s[:57] + "..."
		}
		return s
	}
	var primary string
	for _, k := range []string{"path", "file_path", "command"} {
		if s, ok := x[k].(string); ok {
			primary = s
			break
		}
	}
	if primary == "" {
		b, _ := json.Marshal(v)
		s := string(b)
		if len(s) > 60 {
			s = s[:57] + "..."
		}
		return s
	}

	// Tool-specific decoration. Only the read tool gets a range
	// suffix for now; other tools just truncate the primary arg.
	suffix := ""
	switch strings.ToLower(tool) {
	case "read":
		start := 1
		if n, ok := toInt(x["offset"]); ok && n >= 1 {
			start = n
		}
		if lim, ok := toInt(x["limit"]); ok && lim > 0 {
			end := start + lim - 1
			suffix = fmt.Sprintf(":%d-%d", start, end)
		} else if start > 1 {
			suffix = fmt.Sprintf(":%d-", start)
		}
	}

	// Truncate the primary arg leaving room for the suffix so the
	// range stays visible even on absurdly long paths.
	max := 60 - len(suffix)
	if max < 10 {
		max = 10
	}
	if len(primary) > max {
		primary = primary[:max-3] + "..."
	}
	return primary + suffix
}

// toInt coerces a json.Unmarshal'd number (float64) or a string
// containing a number into an int. Returns ok=false if the value
// is neither. Used by shortArgs to survive model quirks where
// numeric args come back as strings.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

func collectText(blocks []provider.Content) string {
	var sb strings.Builder
	for _, b := range blocks {
		if tb, ok := b.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func truncateLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n  … (" + fmt.Sprintf("%d", len(lines)-n) + " more)"
}

// StatusBarParams groups the many bits of state the status bar needs.
// Grew from a flat argument list once we settled on the layout.
type StatusBarParams struct {
	Theme      Theme
	Provider   string
	Model      string
	Busy       bool
	BusyPrefix string // spinner + funny line when busy
	CWD        string
	Locked     bool // sandbox on?

	// Cumulative session usage and cost.
	Usage provider.Usage
	// Subscription is true when the credential is an OAuth token (claude
	// pro/max, chatgpt plus/pro) rather than a paid api key. We still
	// compute a cost for visibility and append "(sub)" so the user
	// knows no real money moved.
	Subscription bool

	// Last turn's input+cache tokens (approximates current live context).
	ContextUsed int
	ContextMax  int // model's context window; 0 disables the percentage

	// AutoCompacting is true when the agent is currently running a
	// model-triggered condense pass. Surfaces as "(auto)" after the
	// context percentage so it's clear where the spinner is coming from.
	AutoCompacting bool

	// Telegram true when the telegram bridge is connected. Adds a
	// small "· tg ·" tag to the cwd line so the user can tell at a
	// glance that dms are being mirrored into this session.
	Telegram bool

	Cols int // terminal width; drives right-alignment of cwd
}

// StatusBar builds the status shown above the editor. Always returns
// two lines when a cwd is provided: the stats on the first line, the
// cwd on its own line below, indented to match the stats column. This
// keeps the status bar stable across terminal resizes (the cwd never
// jumps from right-aligned-on-line-1 to flush-left-on-line-2) and
// makes a long cwd safe at any width.
//
// Layout:
//
//	<busyPrefix>  (provider) model  stats   <- line 1
//	  cwd                                   <- line 2 (2-space indent)
//
// The old "ctrl+c exit - /help" / "esc cancel" hint is gone entirely.
// The slash-command popup and the queued/sliding-in chips already
// cover the discoverability of those keybindings.
func StatusBar(p StatusBarParams) []string {
	th := p.Theme

	// Token stats: only include each segment when non-zero. Keeps
	// the bar compact on brand-new sessions.
	var stats []string
	if p.Usage.InputTokens > 0 {
		stats = append(stats, fmt.Sprintf("↑%s", formatTokens(p.Usage.InputTokens)))
	}
	if p.Usage.OutputTokens > 0 {
		stats = append(stats, fmt.Sprintf("↓%s", formatTokens(p.Usage.OutputTokens)))
	}
	if p.Usage.CacheReadTokens > 0 {
		stats = append(stats, fmt.Sprintf("R%s", formatTokens(p.Usage.CacheReadTokens)))
	}
	if p.Usage.CacheWriteTokens > 0 {
		stats = append(stats, fmt.Sprintf("W%s", formatTokens(p.Usage.CacheWriteTokens)))
	}

	// Cost: always show the dollar value computed from token counts,
	// even on subscription. Lets you see what the equivalent api cost
	// would be (handy for gauging subscription value). Append "(sub)"
	// only as a hint that no real money moved.
	var costStr string
	if p.Usage.CostUSD > 0 || p.Subscription {
		costStr = fmt.Sprintf("$%.3f", p.Usage.CostUSD)
		if p.Subscription {
			costStr += " (sub)"
		}
	}
	if costStr != "" {
		stats = append(stats, costStr)
	}

	// Context %. Color-coded: yellow >70, red >90.
	ctx, ctxColor := contextUsage(th, p.ContextUsed, p.ContextMax)
	if ctx != "" {
		if p.AutoCompacting {
			ctx += " (auto)"
		}
		stats = append(stats, th.FG256(ctxColor, ctx))
	}

	// Layout uses exactly 2 spaces of horizontal padding everywhere:
	//   2 spaces  (openai) gpt-5.4  $0.000 (sub) 0.0%/400k  ~/Sites/zot
	// matches the editor prompt's left inset so the bar lines up
	// vertically with the conversation column.
	const pad = "  " // 2 spaces

	left := fmt.Sprintf("(%s) %s", p.Provider, p.Model)
	middle := strings.Join(stats, " ")

	var leftBuilder strings.Builder
	if p.BusyPrefix != "" {
		// Don't re-color the payload itself — the caller already set
		// fg colors on the spinner glyph, message, and elapsed
		// segments. Wrapping the whole thing here would override
		// those choices. The pad itself needs no color (it's spaces).
		leftBuilder.WriteString(pad + p.BusyPrefix)
		leftBuilder.WriteString(pad)
	}
	leftBuilder.WriteString(pad)
	leftBuilder.WriteString(th.FG256(th.Muted, left))
	if middle != "" {
		leftBuilder.WriteString(pad)
		// `middle` already has colorized context segments; wrap the rest in muted.
		leftBuilder.WriteString(th.FG256(th.Muted, middle))
	}

	cwd := shortenHome(p.CWD)
	tags := ""
	if p.Locked {
		tags += "· jailed "
	}
	if p.Telegram {
		tags += "· tg "
	}
	if tags != "" && cwd != "" {
		cwd = tags + "· " + cwd
	}

	primary := leftBuilder.String()
	if cwd == "" {
		return []string{primary}
	}

	// Second line: indent with the same 2-space pad so the cwd lines
	// up under the "(provider)" column on line 1.
	cwdRendered := pad + th.FG256(th.Muted, cwd)
	return []string{primary, cwdRendered}
}

// contextUsage renders the "N%/ctxMax" fragment, returning the
// rendered string plus the colour to wrap it in.
func contextUsage(th Theme, used, max int) (string, int) {
	if max <= 0 {
		if used <= 0 {
			return "", th.Muted
		}
		return formatTokens(used), th.Muted
	}
	pct := float64(used) / float64(max) * 100
	text := fmt.Sprintf("%.1f%%/%s", pct, formatTokens(max))
	switch {
	case pct > 90:
		return text, th.Error
	case pct > 70:
		return text, th.Warning
	}
	return text, th.Muted
}

// formatTokens footer formatter:
//
//	< 1000      -> "42"
//	< 10_000    -> "2.7k"
//	< 1_000_000 -> "35k"
//	< 10M       -> "1.1M"
//	else        -> "12M"
func formatTokens(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%dM", (n+500_000)/1_000_000)
	}
}

// shortenHome replaces the user's $HOME prefix with "~" for readability.
func shortenHome(p string) string {
	if p == "" {
		return ""
	}
	home := osUserHome()
	if home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// osUserHome is a tiny wrapper around os.UserHomeDir so tests can mock it.
var osUserHome = func() string {
	if h, err := osUserHomeDir(); err == nil {
		return h
	}
	return ""
}
