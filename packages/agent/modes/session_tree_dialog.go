package modes

import (
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

// sessionTreeDialog renders a compact outline of the current session family.
// Branches are shown inline at the point where they forked, using indentation
// rather than file-level rows. Selecting a node checks out that point into a
// new branch.
type sessionTreeDialog struct {
	active bool
	items  []sessionTreeItem
	cursor int
}

type sessionTreeItem struct {
	label      string
	messageIdx int
	turnNo     int
	role       provider.Role
	prompt     string
	path       string
	depth      int
	current    bool
}

type sessionTreeAction struct {
	Select     bool
	MessageIdx int
	TurnNo     int
	Role       provider.Role
	Prompt     string
	Path       string
	Close      bool
}

func newSessionTreeDialog() *sessionTreeDialog { return &sessionTreeDialog{} }

func (d *sessionTreeDialog) OpenMessages(msgs []provider.Message) bool {
	d.items = buildSessionTreeItems("", msgs, 0, false)
	d.cursor = len(d.items) - 1
	if d.cursor < 0 {
		d.cursor = 0
	}
	d.active = true
	return len(d.items) > 0
}

func (d *sessionTreeDialog) OpenSessionFamily(root, cwd, currentPath string) bool {
	roots := core.BuildSessionTree(root, cwd)
	if len(roots) == 0 {
		return false
	}
	rootNode := findTreeRootForPath(roots, currentPath)
	if rootNode == nil {
		rootNode = roots[0]
	}
	d.items = flattenSessionFamily(rootNode, currentPath)
	d.cursor = indexCurrentTreeItem(d.items)
	d.active = true
	return len(d.items) > 0
}

// Close hides the dialog.
func (d *sessionTreeDialog) Close() { d.active = false }

// CursorPos returns -1 because this dialog has no inline editor.
func (d *sessionTreeDialog) CursorPos() (row, col int) { return -1, -1 }

// Active reports whether the dialog consumes input.
func (d *sessionTreeDialog) Active() bool { return d != nil && d.active }

// HandleKey advances the cursor or resolves the selection.
func (d *sessionTreeDialog) HandleKey(k tui.Key) sessionTreeAction {
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.items)-1 {
			d.cursor++
		}
	case tui.KeyPageUp:
		d.cursor -= 5
		if d.cursor < 0 {
			d.cursor = 0
		}
	case tui.KeyPageDown:
		d.cursor += 5
		if d.cursor >= len(d.items) {
			d.cursor = len(d.items) - 1
		}
		if d.cursor < 0 {
			d.cursor = 0
		}
	case tui.KeyEsc:
		d.Close()
		return sessionTreeAction{Close: true}
	case tui.KeyEnter:
		if len(d.items) == 0 || d.cursor < 0 || d.cursor >= len(d.items) {
			d.Close()
			return sessionTreeAction{Close: true}
		}
		it := d.items[d.cursor]
		d.Close()
		return sessionTreeAction{Select: true, MessageIdx: it.messageIdx, TurnNo: it.turnNo, Role: it.role, Prompt: it.prompt, Path: it.path}
	}
	return sessionTreeAction{}
}

// Render returns the dialog lines.
func (d *sessionTreeDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	lines = append(lines, frameHeader(th, "session tree", width))
	if len(d.items) == 0 {
		lines = append(lines, th.FG256(th.Muted, "no messages in this session yet"))
		lines = append(lines, th.FG256(th.Muted, "press esc to close"))
		lines = append(lines, frameRule(th, width))
		return lines
	}
	lines = append(lines, th.FG256(th.Muted, "session branches (↑/↓, pgup/pgdn, enter checkout, esc cancel):"))

	const maxRows = 12
	start := 0
	end := len(d.items)
	if end > maxRows {
		start = d.cursor - maxRows/2
		if start < 0 {
			start = 0
		}
		end = start + maxRows
		if end > len(d.items) {
			end = len(d.items)
			start = end - maxRows
		}
	}
	if start > 0 {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  ↑ %d more above", start)))
	}
	for i := start; i < end; i++ {
		it := d.items[i]
		indent := strings.Repeat("  ", it.depth)
		plain := "  " + indent + fitSessionTreeLabel(it.label, width-2-len([]rune(indent)))
		if it.current {
			plain += "  [current]"
		}
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, colorSessionTreeLine(th, plain))
		}
	}
	if end < len(d.items) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  ↓ %d more below", len(d.items)-end)))
	}
	lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("%d/%d", d.cursor+1, len(d.items))))
	lines = append(lines, frameRule(th, width))
	return lines
}

func flattenSessionFamily(root *core.TreeNode, currentPath string) []sessionTreeItem {
	childrenByFork := map[int][]*core.TreeNode{}
	for _, child := range root.Children {
		childrenByFork[child.Meta.ForkPoint] = append(childrenByFork[child.Meta.ForkPoint], child)
	}
	_, msgs, err := core.ReadSession(root.Summary.Path)
	if err != nil {
		return nil
	}
	items := buildSessionTreeItems(root.Summary.Path, msgs, 0, root.Summary.Path == currentPath)
	var out []sessionTreeItem
	for idx, item := range items {
		out = append(out, item)
		for _, child := range childrenByFork[idx+1] {
			out = append(out, flattenSessionBranch(child, currentPath, 1)...)
		}
	}
	for _, child := range childrenByFork[0] {
		out = append(out, flattenSessionBranch(child, currentPath, 1)...)
	}
	return out
}

func flattenSessionBranch(node *core.TreeNode, currentPath string, depth int) []sessionTreeItem {
	_, msgs, err := core.ReadSession(node.Summary.Path)
	if err != nil {
		return nil
	}
	start := node.Meta.ForkPoint
	if start < 0 {
		start = 0
	}
	if start > len(msgs) {
		start = len(msgs)
	}
	items := buildSessionTreeItems(node.Summary.Path, msgs[start:], depth, node.Summary.Path == currentPath)
	for idx := range items {
		items[idx].messageIdx += start
	}
	childrenByFork := map[int][]*core.TreeNode{}
	for _, child := range node.Children {
		childrenByFork[child.Meta.ForkPoint] = append(childrenByFork[child.Meta.ForkPoint], child)
	}
	var out []sessionTreeItem
	for relIdx, item := range items {
		out = append(out, item)
		absAfter := start + relIdx + 1
		for _, child := range childrenByFork[absAfter] {
			out = append(out, flattenSessionBranch(child, currentPath, depth+1)...)
		}
	}
	return out
}

func buildSessionTreeItems(path string, msgs []provider.Message, depth int, currentPath bool) []sessionTreeItem {
	out := make([]sessionTreeItem, 0, len(msgs))
	turn := 0
	lastTurn := 0
	for idx, msg := range msgs {
		label := sessionTreeRoleLabel(msg.Role)
		if msg.Role == provider.RoleUser {
			turn++
			lastTurn = turn
		} else if lastTurn == 0 {
			lastTurn = 1
		}
		preview := sessionTreePreview(msg)
		out = append(out, sessionTreeItem{
			label:      fmt.Sprintf("%s: %s", label, preview),
			messageIdx: idx,
			turnNo:     lastTurn,
			role:       msg.Role,
			prompt:     firstTextFromTreeMessage(msg),
			path:       path,
			depth:      depth,
			current:    currentPath && idx == len(msgs)-1,
		})
	}
	return out
}

func findTreeRootForPath(roots []*core.TreeNode, path string) *core.TreeNode {
	for _, root := range roots {
		if treeContainsPath(root, path) {
			return root
		}
	}
	return nil
}

func treeContainsPath(n *core.TreeNode, path string) bool {
	if n == nil {
		return false
	}
	if n.Summary.Path == path {
		return true
	}
	for _, child := range n.Children {
		if treeContainsPath(child, path) {
			return true
		}
	}
	return false
}

func indexCurrentTreeItem(items []sessionTreeItem) int {
	for i, it := range items {
		if it.current {
			return i
		}
	}
	if len(items) == 0 {
		return 0
	}
	return len(items) - 1
}

func firstTextFromTreeMessage(msg provider.Message) string {
	for _, c := range msg.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

func sessionTreeRoleLabel(role provider.Role) string {
	switch role {
	case provider.RoleUser:
		return "you"
	case provider.RoleAssistant:
		return "zot"
	case provider.RoleTool:
		return "tool"
	default:
		return string(role)
	}
}

func sessionTreePreview(msg provider.Message) string {
	var parts []string
	for _, c := range msg.Content {
		switch b := c.(type) {
		case provider.TextBlock:
			text := strings.TrimSpace(strings.ReplaceAll(b.Text, "\n", " "))
			if text != "" {
				parts = append(parts, text)
			}
		case provider.ImageBlock:
			parts = append(parts, "[image]")
		case provider.ToolCallBlock:
			parts = append(parts, "tool "+b.Name)
		case provider.ToolResultBlock:
			if b.IsError {
				parts = append(parts, "tool result error")
			} else {
				parts = append(parts, "tool result")
			}
		case provider.ReasoningBlock:
			if b.Summary != "" {
				parts = append(parts, "reasoning")
			}
		}
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, " ")
}

func colorSessionTreeLine(th tui.Theme, line string) string {
	plain := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(plain, "you:"):
		return th.FG256(th.FG, line)
	case strings.HasPrefix(plain, "zot:"):
		return th.FG256(th.Muted, line)
	case strings.HasPrefix(plain, "tool:"):
		return th.FG256(th.ToolOut, line)
	default:
		return th.FG256(th.Muted, line)
	}
}

func fitSessionTreeLabel(label string, maxWidth int) string {
	if maxWidth < 4 {
		maxWidth = 4
	}
	runes := []rune(label)
	if len(runes) <= maxWidth {
		return label
	}
	return string(runes[:maxWidth-3]) + "..."
}
