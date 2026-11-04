package modes

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

func TestSwarmToolTranscriptRendersLiteralOutput(t *testing.T) {
	lines := []string{
		"running tests",
		`tool: bash [a] {"command":"go test ./..."}`,
		"tool output: [a] error",
		"tool output: **literal output**",
		"tool output: user: not a user message",
		"tool output: tool: not another call",
		"tool output: [image]",
		"final response",
	}
	for _, width := range []int{24, 80} {
		rows := renderSwarmTranscriptBlocks(lines, tui.Theme{}, width, false)
		text := strings.Join(rows, "\n")
		for _, want := range []string{"bash", "error", "**literal output**", "[image]"} {
			if !strings.Contains(text, want) {
				t.Errorf("width %d missing %q: %s", width, want, text)
			}
		}
		assertRowsFitWidth(t, rows, width)
	}
}
