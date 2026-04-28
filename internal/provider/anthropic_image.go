package provider

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"strings"

	xdraw "golang.org/x/image/draw"

	// Register decoders for the formats we want to accept on input.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// anthMaxImageSide is Anthropic's per-image dimension cap when a
// request contains more than one image. Single-image requests are
// allowed up to 8000 px, but zot conversations routinely include
// multiple images (a screenshot here, a tool result image there)
// so we always normalise down to the stricter limit. 2000 px on the
// longest side is well below their cap and still readable for the
// kinds of screenshots / charts the model usually consumes.
const anthMaxImageSide = 2000

// anthShrinkImageBytesIfTooBig returns data unchanged when the image
// already fits within Anthropic's per-image dimension cap. When it
// doesn't, the image is decoded, resampled with Catmull-Rom (a good
// balance of quality and speed for downscaling), and re-encoded in
// the same format. The MIME type may be rewritten when re-encoding
// requires a format change (e.g. an image originally tagged image/gif
// is shipped as image/png after resize, since stdlib only encodes
// gif at the package's own narrow API which is awkward for a single
// frame).
//
// Errors during decode/encode return the original bytes untouched so
// the caller's existing flow continues to work; Anthropic will then
// emit its own clearer error than we could synthesise. We log no
// failures here because this runs on every outbound request and is
// best-effort.
func anthShrinkImageBytesIfTooBig(data []byte, mime string) ([]byte, string) {
	if len(data) == 0 {
		return data, mime
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, mime
	}
	if cfg.Width <= anthMaxImageSide && cfg.Height <= anthMaxImageSide {
		return data, mime
	}
	// Compute target dimensions preserving aspect ratio, longest side
	// clamped to anthMaxImageSide.
	tw, th := cfg.Width, cfg.Height
	if tw >= th {
		th = th * anthMaxImageSide / tw
		tw = anthMaxImageSide
	} else {
		tw = tw * anthMaxImageSide / th
		th = anthMaxImageSide
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mime
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)

	var buf bytes.Buffer
	switch strings.ToLower(format) {
	case "jpeg":
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
			return data, mime
		}
		return buf.Bytes(), "image/jpeg"
	case "png", "gif":
		// gifs are re-encoded as png so the call site stays simple
		// (avoiding a single-frame Encode helper). Anthropic accepts
		// image/png happily; the visual result is the same for the
		// model's vision pipeline.
		if err := png.Encode(&buf, dst); err != nil {
			return data, mime
		}
		nextMime := mime
		if strings.ToLower(format) == "gif" {
			nextMime = "image/png"
		}
		return buf.Bytes(), nextMime
	default:
		return data, mime
	}
}
