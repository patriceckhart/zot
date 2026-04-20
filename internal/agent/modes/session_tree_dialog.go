package modes

import (
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/tui"
)

// sessionTreeDialog renders the branch topology for the current
// cwd (parents, children, siblings) and lets the user pick another
// session file to switch into. Reached via /session tree.
//
// Layout is a single flat list with indentation-based nesting so
// we can reuse the simple up/down cursor model the other dialogs
// use. The current session is tagged with a muted "[current]".
type sessionTreeDialog struct {
	active  bool
	items   []treeItem
	cursor  int
	current string // path of the session we're currently in (not selectable)
}

// treeItem is one visible row. Depth drives the indent; Path is the
// on-disk session file to swap to when the user hits enter.
type treeItem struct {
	label string
	path  string
	depth int
	isCur bool
}

type sessionTreeAction struct {
	Select bool
	Path   string
	Close  bool
}

func newSessionTreeDialog() *sessionTreeDialog { return &sessionTreeDialog{} }

// Open flattens the given forest into indented rows. currentPath
// is highlighted and non-selectable (enter on it closes the dialog).
func (d *sessionTreeDialog) Open(roots []*core.TreeNode, currentPath string) bool {
	items := flattenTree(roots, currentPath)
	if len(items) == 0 {
		return false
	}
	d.items = items
	d.current = currentPath
	d.cursor = indexOfCurrent(items, currentPath)
	d.active = true
	return true
}

// Close hides the dialog.
func (d *sessionTreeDialog) Close() { d.active = false }

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
		if it.isCur {
			return sessionTreeAction{Close: true}
		}
		return sessionTreeAction{Select: true, Path: it.path}
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
	lines = append(lines, th.FG256(th.Muted, "pick a branch to switch to (\u2191/\u2193, enter, esc to cancel):"))
	for i, it := range d.items {
		indent := strings.Repeat("  ", it.depth)
		label := "  " + indent + it.label
		if it.isCur {
			label += "  " + th.FG256(th.Muted, "[current]")
		}
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(label, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, label))
		}
	}
	lines = append(lines, frameRule(th, width))
	return lines
}

// flattenTree walks the forest depth-first and returns one treeItem
// per node. Each label has the shape "<when>  <first-prompt>  (N msgs)".
func flattenTree(roots []*core.TreeNode, currentPath string) []treeItem {
	var out []treeItem
	var walk func(n *core.TreeNode, depth int)
	walk = func(n *core.TreeNode, depth int) {
		label := formatTreeRow(n)
		out = append(out, treeItem{
			label: label,
			path:  n.Summary.Path,
			depth: depth,
			isCur: n.Summary.Path == currentPath,
		})
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return out
}

// formatTreeRow renders one node's label line. Tries to fit a
// compact "date  preview  (N msgs, $cost)" shape; falls back to
// the meta id suffix when everything else is empty.
func formatTreeRow(n *core.TreeNode) string {
	when := formatRelative(n.Summary.Started)
	preview := strings.TrimSpace(n.Summary.FirstUserText)
	if preview == "" {
		if n.Meta.ID != "" && len(n.Meta.ID) >= 8 {
			preview = "(" + n.Meta.ID[:8] + ")"
		} else {
			preview = "(empty)"
		}
	}
	if len(preview) > 50 {
		preview = preview[:49] + "\u2026"
	}
	return fmt.Sprintf("%-14s %s  %d msgs", when, preview, n.Summary.MessageCount)
}

// indexOfCurrent returns the flat-list index of the row whose path
// matches currentPath. -1 when not found; the caller defaults to 0.
func indexOfCurrent(items []treeItem, currentPath string) int {
	for i, it := range items {
		if it.path == currentPath {
			return i
		}
	}
	return 0
}
