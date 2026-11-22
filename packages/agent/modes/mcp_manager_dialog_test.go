package modes

import (
	"errors"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/mcp"
	"github.com/patriceckhart/zot/packages/tui"
)

func managerFixture(servers ...mcp.ServerInfo) *mcpManagerDialog {
	d := newMCPManagerDialog()
	snap := mcp.Snapshot{Loaded: true, UserPath: "/home/u/mcp.json", ProjectPath: "/p/.zot/mcp.json", Servers: servers}
	d.Open(func() mcp.Snapshot { return snap }, false)
	return d
}

func key(r rune) tui.Key { return tui.Key{Kind: tui.KeyRune, Rune: r} }

func renderPlain(d *mcpManagerDialog) string {
	return plainMCPRow(strings.Join(d.Render(tui.Theme{}, 100), "\n"))
}

func plainMCPRow(s string) string {
	for {
		start := strings.Index(s, "\x1b]8;")
		if start < 0 {
			return stripANSIBytes(s)
		}
		end := strings.Index(s[start:], "\x1b\\")
		if end < 0 {
			return stripANSIBytes(s)
		}
		s = s[:start] + s[start+end+2:]
	}
}

var (
	connectedServer = mcp.ServerInfo{Name: "docs", State: mcp.StateConnected, Transport: "streamable-http", Target: "https://example.test/mcp", Exposure: "deferred", Source: "/home/u/mcp.json", ToolCount: 2, Tools: []mcp.ToolInfo{{Name: "search", Description: "Search docs"}, {Name: "fetch"}}}
	failedServer    = mcp.ServerInfo{Name: "fs", State: mcp.StateFailed, Problem: "command not found", Transport: "stdio", Target: "missing-binary", Exposure: "deferred", Source: "/p/.zot/mcp.json", Project: true}
	disabledServer  = mcp.ServerInfo{Name: "old", State: mcp.StateDisabled, Transport: "stdio", Exposure: "deferred", Source: "/home/u/mcp.json"}
)

func TestMCPManagerEmptyStateOffersAdd(t *testing.T) {
	d := managerFixture()
	out := renderPlain(d)
	if !strings.Contains(out, "No MCP servers configured yet") || !strings.Contains(out, "+ Add a server") {
		t.Fatalf("empty view=%s", out)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if d.view != mcpViewAdd {
		t.Fatal("enter on empty list did not open the add view")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if d.view != mcpViewList || !d.Active() {
		t.Fatal("esc from the first add step did not return to the list")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if d.Active() {
		t.Fatal("esc on the list did not close")
	}
}

func TestMCPManagerListShortcuts(t *testing.T) {
	d := managerFixture(failedServer, connectedServer, disabledServer)
	out := renderPlain(d)
	for _, want := range []string{"1/3 connected", "fs", "command not found", "project", "2 tools", "disabled", "r reload all", "x exposure", "a add"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q:\n%s", want, out)
		}
	}
	// t is not offered for a failed server and is ignored.
	if action := d.HandleKey(key('t')); action.Kind != mcpActionNone || d.view != mcpViewList {
		t.Fatal("tools shortcut accepted for a failed server")
	}
	action := d.HandleKey(key('r'))
	if action.Kind != mcpActionReload || action.Name != "" || !d.Busy() {
		t.Fatalf("reload all=%+v", action)
	}
	if d.HandleKey(key('a')).Kind != mcpActionNone || d.view != mcpViewList {
		t.Fatal("keys were accepted while busy")
	}
	d.Finish("reconnected fs", nil)
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(key('t'))
	if d.view != mcpViewTools || !strings.Contains(renderPlain(d), "Search docs") {
		t.Fatalf("tools view=%s", renderPlain(d))
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if d.view != mcpViewList || d.cursor != 1 {
		t.Fatalf("back from tools: view=%v cursor=%d", d.view, d.cursor)
	}
	action = d.HandleKey(key('E'))
	if action.Kind != mcpActionSetEnabled || action.Enabled || action.Path != "/home/u/mcp.json" {
		t.Fatalf("disable=%+v", action)
	}
	d.Finish("disabled docs", nil)
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	if action = d.HandleKey(key('e')); action.Kind != mcpActionSetEnabled || !action.Enabled || action.Name != "old" {
		t.Fatalf("enable=%+v", action)
	}
	d.Finish("", nil)
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if action = d.HandleKey(key('r')); action.Kind != mcpActionNone {
		t.Fatalf("reconnect offered for a disabled server: %+v", action)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	d.HandleKey(tui.Key{Kind: tui.KeyUp})
	d.HandleKey(tui.Key{Kind: tui.KeyUp})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if action = d.HandleKey(key('r')); action.Kind != mcpActionReconnect || action.Name != "fs" {
		t.Fatalf("server reconnect=%+v", action)
	}
}

func TestMCPManagerServerViewExposureAndRemove(t *testing.T) {
	d := managerFixture(connectedServer)
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	out := renderPlain(d)
	for _, want := range []string{"MCP server docs", "https://example.test/mcp", "streamable-http", "t  Tools", "x  Exposure", "d  Remove"} {
		if !strings.Contains(out, want) {
			t.Fatalf("server view missing %q:\n%s", want, out)
		}
	}
	d.HandleKey(key('x'))
	if d.view != mcpViewExposure || !strings.Contains(renderPlain(d), "✓ deferred") {
		t.Fatalf("exposure view=%s", renderPlain(d))
	}
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.Kind != mcpActionNone || d.view != mcpViewServer {
		t.Fatal("saving the unchanged exposure produced an action")
	}
	d.HandleKey(key('x'))
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	action := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if action.Kind != mcpActionSetExposure || action.Value != "direct" {
		t.Fatalf("exposure=%+v", action)
	}
	d.Finish("", nil)
	d.HandleKey(key('d'))
	if d.view != mcpViewRemove || d.cursor != 1 {
		t.Fatal("remove did not default to keep")
	}
	if action = d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.Kind != mcpActionNone || d.view != mcpViewServer {
		t.Fatal("enter on keep removed the server")
	}
	d.HandleKey(key('d'))
	if action = d.HandleKey(key('y')); action.Kind != mcpActionRemove || action.Name != "docs" {
		t.Fatalf("remove=%+v", action)
	}
}

func TestMCPManagerAddRejectsExistingName(t *testing.T) {
	d := managerFixture(connectedServer)
	d.HandleKey(key('a'))
	d.HandleKey(tui.Key{Kind: tui.KeyEnter}) // user
	d.HandleKey(tui.Key{Kind: tui.KeyEnter}) // local command
	for _, r := range "docs" {
		d.HandleKey(key(r))
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !strings.Contains(renderPlain(d), "already exists") {
		t.Fatalf("duplicate accepted:\n%s", renderPlain(d))
	}
}

func TestMCPManagerRendersWithinNarrowWidth(t *testing.T) {
	const width = 40
	check := func(d *mcpManagerDialog, view string) {
		t.Helper()
		for _, line := range d.Render(tui.Dark, width) {
			for _, row := range strings.Split(line, "\n") {
				if w := runewidth.StringWidth(stripANSIBytes(row)); w > width {
					t.Fatalf("%s: row width %d > %d: %q", view, w, width, stripANSIBytes(row))
				}
			}
		}
	}
	long := connectedServer
	long.Target = "https://" + strings.Repeat("very-long-host.", 6) + "example/mcp"
	long.Source = "/" + strings.Repeat("deep/", 12) + "mcp.json"
	d := managerFixture(failedServer, long)
	check(d, "list")
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	check(d, "server")
	for _, view := range []rune{'t', 'x', 'd'} {
		d.HandleKey(key(view))
		check(d, string(view))
		d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	}
	check(managerFixture(), "empty")
	d = managerFixture()
	d.HandleKey(key('a'))
	check(d, "add scope")
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	for _, r := range "docs" {
		d.HandleKey(key(r))
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	for _, r := range long.Target {
		d.HandleKey(key(r))
	}
	check(d, "add url")
	if row, col := d.CursorPos(); row < 0 || col >= width {
		t.Fatalf("caret outside the dialog: row=%d col=%d", row, col)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	check(d, "review")
}

func TestMCPManagerReturnsToTheUsedAction(t *testing.T) {
	d := managerFixture(connectedServer)
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(key('x'))
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if d.view != mcpViewServer || d.serverActions(d.server())[d.cursor].key != 'x' {
		t.Fatalf("view=%v cursor=%d", d.view, d.cursor)
	}
	d.HandleKey(key('d'))
	d.HandleKey(key('n'))
	if d.serverActions(d.server())[d.cursor].key != 'd' {
		t.Fatalf("cursor=%d after keeping", d.cursor)
	}
}

func TestMCPManagerReopenWhileBusyKeepsTheGuard(t *testing.T) {
	d := managerFixture(connectedServer)
	if d.HandleKey(key('r')).Kind != mcpActionReload {
		t.Fatal("reload not started")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyCtrlC})
	snap := d.snap
	d.Open(func() mcp.Snapshot { return snap }, true)
	if d.view == mcpViewAdd || !d.Busy() {
		t.Fatal("reopening dropped the running action")
	}
	if d.HandleKey(key('a')).Kind != mcpActionNone || d.view == mcpViewAdd {
		t.Fatal("a second action started while one runs")
	}
	d.Finish("reloaded MCP servers", nil)
	if d.Busy() {
		t.Fatal("finish did not clear busy")
	}
}

func TestMCPAddUsesTheManagerPaths(t *testing.T) {
	d := managerFixture()
	d.HandleKey(key('a'))
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	for _, r := range "fs" {
		d.HandleKey(key(r))
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	for _, r := range "server" {
		d.HandleKey(key(r))
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	action := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if action.Kind != mcpActionAdd || action.Add.Path != "/p/.zot/mcp.json" {
		t.Fatalf("action=%+v", action)
	}
}

func TestMCPManagerSignInKeysAndCancel(t *testing.T) {
	needsSignIn := mcp.ServerInfo{Name: "secure", State: mcp.StateSignIn, Problem: "sign in to connect", Transport: "streamable-http", Exposure: "deferred", Source: "/home/u/mcp.json", OAuth: true}
	d := managerFixture(needsSignIn)
	if out := renderPlain(d); !strings.Contains(out, "l sign in") || !strings.Contains(out, "sign in to connect") {
		t.Fatalf("list=%s", out)
	}
	action := d.HandleKey(key('l'))
	if action.Kind != mcpActionSignIn || action.Name != "secure" {
		t.Fatalf("sign-in=%+v", action)
	}
	cancelled := false
	d.SetCancel(func() { cancelled = true })
	d.SetAuthURL("https://auth.example.test/authorize?x=1", nil)
	out := renderPlain(d)
	if !strings.Contains(out, "https://auth.example.test/authorize?x=1") || !strings.Contains(out, "esc cancel") {
		t.Fatalf("progress=%s", out)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if !cancelled || !d.Busy() {
		t.Fatal("esc did not cancel the sign-in")
	}
	d.Finish("", nil)
	signedIn := needsSignIn
	signedIn.State, signedIn.SignedIn, signedIn.ToolCount = mcp.StateConnected, true, 1
	d = managerFixture(signedIn)
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if out := renderPlain(d); !strings.Contains(out, "o  Sign out") || strings.Contains(out, "Sign in ") {
		t.Fatalf("signed-in server view=%s", out)
	}
	if action := d.HandleKey(key('o')); action.Kind != mcpActionSignOut {
		t.Fatalf("sign-out=%+v", action)
	}
}

func TestMCPManagerCtrlCCancelsSignIn(t *testing.T) {
	d := managerFixture(mcp.ServerInfo{Name: "secure", State: mcp.StateSignIn, OAuth: true, Source: "/home/u/mcp.json"})
	d.HandleKey(key('l'))
	cancelled := false
	d.SetCancel(func() { cancelled = true })
	d.HandleKey(tui.Key{Kind: tui.KeyCtrlC})
	if !cancelled || d.Active() {
		t.Fatal("closing the dialog left the sign-in waiting")
	}
}

func TestMCPManagerPasteFieldSubmitsTheAddress(t *testing.T) {
	d := managerFixture(mcp.ServerInfo{Name: "secure", State: mcp.StateSignIn, OAuth: true, Source: "/home/u/mcp.json"})
	d.HandleKey(key('l'))
	var got []string
	d.SetAuthURL("https://auth.example.test/authorize", func(input string) error {
		got = append(got, input)
		if input != "http://127.0.0.1:1/callback?code=c&state=s" {
			return errors.New("this URL belongs to another sign-in")
		}
		return nil
	})
	if out := renderPlain(d); !strings.Contains(out, "Browser on another machine") || !strings.Contains(out, "enter submit address") {
		t.Fatalf("paste field missing:\n%s", out)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "http://127.0.0.1:1/callback?code=c&state=x"})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !strings.Contains(renderPlain(d), "another sign-in") || d.paste == nil {
		t.Fatal("refused address not reported or field closed")
	}
	if row, _ := d.CursorPos(); row < 0 {
		t.Fatal("caret not in the paste field")
	}
	for range "x" {
		d.HandleKey(tui.Key{Kind: tui.KeyBackspace})
	}
	d.HandleKey(key('s'))
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if len(got) != 2 || d.paste != nil || !strings.Contains(renderPlain(d), "finishing sign-in") {
		t.Fatalf("submit: got=%v view=%s", got, renderPlain(d))
	}
	if d.HandleKey(key('a')).Kind != mcpActionNone || !d.Busy() {
		t.Fatal("keys accepted while finishing")
	}
}

func TestMCPManagerPasteViewFitsNarrowWidth(t *testing.T) {
	d := managerFixture(mcp.ServerInfo{Name: "secure", State: mcp.StateSignIn, OAuth: true, Source: "/home/u/mcp.json"})
	d.HandleKey(key('l'))
	d.SetAuthURL("https://auth.example.test/authorize", func(string) error {
		return errors.New("this URL belongs to another sign-in, copy the address from the latest browser tab")
	})
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "http://127.0.0.1:1/callback?code=" + strings.Repeat("c", 80)})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	for _, line := range d.Render(tui.Dark, 40) {
		for _, row := range strings.Split(line, "\n") {
			plain := plainMCPRow(row)
			if w := runewidth.StringWidth(plain); w > 40 {
				t.Fatalf("row width %d > 40: %q", w, plain)
			}
		}
	}
	if _, col := d.CursorPos(); col >= 40 {
		t.Fatalf("caret outside the dialog: %d", col)
	}
}
