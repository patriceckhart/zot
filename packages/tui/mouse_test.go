package tui

import (
	"bytes"
	"testing"
)

func TestReaderMousePress(t *testing.T) {
	for _, seq := range []string{"\x1b[<0;12;7M", "\x1b[<4;12;7M"} {
		k := readKey(t, seq)
		if k.Kind != KeyMouseLeft || k.MouseX != 12 || k.MouseY != 7 {
			t.Fatalf("unexpected click: %+v", k)
		}
	}
	for _, seq := range []string{
		"\x1b[<2;12;7M", "\x1b[<33;12;7M", "\x1b[<32;12;7m", "\x1b[<64;12;7m",
		"\x1b[<0;0;7M", "\x1b[<0;12;-1M", "\x1b[<0;bad;7M", "\x1b[<0;12M",
	} {
		if k := readKey(t, seq); k.Kind != KeyUnknown {
			t.Fatalf("invalid or non-left press %q: %+v", seq, k)
		}
	}
}

func TestReaderMouseDragAndRelease(t *testing.T) {
	for _, tc := range []struct {
		seq  string
		kind KeyKind
	}{
		{"\x1b[<32;12;7M", KeyMouseDrag},
		{"\x1b[<0;12;7m", KeyMouseRelease},
		{"\x1b[<4;12;7m", KeyMouseRelease},
	} {
		k := readKey(t, tc.seq)
		if k.Kind != tc.kind || k.MouseX != 12 || k.MouseY != 7 {
			t.Fatalf("Read(%q) = %+v", tc.seq, k)
		}
	}
}

func TestEditorClickPosition(t *testing.T) {
	cases := []struct {
		name, text                    string
		width, row, col, wantR, wantC int
	}{
		{"plain", "hello", 20, 0, 4, 0, 2},
		{"prompt", "hello", 20, 0, 0, 0, 0},
		{"past end", "hello", 20, 0, 19, 0, 5},
		{"empty", "", 20, 0, 2, 0, 0},
		{"multiline", "hello\nworld", 20, 1, 4, 1, 2},
		{"word wrap", "hello world", 9, 1, 4, 0, 8},
		{"hard wrap", "abcdefghijk", 8, 1, 4, 0, 8},
		{"hard wrap start", "abcdefghijk", 8, 1, 2, 0, 6},
		{"wide", "a界b", 20, 0, 4, 0, 1},
		{"after wide", "a界b", 20, 0, 5, 0, 2},
		{"combining", "a\u0308b", 20, 0, 3, 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEditor("\x1b[31m> \x1b[0m")
			e.SetValue(tc.text)
			e.Render(tc.width)
			if !e.MoveCursorToVisual(tc.row, tc.col) || e.CursorR != tc.wantR || e.CursorC != tc.wantC {
				t.Fatalf("cursor = (%d,%d), want (%d,%d)", e.CursorR, e.CursorC, tc.wantR, tc.wantC)
			}
			if tc.name == "hard wrap start" {
				_, row, col := e.Render(tc.width)
				if row != tc.row || col != tc.col {
					t.Fatalf("rendered cursor = (%d,%d), want clicked cell (%d,%d)", row, col, tc.row, tc.col)
				}
			}
			r, c := e.CursorR, e.CursorC
			if e.MoveCursorToVisual(100, 0) || e.MoveCursorToVisual(-1, 0) || e.CursorR != r || e.CursorC != c {
				t.Fatal("outside click changed cursor")
			}
		})
	}
}

func TestRendererBottomScreenRow(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out)
	r.Resize(80, 10)
	r.DrawLog([]string{"chat"}, []string{"status", "input"}, 1, 0)
	if got := r.BottomScreenRow(1); got != 2 {
		t.Fatalf("short log input row = %d, want 2", got)
	}
	r.DrawLog(make([]string, 20), []string{"status", "input"}, 1, 0)
	// DrawLog reserves a blank bottom margin below the input.
	if got := r.BottomScreenRow(1); got != 8 {
		t.Fatalf("scrolled input row = %d, want 8", got)
	}
	r.DrawHistory([]string{"chat"}, []string{"status", "input", "below"}, 1, 0)
	if got := r.BottomScreenRow(1); got != 8 {
		t.Fatalf("history input row = %d, want 8", got)
	}
}
