package tui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRowsForInlineImageRoundsUp(t *testing.T) {
	t.Setenv("ZOT_CELL_ASPECT", "")
	data := testPNG(t, 100, 51)
	got := RowsForInlineImage(data, 10, 0)
	// 51px high at 10 cells wide with a 2.0 cell aspect is 2.55 rows.
	// Rounding down to 2 lets following text overlap; we need 3.
	if got != 3 {
		t.Fatalf("RowsForInlineImage = %d, want 3", got)
	}
}

func TestRowsForInlineImageCellAspectOverride(t *testing.T) {
	data := testPNG(t, 100, 100)
	t.Setenv("ZOT_CELL_ASPECT", "1")
	if got := RowsForInlineImage(data, 10, 0); got != 10 {
		t.Fatalf("aspect=1 rows = %d, want 10", got)
	}
	t.Setenv("ZOT_CELL_ASPECT", "4")
	if got := RowsForInlineImage(data, 10, 0); got != 3 {
		t.Fatalf("aspect=4 rows = %d, want 3", got)
	}
}

func TestDetectImageProtocolPlaceholderAndVSCode(t *testing.T) {
	t.Setenv("ZOT_INLINE_IMAGES", "placeholder")
	if got := DetectImageProtocol(); got != ImageProtocolNone {
		t.Fatalf("placeholder protocol = %v, want none", got)
	}

	t.Setenv("ZOT_INLINE_IMAGES", "")
	t.Setenv("TERM_PROGRAM", "vscode")
	t.Setenv("KITTY_WINDOW_ID", "1")
	if got := DetectImageProtocol(); got != ImageProtocolNone {
		t.Fatalf("vscode auto protocol = %v, want none", got)
	}

	t.Setenv("ZOT_INLINE_IMAGES", "kitty")
	if got := DetectImageProtocol(); got != ImageProtocolKitty {
		t.Fatalf("forced kitty protocol = %v, want kitty", got)
	}
}
