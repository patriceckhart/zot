package tui

import "strings"

// WrapStyledANSILine wraps like WrapANSILine, but makes each row independently
// renderable by replaying prior SGR sequences and ending with a style reset.
// Only CSI SGR styling is carried across rows, not other terminal controls.
func WrapStyledANSILine(s string, limit int) []string {
	rows := WrapANSILine(s, limit)
	var styles strings.Builder
	for i, row := range rows {
		prefix := styles.String()
		for pos := 0; pos < len(row); pos++ {
			if row[pos] != '\x1b' || pos+1 >= len(row) || row[pos+1] != '[' {
				continue
			}
			end := pos + 2
			for end < len(row) && (row[end] < 0x40 || row[end] > 0x7e) {
				end++
			}
			if end == len(row) {
				break
			}
			if row[end] == 'm' {
				seq := row[pos : end+1]
				if seq == "\x1b[m" || seq == reset {
					styles.Reset()
				} else {
					// Replay rather than interpret attributes, including partial
					// resets and extended colors whose parameters may include zero.
					styles.WriteString(seq)
				}
			}
			pos = end
		}
		rows[i] = prefix + row + reset
	}
	return rows
}
