package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"
)

// ImageProtocol describes which inline-image escape the current
// terminal understands.
type ImageProtocol int

const (
	ImageProtocolNone   ImageProtocol = iota // no inline images, use text fallback
	ImageProtocolITerm2                      // iTerm2 proprietary OSC 1337 File= (also: WezTerm)
	ImageProtocolKitty                       // Kitty graphics protocol
)

// DetectImageProtocol returns the best inline-image protocol supported
// by the current terminal, or ImageProtocolNone.
//
// The default is to auto-detect: if the terminal advertises iTerm2 or
// Kitty-graphics support, we use it. The ZOT_INLINE_IMAGES env var
// overrides the default:
//
//	ZOT_INLINE_IMAGES=off   -> force text fallback
//	ZOT_INLINE_IMAGES=iterm -> force iTerm2 protocol
//	ZOT_INLINE_IMAGES=kitty -> force Kitty protocol
//	ZOT_INLINE_IMAGES=auto  -> explicit auto-detect (same as default)
func DetectImageProtocol() ImageProtocol {
	switch strings.ToLower(os.Getenv("ZOT_INLINE_IMAGES")) {
	case "off", "none", "false", "0":
		return ImageProtocolNone
	case "iterm", "iterm2":
		return ImageProtocolITerm2
	case "kitty":
		return ImageProtocolKitty
	}
	return detectImageProtocolAuto()
}

// detectImageProtocolAuto returns the best protocol by sniffing the
// current terminal via env vars. Same detection logic as before.
func detectImageProtocolAuto() ImageProtocol {
	termProgram := os.Getenv("TERM_PROGRAM")
	term := os.Getenv("TERM")
	kittyWindow := os.Getenv("KITTY_WINDOW_ID")

	if kittyWindow != "" || strings.Contains(term, "kitty") || strings.Contains(term, "ghostty") {
		return ImageProtocolKitty
	}
	if termProgram == "ghostty" || termProgram == "kitty" {
		return ImageProtocolKitty
	}
	if termProgram == "iTerm.app" || termProgram == "WezTerm" {
		return ImageProtocolITerm2
	}
	if strings.Contains(strings.ToLower(termProgram), "ghostty") {
		return ImageProtocolKitty
	}
	return ImageProtocolNone
}

// RenderInlineImage returns a terminal escape sequence that draws data
// inline. If the protocol is None, returns "" so the caller can fall
// back to a text placeholder.
//
// maxCellsWide caps the rendered width in terminal cells (columns) for
// protocols that honor it. 0 means "let the terminal decide".
func RenderInlineImage(proto ImageProtocol, data []byte, mime string, maxCellsWide int) string {
	return RenderInlineImageScaled(proto, data, mime, maxCellsWide, 0)
}

// RenderInlineImageScaled renders an image with both width and height
// clamps (in terminal cells). Values <= 0 mean "let the terminal decide".
func RenderInlineImageScaled(proto ImageProtocol, data []byte, mime string, maxCellsWide, maxCellsHigh int) string {
	switch proto {
	case ImageProtocolITerm2:
		return renderITerm2(data, maxCellsWide, maxCellsHigh)
	case ImageProtocolKitty:
		return renderKitty(data, maxCellsWide, maxCellsHigh)
	}
	return ""
}

// renderITerm2 builds an OSC 1337 File= sequence. Works in iTerm2 and WezTerm.
//
// Reference: https://iterm2.com/documentation-images.html
func renderITerm2(data []byte, maxCellsWide, maxCellsHigh int) string {
	b64 := base64.StdEncoding.EncodeToString(data)
	var sb strings.Builder
	sb.WriteString("\x1b]1337;File=inline=1")
	if maxCellsWide > 0 {
		fmt.Fprintf(&sb, ";width=%d", maxCellsWide)
	}
	if maxCellsHigh > 0 {
		fmt.Fprintf(&sb, ";height=%d", maxCellsHigh)
	}
	sb.WriteString(";preserveAspectRatio=1:")
	sb.WriteString(b64)
	sb.WriteString("\x07")
	return sb.String()
}

// renderKitty builds a Kitty graphics protocol sequence. Supports chunked
// data via the "m" continuation flag; chunk size is 4096 to stay under
// terminal escape-buffer limits.
//
// The Kitty protocol preserves aspect ratio automatically when only one
// of c= (columns) or r= (rows) is set. Setting both causes the image to
// be stretched non-uniformly. We pick whichever constraint is tighter
// for the input image so it fits inside maxCellsWide x maxCellsHigh.
//
// Reference: https://sw.kovidgoyal.net/kitty/graphics-protocol/
func renderKitty(data []byte, maxCellsWide, maxCellsHigh int) string {
	b64 := base64.StdEncoding.EncodeToString(data)
	const chunk = 4096
	var sb strings.Builder

	// Pick the most constraining dimension and use only it. Kitty
	// preserves aspect ratio when exactly one of c/r is provided.
	hdr := "a=T,f=100"
	if maxCellsWide > 0 && maxCellsHigh > 0 {
		if pxW, pxH := ImageDimensions(data); pxW > 0 && pxH > 0 {
			// rows that the native width would produce at maxCellsWide
			nativeRows := int(float64(pxH) * float64(maxCellsWide) / float64(pxW) / CellAspectRatio)
			if nativeRows > maxCellsHigh {
				hdr += fmt.Sprintf(",r=%d", maxCellsHigh)
			} else {
				hdr += fmt.Sprintf(",c=%d", maxCellsWide)
			}
		} else {
			hdr += fmt.Sprintf(",c=%d", maxCellsWide)
		}
	} else if maxCellsWide > 0 {
		hdr += fmt.Sprintf(",c=%d", maxCellsWide)
	} else if maxCellsHigh > 0 {
		hdr += fmt.Sprintf(",r=%d", maxCellsHigh)
	}

	for i := 0; i < len(b64); i += chunk {
		end := i + chunk
		if end > len(b64) {
			end = len(b64)
		}
		more := 1
		if end == len(b64) {
			more = 0
		}
		if i == 0 {
			fmt.Fprintf(&sb, "\x1b_G%s,m=%d;%s\x1b\\", hdr, more, b64[i:end])
		} else {
			fmt.Fprintf(&sb, "\x1b_Gm=%d;%s\x1b\\", more, b64[i:end])
		}
	}
	return sb.String()
}

// ImageDimensions returns width and height in pixels, or zeros on error.
// Used for the text fallback so the user sees something useful.
func ImageDimensions(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// CellAspectRatio approximates how many pixel-rows one terminal row
// occupies. Typical monospace cells are ~2x tall as wide; we use 2.0
// as a safe default. Used to compute the rendered row count when
// scaling an image to fit a cell width.
const CellAspectRatio = 2.0

// RowsForInlineImage returns the number of terminal rows an image
// rendered at cellsWide columns will occupy, preserving aspect ratio.
// Clamped to maxRows. Returns 0 if the image cannot be decoded.
func RowsForInlineImage(data []byte, cellsWide, maxRows int) int {
	w, h := ImageDimensions(data)
	if w <= 0 || h <= 0 || cellsWide <= 0 {
		return 0
	}
	// pixels per cell (horizontal)
	// scaleX = imageWidthPx / cellsWide
	// rendered height in cells = imageHeightPx / (scaleX * CellAspectRatio)
	scaleX := float64(w) / float64(cellsWide)
	rows := int(float64(h) / (scaleX * CellAspectRatio))
	if rows < 1 {
		rows = 1
	}
	if maxRows > 0 && rows > maxRows {
		rows = maxRows
	}
	return rows
}
