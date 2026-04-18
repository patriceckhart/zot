// Package assets holds static resources embedded in the zot binary.
// Currently just the zot logo used by the tui welcome banner.
package assets

import _ "embed"

// LogoPNG is the pixel-art zot `z` logo as PNG bytes.
// Used by the interactive welcome banner; decoded once and rasterized
// to Unicode half-blocks so it renders on any terminal without needing
// inline image support.
//
//go:embed zot-logo.png
var LogoPNG []byte
