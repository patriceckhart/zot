package tui

import (
	"strings"
	"testing"
)

func TestStatusBarCompactionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		completed, fallbacks, failed int
		want                         string
	}{
		{4, 1, 0, "compactions: 4, 1 side fallback"},
		{4, 2, 1, "compactions: 4, 2 side fallbacks, 1 failed"},
		{0, 0, 1, "compactions: 0, 1 failed"},
		{0, 1, 0, "compactions: 0, 1 side fallback"},
	} {
		for _, cols := range []int{200, 40, 20} {
			lines := StatusBar(StatusBarParams{Theme: Dark, Compactions: tc.completed, CompactionFallbacks: tc.fallbacks, FailedCompactions: tc.failed, Cols: cols})
			// Wrapping may split the label. Check the exact text at full width
			// and ensure every rendered row fits at narrow widths.
			if cols == 200 && !strings.Contains(strings.Join(lines, "\n"), tc.want) {
				t.Fatalf("missing %q: %q", tc.want, lines)
			}
			for _, line := range lines {
				if visibleWidth(line) > cols {
					t.Fatalf("line exceeds width %d: %q", cols, line)
				}
			}
		}
	}
}

func TestStatusBarCompactionCount(t *testing.T) {
	for _, cols := range []int{200, 40, 20} {
		for _, count := range []int{0, 1, 12} {
			lines := StatusBar(StatusBarParams{Theme: Dark, Provider: "test", Model: "model", Compactions: count, Cols: cols})
			text := strings.Join(lines, "\n")
			if strings.Contains(text, "compactions:") != (count > 0) {
				t.Fatalf("count %d, width %d: %q", count, cols, text)
			}
			if count == 12 && !strings.Contains(text, "compactions: 12") {
				t.Fatalf("missing count: %q", text)
			}
			for _, line := range lines {
				if visibleWidth(line) > cols {
					t.Fatalf("line exceeds width %d: %q", cols, line)
				}
			}
		}
	}
}
