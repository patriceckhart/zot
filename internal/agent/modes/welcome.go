package modes

import "github.com/patriceckhart/zot/internal/tui"

// welcomeBanner returns the intro text shown at the top of an empty chat.
// It uses the `zot` label color (same as the assistant) for consistency.
//
// When version is non-empty AND showVersion is true, the headline
// reads "i'm zot (vX.Y.Z). ..." so users see which build they're on
// the moment zot starts. After welcomeVersionDuration the caller
// flips showVersion off and the headline reverts to plain text.
func welcomeBanner(th tui.Theme, version string, showVersion bool) []string {
	headline := "▌ i'm zot. yet another coding agent harness."
	if showVersion && version != "" {
		headline = "▌ i'm zot (" + version + "). yet another coding agent harness."
	}
	return []string{
		th.FG256(th.Assistant, tui.Bold(headline)),
		th.FG256(th.Muted, "  ask anything, or type /help to see commands."),
		"",
	}
}
