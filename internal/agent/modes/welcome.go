package modes

import "github.com/patriceckhart/zot/internal/tui"

// welcomeBanner returns the intro text shown at the top of an empty chat.
// It uses the `zot` label color (same as the assistant) for consistency.
func welcomeBanner(th tui.Theme) []string {
	return []string{
		th.FG256(th.Assistant, tui.Bold("▌ i'm zot. yet another coding agent harness.")),
		th.FG256(th.Muted, "  ask anything, or type /help to see commands."),
		"",
	}
}
