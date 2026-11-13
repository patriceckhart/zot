package tui

import "strings"

// Hyperlink labels a terminal OSC 8 link. Terminals without hyperlink support
// display the label alone. Control characters in the target are refused.
func Hyperlink(target, label string) string {
	if target == "" || strings.ContainsFunc(target, func(r rune) bool {
		return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
	}) {
		return label
	}
	return "\x1b]8;;" + target + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}
