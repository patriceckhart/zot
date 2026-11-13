package modes

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/patriceckhart/zot/packages/mcp"
	"github.com/patriceckhart/zot/packages/tui"
)

func longAuthDialog() (*mcpManagerDialog, string) {
	d := managerFixture(mcp.ServerInfo{Name: "secure", State: mcp.StateSignIn, OAuth: true})
	d.HandleKey(key('l'))
	url := "https://auth.example.test/authorize?client_id=" + strings.Repeat("c", 60) +
		"&redirect_uri=http%3A%2F%2F127.0.0.1%3A12345%2Fcallback&state=" + strings.Repeat("s", 64) +
		"&code_challenge=" + strings.Repeat("p", 64) + "&code_challenge_method=S256"
	d.SetAuthURL(url, func(string) error { return nil })
	return d, url
}

func TestMCPManagerWrapsFullAuthURL(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		d, url := longAuthDialog()
		var recovered strings.Builder
		inURL := false
		for _, line := range d.Render(tui.Dark, width) {
			for _, row := range strings.Split(line, "\n") {
				plain := plainMCPRow(row)
				if w := runewidth.StringWidth(plain); w > width {
					t.Fatalf("width %d: row width %d: %q", width, w, plain)
				}
				if strings.HasPrefix(plain, "  https://") {
					inURL = true
				}
				if inURL && !strings.Contains(row, "\x1b]8;") {
					recovered.WriteString(strings.TrimPrefix(plain, "  "))
				}
				if strings.Contains(row, "\x1b]8;") {
					inURL = false
					if !strings.Contains(row, "\x1b]8;;"+url+"\x1b\\") {
						t.Fatal("hyperlink lost the full target")
					}
				}
			}
		}
		if recovered.String() != url {
			t.Fatalf("width %d: recovered %q, want %q", width, recovered.String(), url)
		}
	}
}

func TestMCPManagerAuthScrollAndPasteCaret(t *testing.T) {
	const width, height = 40, 6
	d, _ := longAuthDialog()
	full := padDialogFrame(d.Render(tui.Dark, width))
	var expected []string
	for _, line := range full {
		expected = append(expected, strings.Split(line, "\n")...)
	}
	render := func(rows int) []string {
		t.Helper()
		lines := padDialogFrame(d.Render(tui.Dark, width))
		fitted := d.fitAuthRows(tui.Dark, width, rows, lines)
		if len(fitted) > rows {
			t.Fatalf("height %d: got %d rows", rows, len(fitted))
		}
		return fitted
	}
	seen := map[string]bool{}
	for range len(expected) {
		for _, row := range render(height) {
			seen[row] = true
		}
		d.HandleKey(tui.Key{Kind: tui.KeyDown})
	}
	for _, row := range expected {
		if !seen[row] {
			t.Fatalf("row unreachable by scrolling: %q", row)
		}
	}
	for range len(expected) {
		d.HandleKey(tui.Key{Kind: tui.KeyUp})
	}
	render(height)
	if d.authScroll != 0 {
		t.Fatalf("up did not reach start: %d", d.authScroll)
	}
	if row, _ := d.CursorPos(); row >= 0 {
		t.Fatal("offscreen paste field has a visible caret")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "http://127.0.0.1:12345/callback?code=c&state=s"})
	render(height)
	if row, col := d.CursorPos(); row < 0 || row >= height-1 || col >= width {
		t.Fatalf("editing did not reveal caret: row=%d col=%d", row, col)
	}
	// A taller terminal removes scrolling without losing the URL or caret.
	render(100)
	if d.authScroll != 0 || d.authScrollMax != 0 {
		t.Fatal("resize did not reset the viewport")
	}
	// A single available row still exposes content rather than just a hint.
	if got := render(1); len(got) != 1 || strings.Contains(plainMCPRow(got[0]), "up/down") {
		t.Fatalf("one-row viewport: %q", got)
	}
}
