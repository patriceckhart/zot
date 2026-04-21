package modes

import (
	"fmt"
	"path/filepath"

	"github.com/patriceckhart/zot/internal/auth"
	"github.com/patriceckhart/zot/internal/tui"
)

// loginStep is the current node in the login dialog state machine.
type loginStep int

// loginStepClosed is the zero value on purpose: a freshly-constructed
// dialog must default to closed so nothing shows up until Open() is
// explicitly called.
const (
	loginStepClosed   loginStep = iota
	loginStepMethod             // pick apikey vs subscription
	loginStepProvider           // pick anthropic vs openai
	loginStepWaiting            // browser open, waiting for callback
	loginStepDone               // success or error, waiting for key to dismiss
)

// loginDialog is a tiny inline dialog rendered above the editor while
// the user picks their login method and provider.
type loginDialog struct {
	step     loginStep
	method   string // "apikey" | "oauth"
	provider string // "anthropic" | "openai"
	message  string
	success  bool
	url      string
	cursor   int

	// status is a snapshot of the current login state for each
	// provider, captured when Open() runs. Rendered above the
	// method picker so the user can see whether they're already
	// logged in (and how) before starting a new flow. Keys:
	// "anthropic", "openai". Value is "apikey", "oauth", or ""
	// (not logged in).
	status map[string]string
}

func newLoginDialog() *loginDialog {
	return &loginDialog{}
}

// Active reports whether the dialog consumes input.
func (d *loginDialog) Active() bool { return d != nil && d.step != loginStepClosed }

// Open starts the dialog from scratch and captures the current
// login status for each provider so the picker can show it.
// zotHome is the zot state directory ($ZOT_HOME); auth.json
// lives inside it. Passing the path in (instead of importing
// the agent package to call AuthPath()) avoids a cyclic import
// between agent and agent/modes.
func (d *loginDialog) Open(zotHome string) {
	d.step = loginStepMethod
	d.method = ""
	d.provider = ""
	d.message = ""
	d.success = false
	d.url = ""
	d.cursor = 0
	d.status = map[string]string{"anthropic": "", "openai": ""}
	// Best-effort: if the auth file can't be read, treat every
	// provider as not-logged-in. The status line just won't show
	// anything useful in that case, which is fine — the user
	// was about to log in anyway.
	path := filepath.Join(zotHome, "auth.json")
	if creds, err := auth.NewStore(path).Load(); err == nil {
		d.status["anthropic"] = creds.Method("anthropic")
		d.status["openai"] = creds.Method("openai")
	}
}

// Close hides the dialog.
func (d *loginDialog) Close() {
	d.step = loginStepClosed
}

// Render returns the dialog lines or nil when inactive.
func (d *loginDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string

	switch d.step {
	case loginStepMethod:
		opts := []string{
			"api key",
			"subscription (claude pro/max · chatgpt plus/pro)",
		}
		lines = append(lines, frameHeader(th, "login", width))
		for _, l := range d.renderStatusLines(th) {
			lines = append(lines, l)
		}
		lines = append(lines, th.FG256(th.Muted, "choose login method (↑/↓, enter, esc to cancel):"))
		for i, o := range opts {
			plain := "  " + o
			if i == d.cursor {
				lines = append(lines, th.PadHighlight(plain, width))
			} else {
				lines = append(lines, th.FG256(th.Muted, plain))
			}
		}
		lines = append(lines, frameRule(th, width))
	case loginStepProvider:
		opts := []string{"anthropic", "openai"}
		lines = append(lines, frameHeader(th, "login · "+d.method, width))
		for _, l := range d.renderStatusLines(th) {
			lines = append(lines, l)
		}
		lines = append(lines, th.FG256(th.Muted, "choose provider:"))
		for i, o := range opts {
			// Annotate each provider with its current login
			// state so the user can see at a glance which will
			// be replaced if they pick it.
			tag := ""
			switch d.status[o] {
			case "apikey":
				tag = "  (api key)"
			case "oauth":
				tag = "  (subscription)"
			}
			plain := "  " + o + tag
			if i == d.cursor {
				lines = append(lines, th.PadHighlight(plain, width))
			} else {
				lines = append(lines, th.FG256(th.Muted, plain))
			}
		}
		lines = append(lines, frameRule(th, width))
	case loginStepWaiting:
		lines = append(lines, frameHeader(th, "login · "+d.method+" · "+d.provider, width))
		lines = append(lines, th.FG256(th.FG, "opening browser..."))
		lines = append(lines, th.FG256(th.Muted, d.url))
		lines = append(lines, "")
		lines = append(lines, th.FG256(th.Muted, "waiting for callback. press esc to cancel."))
		lines = append(lines, frameRule(th, width))
	case loginStepDone:
		title := "login · failed"
		body := th.FG256(th.Error, d.message)
		if d.success {
			title = "login · success"
			body = th.FG256(th.Tool, fmt.Sprintf("logged in to %s via %s", d.provider, d.method))
		}
		lines = append(lines, frameHeader(th, title, width))
		lines = append(lines, body)
		lines = append(lines, th.FG256(th.Muted, "press any key to close"))
		lines = append(lines, frameRule(th, width))
	}
	return lines
}

// renderStatusLines returns an overview of the current login
// state for each provider, one row per provider, suitable to
// insert between the frame header and the picker body. Logged-
// in providers get a green checkmark in front; providers with
// no credentials render as a muted dash so the list layout
// stays aligned across first-run and re-login cases.
//
// Returns nil when neither provider is logged in (first-run
// case — a pair of "not logged in" rows is just noise when the
// user is about to pick a method anyway).
func (d *loginDialog) renderStatusLines(th tui.Theme) []string {
	anth := d.status["anthropic"]
	op := d.status["openai"]
	if anth == "" && op == "" {
		return nil
	}
	row := func(name, method string) string {
		var mark, body string
		switch method {
		case "apikey":
			mark = th.FG256(th.Tool, "✓")
			body = th.FG256(th.Muted, name+": api key")
		case "oauth":
			mark = th.FG256(th.Tool, "✓")
			body = th.FG256(th.Muted, name+": subscription")
		default:
			mark = th.FG256(th.Muted, "–")
			body = th.FG256(th.Muted, name+": not logged in")
		}
		return "  " + mark + " " + body
	}
	return []string{
		row("anthropic", anth),
		row("openai", op),
		"",
	}
}

// Key is the result of handling a key press.
type loginDialogAction struct {
	StartAPIKey bool
	StartOAuth  bool
	Provider    string
	Close       bool
}

// HandleKey advances the dialog and returns an action to apply, if any.
func (d *loginDialog) HandleKey(k tui.Key) loginDialogAction {
	switch d.step {
	case loginStepMethod:
		return d.handleMethodKey(k)
	case loginStepProvider:
		return d.handleProviderKey(k)
	case loginStepWaiting:
		if k.Kind == tui.KeyEsc {
			d.Close()
			return loginDialogAction{Close: true}
		}
	case loginStepDone:
		d.Close()
		return loginDialogAction{Close: true}
	}
	return loginDialogAction{}
}

func (d *loginDialog) handleMethodKey(k tui.Key) loginDialogAction {
	max := 2
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < max-1 {
			d.cursor++
		}
	case tui.KeyEsc:
		d.Close()
		return loginDialogAction{Close: true}
	case tui.KeyEnter:
		if d.cursor == 0 {
			d.method = "apikey"
		} else {
			d.method = "oauth"
		}
		d.step = loginStepProvider
		d.cursor = 0
	}
	return loginDialogAction{}
}

func (d *loginDialog) handleProviderKey(k tui.Key) loginDialogAction {
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < 1 {
			d.cursor++
		}
	case tui.KeyEsc:
		d.Close()
		return loginDialogAction{Close: true}
	case tui.KeyEnter:
		providers := []string{"anthropic", "openai"}
		d.provider = providers[d.cursor]
		d.step = loginStepWaiting
		if d.method == "apikey" {
			return loginDialogAction{StartAPIKey: true, Provider: d.provider}
		}
		return loginDialogAction{StartOAuth: true, Provider: d.provider}
	}
	return loginDialogAction{}
}

// ShowWaiting transitions to the waiting state with the given URL.
// No-op if the user has already dismissed the dialog.
func (d *loginDialog) ShowWaiting(url string) {
	if d.step == loginStepClosed {
		return
	}
	d.step = loginStepWaiting
	d.url = url
}

// ShowResult transitions to the done state with the given outcome.
// No-op if the user has already dismissed the dialog.
func (d *loginDialog) ShowResult(success bool, message string) {
	if d.step == loginStepClosed {
		return
	}
	d.step = loginStepDone
	d.success = success
	d.message = message
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}
