package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestMouseSelectionVisibleText(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end mouseCell
		want       string
	}{
		{"forward", mouseCell{0, 1}, mouseCell{0, 4}, "ell"},
		{"backward", mouseCell{0, 4}, mouseCell{0, 1}, "ell"},
		{"multiline", mouseCell{0, 3}, mouseCell{1, 3}, "lo\nwor"},
		{"multiline backward", mouseCell{1, 3}, mouseCell{0, 3}, "lo\nwor"},
		{"wide and combining", mouseCell{2, 0}, mouseCell{2, 4}, "a界e\u0308"},
		{"inside wide", mouseCell{2, 2}, mouseCell{2, 3}, "界"},
		{"click", mouseCell{0, 1}, mouseCell{0, 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := NewRenderer(&out)
			r.SetTheme(Dark)
			r.Resize(20, 10)
			r.DrawLog([]string{"\x1b[31mhello\x1b[0m   ", "world", "a界e\u0308b"}, []string{"input"}, 0, 0)
			r.BeginMouseSelection(tc.start.row, tc.start.col)
			out.Reset()
			r.DragMouseSelection(tc.end.row, tc.end.col)
			if tc.want != "" && !strings.Contains(out.String(), Dark.SelectionStyle()) {
				t.Fatal("drag did not highlight selected text")
			}
			if strings.Contains(out.String(), SeqClearScrollback) || strings.Contains(out.String(), SeqClearScreen) {
				t.Fatal("selection cleared native scrollback or screen")
			}
			text, dragged := r.EndMouseSelection(tc.end.row, tc.end.col)
			if text != tc.want || dragged != (tc.name != "click") {
				t.Fatalf("selection = %q, dragged = %v, want %q", text, dragged, tc.want)
			}
			if r.MouseSelectionActive() != dragged || r.MouseSelectionDragging() {
				t.Fatal("release did not preserve a finished drag or clear a stationary click")
			}
			if dragged {
				if copied, ok := r.MouseSelectionText(); !ok || copied != tc.want {
					t.Fatal("finished selection text was not retained")
				}
			}
		})
	}
}

func TestMouseSelectionFreezesStreamingAndResumes(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out)
	r.Resize(20, 5)
	r.DrawLog([]string{"before"}, []string{"input"}, 0, 0)
	r.BeginMouseSelection(0, 0)
	out.Reset()
	r.DrawLog([]string{"replacement", "new output"}, []string{"input"}, 0, 0)
	if out.Len() != 0 {
		t.Fatal("streaming moved the selection snapshot")
	}
	text, dragged := r.EndMouseSelection(0, 6)
	if text != "before" || !dragged {
		t.Fatalf("selection = %q, dragged = %v", text, dragged)
	}
	out.Reset()
	r.DrawLog([]string{"replacement", "new output"}, []string{"input"}, 0, 0)
	if out.Len() != 0 || !r.MouseSelectionActive() {
		t.Fatal("redraw removed the persistent selection")
	}
	r.ClearMouseSelection()
	out.Reset()
	r.DrawLog([]string{"replacement", "new output"}, []string{"input"}, 0, 0)
	if !strings.Contains(out.String(), "new output") {
		t.Fatal("streaming did not resume after clearing selection")
	}
}

func TestMouseSelectionHistoryAndResize(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out)
	r.Resize(20, 5)
	r.DrawHistory([]string{"history"}, []string{"input"}, 0, 0)
	r.BeginMouseSelection(3, 0)
	text, _ := r.EndMouseSelection(3, 7)
	if text != "history" {
		t.Fatalf("history selection = %q", text)
	}
	r.BeginMouseSelection(3, 0)
	r.DragMouseSelection(3, 4)
	r.Resize(10, 5)
	if r.MouseSelectionActive() {
		t.Fatal("resize did not cancel selection")
	}
	if text, _ := r.EndMouseSelection(3, 7); text != "" {
		t.Fatal("release after resize copied stale text")
	}
}

func TestSelectionLineExcludesControlPayloads(t *testing.T) {
	line := "\x1b]8;;https://example.invalid/private\x1b\\hello\x1b]8;;\x1b\\\x1b[0m"
	_, text := selectionLine(line, 0, 5, Dark)
	if text != "hello" {
		t.Fatalf("copied hyperlink payload: %q", text)
	}
}
