package modes

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/tui"
)

type settingsDialog struct {
	active       bool
	title        string
	items        []settingsItem
	cursor       int
	selecting    bool
	direct       bool
	optionCursor int
	parentItems  []settingsItem
	parentCursor int
	version      string

	// MaxRows caps how many rows the dialog renders. The host sets it
	// from the terminal height before each render; zero renders the
	// list in full. A list taller than the cap scrolls in a window
	// that follows the cursor so entries never disappear off-screen.
	MaxRows int

	// viewTop is the index of the first entry drawn in the current
	// window. It survives across renders so the window only moves when
	// the cursor pushes past one of its edges, and is reset whenever
	// the dialog swaps between the item list and an option list.
	viewTop int
}

type settingsItem struct {
	key      string
	label    string
	desc     string
	value    bool
	options  []settingsOption
	children []settingsItem
	picker   bool
	choice   int
	disabled bool
	hint     string
}

type settingsOption struct {
	value string
	label string
	desc  string
}

type settingsAction struct {
	Toggle            bool
	Key               string
	Value             bool
	StringValue       string
	ModelShortcutSlot int
	Close             bool
}

func newSettingsDialog() *settingsDialog { return &settingsDialog{} }

func (d *settingsDialog) Open(items []settingsItem) bool {
	if len(items) == 0 {
		return false
	}
	d.title = "settings"
	d.items = items
	d.cursor = 0
	d.selecting = false
	d.direct = false
	d.optionCursor = 0
	d.parentItems = nil
	d.parentCursor = 0
	d.viewTop = 0
	d.active = true
	return true
}

func (d *settingsDialog) OpenDirectOption(item settingsItem) bool {
	if len(item.options) == 0 {
		return false
	}
	d.title = item.label
	d.items = []settingsItem{item}
	d.cursor = 0
	d.selecting = true
	d.direct = true
	d.optionCursor = item.choice
	if d.optionCursor < 0 || d.optionCursor >= len(item.options) {
		d.optionCursor = 0
	}
	d.parentItems = nil
	d.parentCursor = 0
	d.viewTop = 0
	d.active = true
	return true
}

func (d *settingsDialog) Close() {
	d.active = false
	d.selecting = false
	d.direct = false
	d.parentItems = nil
	d.viewTop = 0
}
func (d *settingsDialog) Active() bool { return d != nil && d.active }

func (d *settingsDialog) HandleKey(k tui.Key) settingsAction {
	if d.selecting {
		return d.handleOptionKey(k)
	}
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.items)-1 {
			d.cursor++
		}
	case tui.KeyBackspace:
		if len(d.items) > 0 {
			it := d.items[d.cursor]
			if strings.HasPrefix(it.key, "quick_model_") {
				return settingsAction{Toggle: true, Key: it.key, StringValue: ""}
			}
		}
	case tui.KeyEsc:
		if len(d.parentItems) > 0 {
			d.items = d.parentItems
			d.cursor = d.parentCursor
			d.parentItems = nil
			d.parentCursor = 0
			d.viewTop = 0
			d.title = "settings"
			return settingsAction{}
		}
		d.Close()
		return settingsAction{Close: true}
	case tui.KeyEnter:
		return d.toggleCurrent()
	case tui.KeyRune:
		if k.Rune == ' ' {
			return d.toggleCurrent()
		}
	}
	return settingsAction{}
}

func (d *settingsDialog) handleOptionKey(k tui.Key) settingsAction {
	it := d.items[d.cursor]
	switch k.Kind {
	case tui.KeyUp:
		if d.optionCursor > 0 {
			d.optionCursor--
		}
	case tui.KeyDown:
		if d.optionCursor < len(it.options)-1 {
			d.optionCursor++
		}
	case tui.KeyEsc:
		if d.direct {
			d.Close()
			return settingsAction{Close: true}
		}
		d.selecting = false
		d.viewTop = 0
	case tui.KeyEnter:
		return d.selectCurrentOption()
	case tui.KeyRune:
		if k.Rune == ' ' {
			return d.selectCurrentOption()
		}
	}
	return settingsAction{}
}

func (d *settingsDialog) toggleCurrent() settingsAction {
	if len(d.items) == 0 {
		d.Close()
		return settingsAction{Close: true}
	}
	it := d.items[d.cursor]
	if it.disabled {
		return settingsAction{}
	}
	if it.picker {
		slotText := strings.TrimPrefix(it.key, "quick_model_")
		slot := 0
		for _, r := range slotText {
			if r < '0' || r > '9' {
				slot = 0
				break
			}
			slot = slot*10 + int(r-'0')
		}
		return settingsAction{ModelShortcutSlot: slot}
	}
	if len(it.children) > 0 {
		d.parentItems = d.items
		d.parentCursor = d.cursor
		d.items = it.children
		d.cursor = 0
		d.optionCursor = 0
		d.viewTop = 0
		d.title = "settings: " + it.label
		return settingsAction{}
	}
	if len(it.options) > 0 {
		d.optionCursor = it.choice
		if d.optionCursor < 0 || d.optionCursor >= len(it.options) {
			d.optionCursor = 0
		}
		d.selecting = true
		d.viewTop = 0
		return settingsAction{}
	}
	it.value = !it.value
	d.items[d.cursor] = it
	return settingsAction{Toggle: true, Key: it.key, Value: it.value}
}

func (d *settingsDialog) selectCurrentOption() settingsAction {
	if len(d.items) == 0 {
		d.Close()
		return settingsAction{Close: true}
	}
	it := d.items[d.cursor]
	if len(it.options) == 0 {
		d.selecting = false
		return settingsAction{}
	}
	if d.optionCursor < 0 || d.optionCursor >= len(it.options) {
		d.optionCursor = 0
	}
	it.choice = d.optionCursor
	d.items[d.cursor] = it
	d.selecting = false
	d.viewTop = 0
	action := settingsAction{Toggle: true, Key: it.key, StringValue: it.options[it.choice].value}
	if d.direct {
		d.Close()
		action.Close = true
	}
	return action
}

func (d *settingsDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	if d.selecting {
		return d.renderOptions(th, width)
	}
	var lines []string
	lines = append(lines, frameHeader(th, d.title, width))
	if len(d.parentItems) > 0 {
		lines = append(lines, th.FG256(th.Muted, "change with enter/space, esc to go back:"))
	} else {
		lines = append(lines, th.FG256(th.Muted, "change with enter/space, esc to close:"))
	}

	// Render every entry to its own block up front so the row window
	// can count wrapped descriptions, then draw only the blocks that
	// fit the budget. Windowing keeps the cursor entry on screen: a
	// list taller than the terminal scrolls instead of having its top
	// entries clipped off and unreachable.
	blocks := make([][]string, len(d.items))
	heights := make([]int, len(d.items))
	for i := range d.items {
		blocks[i] = d.itemBlock(th, width, i)
		heights[i] = len(blocks[i])
	}
	fixed := len(lines) + 1 // one row for the closing rule
	// Leave at least one row for the selected setting.
	showVersion := d.version != "" && len(d.parentItems) == 0 && width > 0 &&
		(d.MaxRows <= 0 || d.MaxRows > fixed+1)
	if showVersion {
		fixed++
	}
	padVersion := showVersion && (d.MaxRows <= 0 || d.MaxRows > fixed+1)
	if padVersion {
		fixed++
	}
	budget := d.rowBudget(fixed)
	start, end := d.windowBlocks(heights, d.cursor, budget)
	lines = append(lines, windowLines(th, blocks, start, end, budget)...)
	if padVersion {
		lines = append(lines, "")
	}
	if showVersion {
		lines = append(lines, th.FG256(th.Muted, runewidth.Truncate("zot "+d.version, width, "…")))
	}
	lines = append(lines, frameRule(th, width))
	return lines
}

// itemBlock renders one entry as its label row followed by any wrapped
// description rows. Keeping both in one block lets the row window
// count the description and trim it when space runs out without losing
// the label row the cursor highlight sits on.
func (d *settingsDialog) itemBlock(th tui.Theme, width, idx int) []string {
	it := d.items[idx]
	box := "[ ]"
	if it.value {
		box = "[✓]"
	}
	plain := "  " + box + " " + it.label
	if it.picker || len(it.children) > 0 {
		box = "[→]"
		plain = "  " + box + " " + it.label
	} else if len(it.options) > 0 {
		box = "[→]"
		if it.choice < 0 || it.choice >= len(it.options) {
			it.choice = 0
		}
		plain = "  " + box + " " + it.label + ": " + it.options[it.choice].label
	}
	if it.hint != "" {
		plain += "  " + th.FG256(th.Muted, "("+it.hint+")")
	}
	line := plain
	switch {
	case it.disabled:
		line = th.FG256(th.Muted, plain)
	case idx == d.cursor:
		line = th.PadHighlight(plain, width)
	}
	block := []string{line}
	for _, desc := range wrapSettingDescription(it.desc, width, 6) {
		block = append(block, th.FG256(th.Muted, desc))
	}
	return block
}

// rowBudget returns the rows left for the window after the fixed rows
// around it (frame header, hint, closing rule) are counted against
// MaxRows. Zero means unbounded.
func (d *settingsDialog) rowBudget(fixed int) int {
	if d.MaxRows <= 0 {
		return 0
	}
	if budget := d.MaxRows - fixed; budget > 0 {
		return budget
	}
	return 1
}

// windowBlocks returns the [start, end) range of entry blocks to draw
// so the cursor entry stays visible. Heights are the rendered row
// counts per entry, budget is the rows available for the blocks plus
// the "more above/below" markers, and zero means windowing is off.
func (d *settingsDialog) windowBlocks(heights []int, cursor, budget int) (start, end int) {
	total := len(heights)
	if total == 0 {
		return 0, 0
	}
	if budget <= 0 {
		d.viewTop = 0
		return 0, total
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= total {
		cursor = total - 1
	}
	rows := 0
	for _, h := range heights {
		rows += h
	}
	if rows <= budget {
		d.viewTop = 0
		return 0, total
	}
	// Entries are hidden above and/or below, so hold back a row for
	// the markers. On a terminal too short for the cursor entry plus
	// markers, drop the reservation: the highlighted row matters more
	// than the hint that more entries exist.
	inner := budget - 2
	if inner < heights[cursor] {
		inner = budget
	}
	return d.anchorBlocks(heights, cursor, inner)
}

// anchorBlocks returns the block range of combined height <= budget
// that keeps the cursor entry visible. The window extends downward
// from the persistent viewTop; when the cursor moves past either edge
// the window re-anchors so the cursor entry becomes the first or last
// visible one. viewTop is updated for the next render so the window
// stays put while the cursor moves inside it.
func (d *settingsDialog) anchorBlocks(heights []int, cursor, budget int) (start, end int) {
	total := len(heights)
	start = d.viewTop
	if start > cursor || start >= total {
		start = cursor
	}
	used := 0
	for end = start; end < total && (end == start || used+heights[end] <= budget); end++ {
		used += heights[end]
	}
	if cursor >= end {
		// The cursor fell past the bottom edge: re-anchor on it and
		// fill the remaining budget with entries above.
		start, end = cursor, cursor+1
		used = heights[cursor]
		for start > 0 && used+heights[start-1] <= budget {
			start--
			used += heights[start]
		}
	}
	d.viewTop = start
	return start, end
}

// windowLines renders the [start, end) slice of entry blocks, framed by
// muted "more above/below" markers when entries are hidden on either
// side. Blocks are trimmed so the result never uses more than budget
// rows; budget <= 0 means unbounded.
func windowLines(th tui.Theme, blocks [][]string, start, end, budget int) []string {
	unbounded := budget <= 0
	remaining := budget
	var lines []string
	// Spend a row on the above marker only when the first block still
	// gets a row afterwards: on a terminal this short the highlight
	// matters more than the hint that entries are hidden.
	if start > 0 && (unbounded || remaining > 1) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  ↑ %d more above", start)))
		remaining--
	}
	for i := start; i < end; i++ {
		block := blocks[i]
		if !unbounded {
			if remaining <= 0 {
				break
			}
			if len(block) > remaining {
				block = block[:remaining]
			}
		}
		lines = append(lines, block...)
		remaining -= len(block)
	}
	if end < len(blocks) && (unbounded || remaining > 0) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  ↓ %d more below", len(blocks)-end)))
	}
	return lines
}

func (d *settingsDialog) renderOptions(th tui.Theme, width int) []string {
	if len(d.items) == 0 || d.cursor < 0 || d.cursor >= len(d.items) {
		d.selecting = false
		return d.Render(th, width)
	}
	it := d.items[d.cursor]
	title := "settings: " + it.label
	if d.direct {
		title = d.title
	}
	lines := []string{frameHeader(th, title, width)}
	// Keep room for the hint, closing rule, and at least the selected
	// option's label before spending a row on the optional description.
	if it.desc != "" && (d.MaxRows <= 0 || d.MaxRows >= 5) {
		lines = append(lines, th.FG256(th.Muted, it.desc))
	}
	lines = append(lines, th.FG256(th.Muted, "select with enter/space, esc to go back:"))
	blocks := make([][]string, len(it.options))
	heights := make([]int, len(it.options))
	for idx, opt := range it.options {
		marker := "  "
		if idx == it.choice {
			marker = "✓ "
		}
		plain := "  " + marker + opt.label
		block := make([]string, 0, 4)
		if idx == d.optionCursor {
			block = append(block, th.PadHighlight(plain, width))
		} else {
			block = append(block, plain)
		}
		for _, desc := range wrapSettingDescription(opt.desc, width, 6) {
			block = append(block, th.FG256(th.Muted, desc))
		}
		blocks[idx] = block
		heights[idx] = len(block)
	}
	// Same windowing as the item list: a theme list longer than the
	// terminal scrolls with the cursor instead of losing its head.
	budget := d.rowBudget(len(lines) + 1) // one row for the closing rule
	start, end := d.windowBlocks(heights, d.optionCursor, budget)
	lines = append(lines, windowLines(th, blocks, start, end, budget)...)
	lines = append(lines, frameRule(th, width))
	return lines
}

func wrapSettingDescription(desc string, width, indent int) []string {
	prefix := strings.Repeat(" ", indent)
	limit := width - indent
	if limit < 20 {
		limit = 20
	}
	words := strings.Fields(desc)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		candidate := line + " " + word
		if runewidth.StringWidth(candidate) <= limit {
			line = candidate
			continue
		}
		lines = append(lines, prefix+line)
		line = word
	}
	lines = append(lines, prefix+line)
	return lines
}
