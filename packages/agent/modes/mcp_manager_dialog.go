package modes

import (
	"fmt"
	"strings"
	"sync"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/mcp"
	"github.com/patriceckhart/zot/packages/tui"
)

type mcpView int

const (
	mcpViewClosed mcpView = iota
	mcpViewList
	mcpViewServer
	mcpViewTools
	mcpViewExposure
	mcpViewRemove
	mcpViewAdd
)

// mcpActionKind is a change the host performs outside the input loop.
type mcpActionKind int

const (
	mcpActionNone mcpActionKind = iota
	mcpActionReload
	mcpActionSetEnabled
	mcpActionSetExposure
	mcpActionRemove
	mcpActionAdd
	mcpActionReconnect
	mcpActionSignIn
	mcpActionSignOut
)

// mcpAction describes one requested change. Path is the file defining the
// server, so edits land where the entry actually lives.
type mcpAction struct {
	Kind    mcpActionKind
	Name    string
	Path    string
	Enabled bool
	Value   string
	Add     mcpAddResult
}

// mcpManagerDialog is the /mcp manager: a server list with single-key
// shortcuts, a per-server view, a tool list, exposure picker, removal
// confirmation, and the add wizard, all in one dialog.
type mcpManagerDialog struct {
	mu sync.Mutex

	view     mcpView
	back     mcpView // view to return to from tools, exposure, remove
	cursor   int
	selected string
	snap     mcp.Snapshot
	snapshot func() mcp.Snapshot
	busy     string
	// cancel stops a running sign-in. authURL is shown so the user can
	// open it by hand, for example when the browser runs elsewhere.
	cancel  func()
	authURL string
	// Sign-in content scrolls independently when the terminal is short.
	authScroll, authScrollMax int
	authFollowPaste           bool
	// paste completes a running sign-in with a redirect URL copied from a
	// browser on another machine. pasteInput is the field's text and
	// pasteErr the reason the last value was refused.
	paste      func(string) error
	pasteInput string
	pasteErr   string
	// pasteRow and pasteCol locate the caret in the last render, -1 when
	// the field is not shown.
	pasteRow, pasteCol int
	notice             string
	err                string
	add                *mcpAddDialog
}

func newMCPManagerDialog() *mcpManagerDialog { return &mcpManagerDialog{add: newMCPAddDialog()} }

// Open shows the manager. startAdd opens the add wizard directly, and esc
// from the wizard then lands on the server list.
func (d *mcpManagerDialog) Open(snapshot func() mcp.Snapshot, startAdd bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.snapshot = snapshot
	// busy survives reopening: an action started before ctrl+c may still
	// be writing the file or reloading, and must finish first.
	d.cursor, d.selected, d.notice, d.err = 0, "", "", ""
	// A running reload has already closed the old connections, so a fresh
	// snapshot would show an empty list until it finishes. Finish refreshes.
	if d.busy == "" {
		d.refreshLocked()
	}
	d.view = mcpViewList
	if startAdd && d.busy == "" {
		d.openAddLocked()
	}
}

func (d *mcpManagerDialog) Active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.view != mcpViewClosed
}

func (d *mcpManagerDialog) Close() {
	d.mu.Lock()
	d.view = mcpViewClosed
	d.mu.Unlock()
	d.add.Close()
}

// SetCancel lets esc stop the running action.
func (d *mcpManagerDialog) SetCancel(cancel func()) {
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()
}

// SetAuthURL shows the authorization URL of a running sign-in and the
// field for pasting the redirect URL.
func (d *mcpManagerDialog) SetAuthURL(u string, paste func(string) error) {
	d.mu.Lock()
	d.authURL, d.paste, d.pasteInput, d.pasteErr = u, paste, "", ""
	d.authScroll, d.authScrollMax, d.authFollowPaste = 0, 0, false
	d.mu.Unlock()
}

// Busy reports whether an action is running. Keys other than ctrl+c are
// ignored meanwhile so a second edit cannot race the first.
func (d *mcpManagerDialog) Busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.busy != ""
}

// Finish records the outcome of an action and reloads the snapshot.
func (d *mcpManagerDialog) Finish(notice string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.busy, d.cancel, d.authURL = "", nil, ""
	d.paste, d.pasteInput, d.pasteErr = nil, "", ""
	d.notice, d.err = notice, ""
	if err != nil {
		d.notice, d.err = "", err.Error()
	}
	d.refreshLocked()
	if d.view == mcpViewServer && d.server() == nil {
		d.view, d.selected = mcpViewList, ""
	}
	d.clampLocked()
}

func (d *mcpManagerDialog) refreshLocked() {
	name := ""
	if d.view == mcpViewList && d.cursor < len(d.snap.Servers) {
		name = d.snap.Servers[d.cursor].Name
	}
	if d.snapshot != nil {
		d.snap = d.snapshot()
	}
	if name != "" {
		for idx, s := range d.snap.Servers {
			if s.Name == name {
				d.cursor = idx
				break
			}
		}
	}
	d.clampLocked()
}

// NeedsTickRefresh keeps initial connection progress live without animating
// static dialogs or the add wizard's editor.
func (d *mcpManagerDialog) NeedsTickRefresh() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.view == mcpViewClosed || d.view == mcpViewAdd || d.busy != "" {
		return false
	}
	for _, s := range d.snap.Servers {
		if s.State == mcp.StateConnecting {
			return true
		}
	}
	return false
}

func (d *mcpManagerDialog) server() *mcp.ServerInfo {
	for idx := range d.snap.Servers {
		if d.snap.Servers[idx].Name == d.selected {
			return &d.snap.Servers[idx]
		}
	}
	return nil
}

func (d *mcpManagerDialog) existingNames() []string {
	names := make([]string, 0, len(d.snap.Servers))
	for _, s := range d.snap.Servers {
		names = append(names, s.Name)
	}
	return names
}

func (d *mcpManagerDialog) openAddLocked() {
	d.add.Open(d.snap.UserPath, d.snap.ProjectPath, d.existingNames()...)
	d.view, d.notice, d.err = mcpViewAdd, "", ""
}

// serverActions lists the actions for the selected server in display order.
type mcpServerAction struct {
	key   rune
	label string
	hint  string
	id    string
}

func (d *mcpManagerDialog) serverActions(s *mcp.ServerInfo) []mcpServerAction {
	var out []mcpServerAction
	switch s.State {
	case mcp.StateInvalid:
		return []mcpServerAction{{'d', "Remove", "delete the entry from " + scopeName(s), "remove"}}
	case mcp.StateDisabled:
		out = append(out, mcpServerAction{'e', "Enable", "connect and register tools", "toggle"})
	default:
		if s.State == mcp.StateSignIn || (s.OAuth && !s.SignedIn) {
			out = append(out, mcpServerAction{'l', "Sign in", "opens the browser", "signin"})
		}
		if s.State == mcp.StateConnected {
			out = append(out, mcpServerAction{'t', "Tools", fmt.Sprintf("%d offered", s.ToolCount), "tools"})
		}
		out = append(out, mcpServerAction{'r', "Reconnect", "restart this server only", "reload"})
		if s.OAuth && s.SignedIn {
			out = append(out, mcpServerAction{'o', "Sign out", "delete the stored credentials", "signout"})
		}
		exposure := s.Exposure
		if s.ToolExposure {
			exposure += ", some tools override it"
		}
		out = append(out, mcpServerAction{'x', "Exposure", exposure, "exposure"})
		out = append(out, mcpServerAction{'e', "Disable", "keep the entry, stop connecting", "toggle"})
	}
	return append(out, mcpServerAction{'d', "Remove", "delete the entry from " + scopeName(s), "remove"})
}

func scopeName(s *mcp.ServerInfo) string {
	if s.Project {
		return "the project file"
	}
	return "the user file"
}

func (d *mcpManagerDialog) count() int {
	switch d.view {
	case mcpViewList:
		return len(d.snap.Servers)
	case mcpViewServer:
		if s := d.server(); s != nil {
			return len(d.serverActions(s))
		}
	case mcpViewTools:
		if s := d.server(); s != nil {
			return len(s.Tools)
		}
	case mcpViewExposure:
		return len(mcpExposureChoices)
	case mcpViewRemove:
		return 2
	}
	return 0
}

func (d *mcpManagerDialog) clampLocked() {
	if n := d.count(); d.cursor >= n {
		d.cursor = max(0, n-1)
	}
}

// HandleKey applies k and returns an action for the host to run.
func (d *mcpManagerDialog) HandleKey(k tui.Key) mcpAction {
	d.mu.Lock()
	if d.view == mcpViewClosed {
		d.mu.Unlock()
		return mcpAction{}
	}
	if k.Kind == tui.KeyCtrlC {
		d.view = mcpViewClosed
		// A sign-in waits on the browser, stop it with the dialog so the
		// callback port is released. Other actions finish in the background.
		cancel := d.cancel
		d.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		d.add.Close()
		return mcpAction{}
	}
	if d.busy != "" {
		if d.authURL != "" && (k.Kind == tui.KeyUp || k.Kind == tui.KeyDown) {
			if k.Kind == tui.KeyUp {
				d.authScroll = max(0, d.authScroll-1)
			} else {
				d.authScroll = min(d.authScrollMax, d.authScroll+1)
			}
			d.authFollowPaste = false
			d.mu.Unlock()
			return mcpAction{}
		}
		if d.paste != nil && d.handlePasteKeyLocked(k) {
			d.authFollowPaste = true
			d.mu.Unlock()
			return mcpAction{}
		}
		cancel := d.cancel
		d.mu.Unlock()
		if k.Kind == tui.KeyEsc && cancel != nil {
			cancel()
		}
		return mcpAction{}
	}
	if d.view == mcpViewAdd {
		d.mu.Unlock()
		result, done := d.add.HandleKey(k)
		d.mu.Lock()
		defer d.mu.Unlock()
		if done {
			d.view = mcpViewList
			return d.startLocked(mcpAction{Kind: mcpActionAdd, Name: result.Name, Path: result.Path, Add: result}, "adding "+result.Name)
		}
		if !d.add.Active() {
			d.view = mcpViewList
		}
		return mcpAction{}
	}
	defer d.mu.Unlock()
	d.notice, d.err = "", ""
	switch k.Kind {
	case tui.KeyEsc:
		d.goBackLocked()
		return mcpAction{}
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
		return mcpAction{}
	case tui.KeyDown:
		if d.cursor+1 < d.count() {
			d.cursor++
		}
		return mcpAction{}
	case tui.KeyLeft:
		if d.view != mcpViewList {
			d.goBackLocked()
		}
		return mcpAction{}
	}
	switch d.view {
	case mcpViewList:
		return d.listKeyLocked(k)
	case mcpViewServer:
		return d.serverKeyLocked(k)
	case mcpViewTools:
		return mcpAction{}
	case mcpViewExposure:
		if k.Kind == tui.KeyEnter {
			s := d.server()
			value := mcpExposureChoices[d.cursor].value
			d.goBackLocked()
			if s == nil || value == s.Exposure {
				return mcpAction{}
			}
			return d.startLocked(mcpAction{Kind: mcpActionSetExposure, Name: s.Name, Path: s.Source, Value: value}, "saving exposure of "+s.Name)
		}
	case mcpViewRemove:
		if isRune(k, 'y') || (k.Kind == tui.KeyEnter && d.cursor == 0) {
			s := d.server()
			// The removed row disappears, the cursor stays in place.
			d.view, d.cursor = mcpViewList, d.indexOf(d.selected)
			if s == nil {
				return mcpAction{}
			}
			return d.startLocked(mcpAction{Kind: mcpActionRemove, Name: s.Name, Path: s.Source}, "removing "+s.Name)
		}
		if isRune(k, 'n') || k.Kind == tui.KeyEnter {
			d.goBackLocked()
		}
	}
	return mcpAction{}
}

// handlePasteKeyLocked edits the redirect field of a running sign-in and
// submits it on enter. It reports whether the key was used.
func (d *mcpManagerDialog) handlePasteKeyLocked(k tui.Key) bool {
	switch k.Kind {
	case tui.KeyPaste:
		d.pasteInput += strings.Join(strings.Fields(k.Paste), "")
	case tui.KeyRune:
		if k.Ctrl || k.Alt || k.Rune < 0x20 {
			return false
		}
		d.pasteInput += string(k.Rune)
	case tui.KeyBackspace:
		if runes := []rune(d.pasteInput); len(runes) > 0 {
			d.pasteInput = string(runes[:len(runes)-1])
		}
	case tui.KeyEnter:
		if strings.TrimSpace(d.pasteInput) == "" {
			return true
		}
		if err := d.paste(d.pasteInput); err != nil {
			d.pasteErr = err.Error()
			return true
		}
		d.pasteErr, d.paste = "", nil
		d.busy = "finishing sign-in"
	default:
		return false
	}
	if k.Kind != tui.KeyEnter {
		d.pasteErr = ""
	}
	return true
}

func isRune(k tui.Key, r rune) bool {
	return k.Kind == tui.KeyRune && !k.Ctrl && !k.Alt && (k.Rune == r || k.Rune == r-'a'+'A')
}

func (d *mcpManagerDialog) goBackLocked() {
	switch d.view {
	case mcpViewList:
		d.view = mcpViewClosed
	case mcpViewServer:
		d.view, d.cursor = mcpViewList, d.indexOf(d.selected)
	case mcpViewTools, mcpViewExposure, mcpViewRemove:
		from := map[mcpView]rune{mcpViewTools: 't', mcpViewExposure: 'x', mcpViewRemove: 'd'}[d.view]
		d.view, d.cursor = d.back, 0
		if d.back == mcpViewList {
			d.cursor = d.indexOf(d.selected)
		} else if s := d.server(); s != nil {
			// Return to the action that opened the subview.
			for idx, a := range d.serverActions(s) {
				if a.key == from {
					d.cursor = idx
				}
			}
		}
	}
}

func (d *mcpManagerDialog) indexOf(name string) int {
	for idx, s := range d.snap.Servers {
		if s.Name == name {
			return idx
		}
	}
	return 0
}

func (d *mcpManagerDialog) startLocked(action mcpAction, busy string) mcpAction {
	d.busy = busy
	return action
}

func (d *mcpManagerDialog) listKeyLocked(k tui.Key) mcpAction {
	switch {
	case isRune(k, 'a'):
		d.openAddLocked()
		return mcpAction{}
	case isRune(k, 'r'):
		return d.startLocked(mcpAction{Kind: mcpActionReload}, "reloading MCP servers")
	}
	if len(d.snap.Servers) == 0 {
		if k.Kind == tui.KeyEnter {
			d.openAddLocked()
		}
		return mcpAction{}
	}
	s := &d.snap.Servers[d.cursor]
	d.selected, d.back = s.Name, mcpViewList
	switch {
	case k.Kind == tui.KeyEnter || k.Kind == tui.KeyRight:
		d.view, d.cursor = mcpViewServer, 0
	default:
		return d.shortcutLocked(k, s)
	}
	return mcpAction{}
}

func (d *mcpManagerDialog) serverKeyLocked(k tui.Key) mcpAction {
	s := d.server()
	if s == nil {
		d.view = mcpViewList
		return mcpAction{}
	}
	d.back = mcpViewServer
	if k.Kind == tui.KeyEnter || k.Kind == tui.KeyRight {
		actions := d.serverActions(s)
		if d.cursor < len(actions) {
			return d.shortcutLocked(tui.Key{Kind: tui.KeyRune, Rune: actions[d.cursor].key}, s)
		}
		return mcpAction{}
	}
	return d.shortcutLocked(k, s)
}

// shortcutLocked runs a per-server shortcut from the list or server view.
// Shortcuts that do not apply to the server's state are ignored.
func (d *mcpManagerDialog) shortcutLocked(k tui.Key, s *mcp.ServerInfo) mcpAction {
	if k.Kind != tui.KeyRune {
		return mcpAction{}
	}
	allowed := map[rune]bool{}
	for _, a := range d.serverActions(s) {
		allowed[a.key] = true
	}
	r := k.Rune
	if r >= 'A' && r <= 'Z' {
		r += 'a' - 'A'
	}
	if k.Ctrl || k.Alt || !allowed[r] {
		return mcpAction{}
	}
	switch r {
	case 't':
		d.view, d.cursor = mcpViewTools, 0
	case 'x':
		d.view, d.cursor = mcpViewExposure, exposureIndex(s.Exposure)
	case 'd':
		d.view, d.cursor = mcpViewRemove, 1
	case 'r':
		return d.startLocked(mcpAction{Kind: mcpActionReconnect, Name: s.Name}, "reconnecting "+s.Name)
	case 'l':
		return d.startLocked(mcpAction{Kind: mcpActionSignIn, Name: s.Name}, "signing in to "+s.Name+", finish in the browser")
	case 'o':
		return d.startLocked(mcpAction{Kind: mcpActionSignOut, Name: s.Name}, "signing out of "+s.Name)
	case 'e':
		enable := s.State == mcp.StateDisabled
		verb := "disabling "
		if enable {
			verb = "enabling "
		}
		return d.startLocked(mcpAction{Kind: mcpActionSetEnabled, Name: s.Name, Path: s.Source, Enabled: enable}, verb+s.Name)
	}
	return mcpAction{}
}

// Render draws the current view.
func (d *mcpManagerDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	view := d.view
	d.pasteRow = -1
	d.mu.Unlock()
	if view == mcpViewClosed {
		return nil
	}
	if view == mcpViewAdd {
		return d.add.Render(th, width)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busy == "" {
		d.refreshLocked()
	}
	var lines []string
	var keys []string
	switch view {
	case mcpViewList:
		lines, keys = d.renderListLocked(th, width)
	case mcpViewServer:
		lines, keys = d.renderServerLocked(th, width)
	case mcpViewTools:
		lines, keys = d.renderToolsLocked(th, width)
	case mcpViewExposure:
		lines, keys = d.renderExposureLocked(th, width)
	case mcpViewRemove:
		lines, keys = d.renderRemoveLocked(th, width)
	}
	if d.busy != "" {
		lines = append(lines, "")
		for _, row := range tui.WrapANSILine(d.busy+"...", max(1, width-2)) {
			lines = append(lines, th.FG256(th.Accent, "  "+row))
		}
		keys = []string{"ctrl+c close"}
		if d.authURL != "" {
			for _, row := range tui.WrapANSILine("If the browser did not open, visit:", max(1, width-2)) {
				lines = append(lines, th.FG256(th.Muted, "  "+row))
			}
			for _, row := range tui.WrapANSILine(d.authURL, max(1, width-2)) {
				lines = append(lines, "  "+row)
			}
			// Wrap the label before adding OSC 8 so each row has a complete
			// hyperlink and the target is never split across terminal rows.
			for _, row := range tui.WrapANSILine("Click here to open", max(1, width-2)) {
				lines = append(lines, "  "+tui.Hyperlink(d.authURL, row))
			}
		}
		if d.paste != nil {
			lines = append(lines, "")
			for _, row := range tui.WrapANSILine("Browser on another machine? After signing in it shows an address that does not load. Paste that address here:", max(1, width-2)) {
				lines = append(lines, th.FG256(th.Muted, "  "+row))
			}
			// +1 accounts for the blank row the frame inserts below the header.
			d.pasteRow, d.pasteCol = len(lines)+1, 2+runewidth.StringWidth(visibleInput(d.pasteInput, width))
			lines = append(lines, th.FG256(th.FG, "  "+visibleInput(d.pasteInput, width)))
			if d.pasteErr != "" {
				for _, row := range tui.WrapANSILine(d.pasteErr, max(1, width-2)) {
					lines = append(lines, th.FG256(th.Error, "  "+row))
				}
			}
		}
		if d.paste != nil {
			keys = []string{"enter submit address", "esc cancel", "ctrl+c close"}
		} else if d.cancel != nil {
			keys = []string{"esc cancel", "ctrl+c close"}
		}
	} else if d.err != "" {
		lines = append(lines, "")
		for _, row := range tui.WrapANSILine(d.err, max(1, width-4)) {
			lines = append(lines, th.FG256(th.Error, "  "+row))
		}
	} else if d.notice != "" {
		lines = append(lines, "")
		for _, row := range tui.WrapANSILine(d.notice, max(1, width-2)) {
			lines = append(lines, th.FG256(th.Accent, "  "+row))
		}
	}
	lines = append(lines, "", renderKeyHints(th, keys, width), frameRule(th, width))
	return lines
}

// renderKeyHints draws "key action" pairs with the key emphasized.
func renderKeyHints(th tui.Theme, hints []string, width int) string {
	var parts []string
	for _, hint := range hints {
		key, action, _ := strings.Cut(hint, " ")
		parts = append(parts, th.FG256(th.FG, key)+" "+th.FG256(th.Muted, action))
	}
	line := strings.Join(parts, th.FG256(th.Muted, "  "))
	rows := tui.WrapANSILine(line, max(1, width))
	return strings.Join(rows, "\n")
}

func stateMark(th tui.Theme, s mcp.State) string {
	switch s {
	case mcp.StateConnected:
		return th.FG256(th.Accent, "●")
	case mcp.StateFailed, mcp.StateInvalid, mcp.StateSignIn:
		return th.FG256(th.Error, "✕")
	}
	return th.FG256(th.Muted, "○")
}

func stateText(s mcp.ServerInfo) string {
	switch s.State {
	case mcp.StateConnecting:
		return "connecting"
	case mcp.StateConnected:
		text := fmt.Sprintf("%d tools", s.ToolCount)
		if s.ToolCount == 1 {
			text = "1 tool"
		}
		if s.Resources {
			text += " + resources"
		}
		return text
	case mcp.StateFailed, mcp.StateInvalid, mcp.StateSignIn:
		return s.Problem
	}
	return "disabled"
}

func (d *mcpManagerDialog) header(th tui.Theme, title string, width int) []string {
	return []string{frameHeader(th, title, width)}
}

func (d *mcpManagerDialog) renderListLocked(th tui.Theme, width int) ([]string, []string) {
	connected := 0
	for _, s := range d.snap.Servers {
		if s.State == mcp.StateConnected {
			connected++
		}
	}
	title := "MCP servers"
	if n := len(d.snap.Servers); n > 0 {
		title = fmt.Sprintf("MCP servers  %d/%d connected", connected, n)
	}
	lines := d.header(th, title, width)
	for _, problem := range d.snap.FileErrors {
		lines = append(lines, th.FG256(th.Error, "  "+friendlyPathInText(problem)))
	}
	if len(d.snap.Servers) == 0 {
		lines = append(lines, "  No MCP servers configured yet.")
		for _, row := range tui.WrapANSILine("Servers give the model extra tools, for example a filesystem, a browser, or an issue tracker.", max(1, width-2)) {
			lines = append(lines, th.FG256(th.Muted, "  "+row))
		}
		lines = append(lines,
			"",
			th.PadHighlight("  + Add a server", width),
			"",
			th.FG256(th.Muted, "  user     "+friendlyPath(d.snap.UserPath)),
			th.FG256(th.Muted, "  project  "+friendlyPath(d.snap.ProjectPath)),
		)
		return lines, []string{"enter add", "a add", "r reload", "esc close"}
	}
	nameWidth := 4
	for _, s := range d.snap.Servers {
		nameWidth = max(nameWidth, runewidth.StringWidth(s.Name))
	}
	nameWidth = min(nameWidth, 28)
	start, end := dialogWindow(d.cursor, len(d.snap.Servers), 12)
	for idx := start; idx < end; idx++ {
		s := d.snap.Servers[idx]
		scope := "user"
		if s.Project {
			scope = "project"
			if s.Override {
				scope = "project override"
			}
		}
		name := runewidth.FillRight(runewidth.Truncate(s.Name, nameWidth, "…"), nameWidth)
		detail := stateText(s)
		if s.State == mcp.StateConnected || s.State == mcp.StateDisabled {
			detail += "  " + s.Exposure
		}
		if idx == d.cursor {
			plain := "  " + stateMarkPlain(s.State) + "  " + name + "  " + detail + "  " + scope
			lines = append(lines, th.PadHighlight(runewidth.Truncate(plain, width, "…"), width))
			continue
		}
		color := th.Muted
		if s.State == mcp.StateFailed || s.State == mcp.StateInvalid || s.State == mcp.StateSignIn {
			color = th.Error
		}
		rest := runewidth.Truncate(detail+"  "+scope, max(0, width-nameWidth-7), "…")
		shownDetail, shownScope := rest, ""
		if strings.HasPrefix(rest, detail) {
			shownDetail, shownScope = detail, strings.TrimPrefix(rest, detail)
		}
		lines = append(lines, "  "+stateMark(th, s.State)+"  "+th.FG256(th.FG, name)+"  "+th.FG256(color, shownDetail)+th.FG256(th.Muted, shownScope))
	}
	if end-start < len(d.snap.Servers) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  %d of %d", d.cursor+1, len(d.snap.Servers))))
	}
	return lines, d.listKeys()
}

func stateMarkPlain(s mcp.State) string {
	switch s {
	case mcp.StateConnected:
		return "●"
	case mcp.StateFailed, mcp.StateInvalid, mcp.StateSignIn:
		return "✕"
	}
	return "○"
}

func (d *mcpManagerDialog) listKeys() []string {
	keys := []string{"enter open"}
	if d.cursor < len(d.snap.Servers) {
		s := &d.snap.Servers[d.cursor]
		for _, a := range d.serverActions(s) {
			if a.key == 'r' {
				continue // r reloads all servers in the list
			}
			keys = append(keys, string(a.key)+" "+strings.ToLower(a.label))
		}
	}
	return append(keys, "a add", "r reload all", "esc close")
}

func (d *mcpManagerDialog) renderServerLocked(th tui.Theme, width int) ([]string, []string) {
	s := d.server()
	if s == nil {
		return d.header(th, "MCP server", width), []string{"esc back"}
	}
	lines := d.header(th, "MCP server "+s.Name, width)
	row := func(key, value string) {
		prefix := th.FG256(th.Muted, fmt.Sprintf("  %-10s", key))
		avail := max(1, width-12)
		for idx, part := range tui.WrapANSILine(value, avail) {
			if idx > 0 {
				prefix = strings.Repeat(" ", 12)
			}
			lines = append(lines, prefix+part)
		}
	}
	status := stateMark(th, s.State) + " " + string(s.State)
	if s.State == mcp.StateConnected {
		status += th.FG256(th.Muted, "  "+stateText(*s))
	}
	row("status", status)
	if s.Problem != "" {
		row("problem", th.FG256(th.Error, s.Problem))
	}
	if s.Transport != "" {
		row("transport", s.Transport)
	}
	if s.Target != "" {
		label := "command"
		if s.Transport != "stdio" {
			label = "url"
		}
		row(label, s.Target)
	}
	if s.State != mcp.StateInvalid {
		row("exposure", s.Exposure+th.FG256(th.Muted, "  "+exposureHint(s.Exposure)))
	}
	file := friendlyPath(s.Source)
	if s.Override {
		file += th.FG256(th.Muted, "  overrides the user entry")
	}
	row("file", file)
	lines = append(lines, "")
	for idx, a := range d.serverActions(s) {
		line := fmt.Sprintf("  %c  %-10s %s", a.key, a.label, a.hint)
		if idx == d.cursor {
			lines = append(lines, th.PadHighlight(runewidth.Truncate(line, width, "…"), width))
		} else {
			lines = append(lines, th.FG256(th.Accent, fmt.Sprintf("  %c  ", a.key))+fmt.Sprintf("%-10s ", a.label)+th.FG256(th.Muted, runewidth.Truncate(a.hint, max(0, width-16), "…")))
		}
	}
	return lines, []string{"enter select", "esc back", "ctrl+c close"}
}

func exposureHint(exposure string) string {
	for _, c := range mcpExposureChoices {
		if c.value == exposure {
			return c.hint
		}
	}
	return ""
}

func (d *mcpManagerDialog) renderToolsLocked(th tui.Theme, width int) ([]string, []string) {
	s := d.server()
	if s == nil {
		return d.header(th, "Tools", width), []string{"esc back"}
	}
	lines := d.header(th, fmt.Sprintf("Tools of %s  %d", s.Name, len(s.Tools)), width)
	for _, row := range tui.WrapANSILine("exposure "+s.Exposure+": "+exposureHint(s.Exposure), max(1, width-2)) {
		lines = append(lines, th.FG256(th.Muted, "  "+row))
	}
	if s.Exposure == "hidden" {
		lines = append(lines, th.FG256(th.Muted, "  hidden tools are not callable by the model"))
	}
	lines = append(lines, "")
	if len(s.Tools) == 0 {
		lines = append(lines, th.FG256(th.Muted, "  The server offers no tools."))
		return lines, []string{"esc back"}
	}
	nameWidth := 0
	for _, t := range s.Tools {
		nameWidth = max(nameWidth, runewidth.StringWidth(t.Name))
	}
	nameWidth = min(nameWidth, 32)
	start, end := dialogWindow(d.cursor, len(s.Tools), 12)
	for idx := start; idx < end; idx++ {
		t := s.Tools[idx]
		name := runewidth.FillRight(runewidth.Truncate(t.Name, nameWidth, "…"), nameWidth)
		desc := runewidth.Truncate(t.Description, max(0, width-nameWidth-6), "…")
		if idx == d.cursor {
			lines = append(lines, th.PadHighlight(runewidth.Truncate("  "+name+"  "+desc, width, "…"), width))
		} else {
			lines = append(lines, "  "+th.FG256(th.FG, name)+"  "+th.FG256(th.Muted, desc))
		}
	}
	if end-start < len(s.Tools) {
		lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  %d of %d", d.cursor+1, len(s.Tools))))
	}
	return lines, []string{"↑↓ scroll", "esc back"}
}

func (d *mcpManagerDialog) renderExposureLocked(th tui.Theme, width int) ([]string, []string) {
	s := d.server()
	name := ""
	current := ""
	if s != nil {
		name, current = s.Name, s.Exposure
	}
	lines := d.header(th, "Exposure of "+name, width)
	if s != nil {
		for _, row := range tui.WrapANSILine("saved to "+friendlyPath(s.Source), max(1, width-2)) {
			lines = append(lines, th.FG256(th.Muted, "  "+row))
		}
		lines = append(lines, "")
	}
	for idx, c := range mcpExposureChoices {
		mark := "  "
		if c.value == current {
			mark = "✓ "
		}
		line := fmt.Sprintf("  %s%-9s %s", mark, c.label, c.hint)
		if idx == d.cursor {
			lines = append(lines, th.PadHighlight(runewidth.Truncate(line, width, "…"), width))
		} else {
			lines = append(lines, th.FG256(th.Muted, runewidth.Truncate(line, width, "…")))
		}
	}
	return lines, []string{"enter save", "esc back"}
}

func (d *mcpManagerDialog) renderRemoveLocked(th tui.Theme, width int) ([]string, []string) {
	s := d.server()
	if s == nil {
		return d.header(th, "Remove", width), []string{"esc back"}
	}
	lines := d.header(th, "Remove "+s.Name, width)
	for _, row := range tui.WrapANSILine("Delete "+s.Name+" from "+friendlyPath(s.Source)+"?", max(1, width-2)) {
		lines = append(lines, "  "+row)
	}
	if s.Override {
		lines = append(lines, th.FG256(th.Muted, "  The user entry with the same name becomes active again."))
	}
	lines = append(lines, "")
	for idx, label := range []string{"Remove", "Keep"} {
		line := "  " + label
		if idx == d.cursor {
			lines = append(lines, th.PadHighlight(line, width))
		} else {
			lines = append(lines, th.FG256(th.Muted, line))
		}
	}
	return lines, []string{"y remove", "n keep", "esc back"}
}

// CursorPos forwards the caret of the add wizard.
func (d *mcpManagerDialog) CursorPos() (row, col int) {
	d.mu.Lock()
	view := d.view
	pasteRow, pasteCol := d.pasteRow, d.pasteCol
	pasting := d.paste != nil
	d.mu.Unlock()
	if pasting && pasteRow >= 0 {
		return pasteRow, pasteCol
	}
	if view != mcpViewAdd {
		return -1, -1
	}
	return d.add.CursorPos()
}

// friendlyPathInText shortens a leading absolute path in a message.
func friendlyPathInText(s string) string {
	path, rest, ok := strings.Cut(s, ": ")
	if !ok {
		return s
	}
	return friendlyPath(path) + ": " + rest
}
