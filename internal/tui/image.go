package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"strconv"
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
//	ZOT_INLINE_IMAGES=off         -> force text fallback
//	ZOT_INLINE_IMAGES=placeholder -> force text fallback (alias for off)
//	ZOT_INLINE_IMAGES=iterm       -> force iTerm2 protocol
//	ZOT_INLINE_IMAGES=kitty       -> force Kitty protocol
//	ZOT_INLINE_IMAGES=auto        -> explicit auto-detect (same as default)
func DetectImageProtocol() ImageProtocol {
	switch strings.ToLower(os.Getenv("ZOT_INLINE_IMAGES")) {
	case "off", "none", "false", "0", "placeholder", "text":
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

	// VS Code's integrated terminal exposes several protocol-ish env
	// combinations depending on the underlying shell/pty, but its image
	// layer is inconsistent enough that auto-enable is more annoying than
	// useful. Users can still force a protocol via ZOT_INLINE_IMAGES.
	if strings.EqualFold(termProgram, "vscode") {
		return ImageProtocolNone
	}

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
	sb.WriteString(";preserveAspectRatio=1")
	// doNotMoveCursor=1 keeps the cursor at the start of the image
	// after rendering. We need that so the caller can pad with spaces
	// and draw the box's closing │ at a fixed terminal column without
	// having to know how many cells the image actually occupied (the
	// rendered width depends on the terminal's cell aspect ratio).
	sb.WriteString(";doNotMoveCursor=1:")
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
	//
	// C=1 tells kitty/ghostty to not move the cursor after rendering
	// the image. We need that so the caller can pad with spaces and
	// draw the box's closing │ at a fixed terminal column without
	// having to know how many cells the image actually occupied (the
	// rendered width depends on the terminal's cell aspect ratio,
	// which differs across fonts/terminals).
	hdr := "a=T,f=100,C=1"
	if maxCellsWide > 0 && maxCellsHigh > 0 {
		if pxW, pxH := ImageDimensions(data); pxW > 0 && pxH > 0 {
			// rows that the native width would produce at maxCellsWide.
			// Round up so we don't under-reserve and let following text
			// paint over the image on terminals whose cell metrics differ.
			nativeRows := int(math.Ceil(float64(pxH) * float64(maxCellsWide) / float64(pxW) / CellAspectRatio()))
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

// defaultCellAspectRatio approximates how many pixel-rows one terminal
// row occupies. Typical monospace cells are ~2x tall as wide; we use 2.0
// as a safe default. Used to compute the rendered row count when scaling
// an image to fit a cell width.
const defaultCellAspectRatio = 2.0

// CellAspectRatio returns the pixel-height / pixel-width ratio for one
// terminal cell. ZOT_CELL_ASPECT lets users tune inline-image row
// reservation for terminals/fonts where the default causes overlap or
// excessive blank space. Values outside a sane range are ignored.
func CellAspectRatio() float64 {
	v := strings.TrimSpace(os.Getenv("ZOT_CELL_ASPECT"))
	if v == "" {
		return defaultCellAspectRatio
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0.5 || f > 5 {
		return defaultCellAspectRatio
	}
	return f
}

// RowsForInlineImage returns the number of terminal rows an image
// rendered at cellsWide columns will occupy, preserving aspect ratio.
// Clamped to maxRows. Returns 0 if the image cannot be decoded.
func RowsForInlineImage(data []byte, cellsWide, maxRows int) int {
	rows, _ := InlineImageFootprint(data, cellsWide, maxRows)
	return rows
}

// InlineImageFootprint returns the rendered (rows, cells) footprint
// of an image at the requested cellsWide / maxRows budget,
// preserving aspect ratio. When the image's natural height exceeds
// maxRows, the rendered width shrinks below cellsWide so the
// aspect ratio is preserved within the row clamp; callers wrapping
// the image in a frame need that actual width to place a closing
// border at the right column.
func InlineImageFootprint(data []byte, cellsWide, maxRows int) (int, int) {
	w, h := ImageDimensions(data)
	if w <= 0 || h <= 0 || cellsWide <= 0 {
		return 0, 0
	}
	scaleX := float64(w) / float64(cellsWide)
	rowsF := float64(h) / (scaleX * CellAspectRatio())
	rows := int(math.Ceil(rowsF))
	if rows < 1 {
		rows = 1
	}
	actualCells := cellsWide
	if maxRows > 0 && rows > maxRows {
		rows = maxRows
		// Image is constrained by height; recompute the width so
		// aspect ratio is preserved. cellsHigh = maxRows; cellsWide
		// = pxW * maxRows * CellAspectRatio / pxH.
		newCells := int(math.Floor(float64(w)*float64(maxRows)*CellAspectRatio()/float64(h) + 0.5))
		if newCells < 1 {
			newCells = 1
		}
		if newCells > cellsWide {
			newCells = cellsWide
		}
		actualCells = newCells
	}
	return rows, actualCells
}
