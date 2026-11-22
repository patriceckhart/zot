package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestHyperlinkWidthAndTruncation(t *testing.T) {
	url := "https://example.test/authorize?state=" + strings.Repeat("s", 200)
	for _, terminator := range []string{"\x1b\\", "\x07"} {
		open := "\x1b]8;;" + url + terminator
		close := "\x1b]8;;" + terminator
		link := open + "Open 界 link" + close
		if got := visibleWidth(link); got != 12 {
			t.Fatalf("visibleWidth=%d, want 12", got)
		}
		for _, tc := range []struct {
			width int
			label string
		}{{40, "Open 界 link"}, {7, "Open 界"}, {1, "O"}} {
			if got, want := truncateToWidth(link, tc.width), open+tc.label+close; got != want {
				t.Fatalf("width %d: got %q, want %q", tc.width, got, want)
			}
		}
	}
}

func TestRendererPreservesHyperlink(t *testing.T) {
	url := "https://example.test/authorize?state=" + strings.Repeat("s", 200)
	link := Hyperlink(url, "Click here to open")
	for _, logMode := range []bool{false, true} {
		var output bytes.Buffer
		r := NewRenderer(&output)
		r.Resize(24, 8)
		if logMode {
			r.DrawLog(nil, []string{link}, -1, 0)
		} else {
			r.Draw([]string{link}, -1, 0)
		}
		if !strings.Contains(output.String(), link) {
			t.Fatalf("log=%v: renderer damaged hyperlink: %q", logMode, output.String())
		}
	}
}

func TestHyperlinkRefusesControlCharacters(t *testing.T) {
	for _, target := range []string{"", "https://example.test/\x1b]8;;bad", "https://example.test/\n", "https://example.test/\x07", "https://example.test/\u009c"} {
		if got := Hyperlink(target, "open"); got != "open" {
			t.Fatalf("unsafe target emitted: %q", got)
		}
	}
}
