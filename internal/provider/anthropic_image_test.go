package provider

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func makeRect(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Fill with a recognisable colour so a buggy resize that goes
	// transparent or zero-size is obvious.
	c := color.RGBA{R: 80, G: 200, B: 120, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func decodeConfig(t *testing.T, data []byte) image.Config {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

func TestAnthShrinkImage_PassesThroughWhenSmall(t *testing.T) {
	src := encodePNG(t, makeRect(800, 600))
	out, mime := anthShrinkImageBytesIfTooBig(src, "image/png")
	if !bytes.Equal(out, src) {
		t.Errorf("small image was rewritten; expected pass-through")
	}
	if mime != "image/png" {
		t.Errorf("mime changed unexpectedly: %s", mime)
	}
}

func TestAnthShrinkImage_DownscalesWhenTooWide(t *testing.T) {
	src := encodePNG(t, makeRect(4000, 1000))
	out, mime := anthShrinkImageBytesIfTooBig(src, "image/png")
	if bytes.Equal(out, src) {
		t.Fatalf("image was not resized")
	}
	if mime != "image/png" {
		t.Errorf("mime changed: %s", mime)
	}
	cfg := decodeConfig(t, out)
	if cfg.Width != anthMaxImageSide {
		t.Errorf("width: got %d want %d", cfg.Width, anthMaxImageSide)
	}
	// Aspect ratio preserved: 4000:1000 -> 2000:500.
	if cfg.Height != 500 {
		t.Errorf("height: got %d want 500", cfg.Height)
	}
}

func TestAnthShrinkImage_DownscalesWhenTooTall(t *testing.T) {
	src := encodePNG(t, makeRect(1500, 6000))
	out, _ := anthShrinkImageBytesIfTooBig(src, "image/png")
	cfg := decodeConfig(t, out)
	if cfg.Height != anthMaxImageSide {
		t.Errorf("height: got %d want %d", cfg.Height, anthMaxImageSide)
	}
	// 1500:6000 -> 500:2000.
	if cfg.Width != 500 {
		t.Errorf("width: got %d want 500", cfg.Width)
	}
}

func TestAnthShrinkImage_PreservesJPEGFormat(t *testing.T) {
	src := encodeJPEG(t, makeRect(3000, 2500))
	out, mime := anthShrinkImageBytesIfTooBig(src, "image/jpeg")
	if mime != "image/jpeg" {
		t.Errorf("mime should stay image/jpeg, got %s", mime)
	}
	cfg := decodeConfig(t, out)
	if cfg.Width > anthMaxImageSide || cfg.Height > anthMaxImageSide {
		t.Errorf("dimensions exceed cap: %dx%d", cfg.Width, cfg.Height)
	}
}

func TestAnthShrinkImage_BadDataReturnsOriginal(t *testing.T) {
	src := []byte("not an image at all")
	out, mime := anthShrinkImageBytesIfTooBig(src, "image/png")
	if !bytes.Equal(out, src) {
		t.Errorf("garbage input was mutated")
	}
	if mime != "image/png" {
		t.Errorf("mime was changed on bad input: %s", mime)
	}
}
