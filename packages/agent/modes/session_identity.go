package modes

import (
	"context"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"github.com/patriceckhart/zot/packages/tui"
)

type sessionIdentity struct {
	ID    string
	Alias string
}

func (i *Interactive) currentSessionIdentity() sessionIdentity {
	if i.cfg.CurrentSessionIdentity == nil {
		return sessionIdentity{}
	}
	id, alias := i.cfg.CurrentSessionIdentity()
	return sessionIdentity{ID: id, Alias: alias}
}

// renderSessionIdentity wraps rather than truncates the UUID so it remains
// inspectable on narrow terminals. Metadata from session files is untrusted.
func renderSessionIdentity(th tui.Theme, width int, session sessionIdentity) []string {
	if width < 1 {
		return nil
	}
	text := "session ID: " + session.ID
	if session.ID == "" {
		text = "no active session"
	} else if session.Alias != "" {
		text += "  alias: " + session.Alias
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	rows := tui.WrapANSILine(text, width)
	for n, row := range rows {
		rows[n] = th.FG256(th.Muted, runewidth.Truncate(row, width, ""))
	}
	return rows
}

// copySessionID accepts the clipboard writer so tests never touch the user's
// clipboard. Local clipboard errors are reported, not claimed as successes.
func (i *Interactive) copySessionID(ctx context.Context, write func(context.Context, string) error) {
	session := i.currentSessionIdentity()
	var notice, failure string
	if session.ID == "" {
		failure = "copy session ID: no active session (running with --no-session?)"
	} else if err := write(ctx, session.ID); err != nil {
		failure = "copy session ID failed: " + err.Error()
	} else {
		notice = "session ID copied to clipboard"
	}
	i.mu.Lock()
	i.statusOK, i.statusErr = notice, failure
	i.mu.Unlock()
	i.invalidate()
}
