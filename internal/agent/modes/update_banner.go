package modes

import (
	"fmt"
	"runtime"

	"github.com/patriceckhart/zot/internal/tui"
)

// UpdateInfo mirrors agent.UpdateInfo without the import cycle. The
// parent package builds one of these via agent.CheckForUpdate and
// passes it in through InteractiveConfig.UpdateInfoChan.
type UpdateInfo struct {
	Current   string
	Latest    string
	Available bool
	URL       string
}

// renderUpdateBanner builds the "new version available" block shown at
// the top of the chat area. Yellow-framed like a warning, but worded
// gently since this is informational, not urgent.
//
// Returns nil when no update is available, so callers can just
// append (or prepend) unconditionally.
func renderUpdateBanner(th tui.Theme, info UpdateInfo, width int) []string {
	if !info.Available {
		return nil
	}
	color := th.Warning
	out := []string{
		frameHeaderColor(th, "update available", width, color),
	}

	title := fmt.Sprintf("zot %s is available (you're on %s).", info.Latest, info.Current)
	out = append(out, "  "+th.FG256(color, tui.Bold(title)))

	cmd := recommendedUpdateCommand()
	out = append(out, "  "+th.FG256(th.Muted, "run: ")+th.FG256(th.ToolOut, cmd))

	if info.URL != "" {
		out = append(out, "  "+th.FG256(th.Muted, "changelog: "+info.URL))
	}

	out = append(out, frameRuleColor(th, width, color))
	out = append(out, "")
	return out
}

// recommendedUpdateCommand returns the one-liner appropriate for the
// user's platform. Points at www.zot.sh, which 301s to
// the raw install scripts on github — same surface, stable short
// URL that's safe to hard-code even if the scripts later move.
func recommendedUpdateCommand() string {
	if runtime.GOOS == "windows" {
		return `iwr -useb https://www.zot.sh/install.ps1 | iex`
	}
	return `curl -fsSL https://www.zot.sh/install.sh | bash`
}
