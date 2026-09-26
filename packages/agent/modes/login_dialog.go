package modes

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
	"github.com/patriceckhart/zot/packages/tui"
)

// loginStep is the current node in the login dialog state machine.
type loginStep int

// loginStepClosed is the zero value on purpose: a freshly-constructed
// dialog must default to closed so nothing shows up until Open() is
// explicitly called.
const (
	loginStepClosed      loginStep = iota
	loginStepMethod                // pick apikey vs subscription
	loginStepProvider              // pick anthropic vs openai vs kimi
	loginStepOAuthMethod           // pick browser or copy-code for Anthropic
	loginStepLlamaURL              // enter and validate llama.cpp router URL
	loginStepLlamaKey              // enter optional llama.cpp API key
	loginStepWaiting               // browser open, waiting for callback
	loginStepPasteCode             // user pastes the auth code here
	loginStepInfo                  // informational setup guidance
	loginStepDone                  // success or error, waiting for key to dismiss
)

const loginProviderPageSize = 8

// loginDialog is a tiny inline dialog rendered above the editor while
// the user picks their login method and provider.
type loginDialog struct {
	step          loginStep
	method        string // "apikey" | "oauth"
	provider      string // "anthropic" | "openai" | "openai-codex" | "kimi" | "google"
	message       string
	success       bool
	url           string
	cursor        int
	providerQuery string
	codeEd        *tui.Editor
	llamaEd       *tui.Editor
	llamaURL      string
	infoTitle     string
	infoLines     []string

	// status is a snapshot of the current login state for each
	// provider, captured when Open() runs. Rendered above the
	// method picker so the user can see whether they're already
	// logged in (and how) before starting a new flow. Keys:
	// "anthropic", "openai", "openai-codex", "kimi", "google". Value is
	// "apikey", "oauth", or "" (not logged in).
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
	d.providerQuery = ""
	d.codeEd = nil
	d.llamaEd = nil
	d.llamaURL = ""
	d.infoTitle = ""
	d.infoLines = nil
	d.status = map[string]string{}
	for _, p := range providersForMethod("apikey") {
		d.status[p] = ""
	}
	for _, p := range providersForMethod("oauth") {
		d.status[p] = ""
	}
	// Best-effort: if the auth file can't be read, treat every
	// provider as not-logged-in. The status line just won't show
	// anything useful in that case, which is fine — the user
	// was about to log in anyway.
	path := filepath.Join(zotHome, "auth.json")
	if creds, err := auth.NewStore(path).Load(); err == nil {
		d.status["anthropic"] = creds.Method("anthropic")
		d.status["openai"] = ""
		if creds.OpenAI.APIKey != "" || creds.OpenAI.APIKeyCommand != nil {
			d.status["openai"] = "apikey"
		}
		d.status["openai-codex"] = ""
		if creds.OpenAI.OAuth != nil {
			d.status["openai-codex"] = "oauth"
		}
		d.status["kimi"] = creds.Method("kimi")
		d.status["deepseek"] = creds.Method("deepseek")
		d.status["google"] = creds.Method("google")
		d.status["github-copilot"] = creds.Method("github-copilot")
		for p := range creds.AdditionalAPIKeyCreds {
			d.status[p] = creds.Method(p)
		}
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
			"subscription (claude pro/max - chatgpt plus/pro - chatgpt codex - kimi code - github copilot)",
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
		opts := d.providerOptions()
		lines = append(lines, frameHeader(th, "login - "+d.method, width))
		for _, l := range d.renderStatusLines(th) {
			lines = append(lines, l)
		}
		hint := "pick a provider (↑/↓, enter, esc to cancel) - type to filter"
		if d.providerQuery != "" {
			hint = fmt.Sprintf("filter: %s (%d match)", d.providerQuery, len(opts))
			if len(opts) != 1 {
				hint = fmt.Sprintf("filter: %s (%d matches)", d.providerQuery, len(opts))
			}
		}
		lines = append(lines, th.FG256(th.Muted, hint))
		start, end := d.providerPage(len(opts))
		for i := start; i < end; i++ {
			o := opts[i]
			tag := providerPickerTag(d.method, d.status[o])
			providerName := providerLabel(o)
			if d.method == "oauth" && o == "xai" {
				providerName = "Sign in with SuperGrok or X Premium"
			}
			label := "  " + providerName
			plain := label + tag
			if i == d.cursor {
				lines = append(lines, th.PadHighlight(plain, width))
			} else {
				lines = append(lines, th.FG256(th.Muted, label)+th.FG256(th.Accent, tag))
			}
		}
		if len(opts) == 0 {
			lines = append(lines, th.FG256(th.Muted, "  no providers match "+fmt.Sprintf("%q", d.providerQuery)))
		} else if len(opts) > loginProviderPageSize {
			lines = append(lines, th.FG256(th.Muted, fmt.Sprintf("  (%d/%d)", d.cursor+1, len(opts))))
		}
		lines = append(lines, frameRule(th, width))
	case loginStepOAuthMethod:
		lines = append(lines, frameHeader(th, "login - subscription - "+providerLabel(d.provider), width))
		lines = append(lines, th.FG256(th.Muted, "choose sign-in flow (↑/↓, enter, esc to go back):"))
		for idx, label := range []string{"browser callback", "copy code (headless / SSH)"} {
			plain := "  " + label
			if idx == d.cursor {
				lines = append(lines, th.PadHighlight(plain, width))
			} else {
				lines = append(lines, th.FG256(th.Muted, plain))
			}
		}
		lines = append(lines, frameRule(th, width))
	case loginStepLlamaURL:
		lines = append(lines, frameHeader(th, "login - api key - "+providerLabel(d.provider), width))
		lines = append(lines, th.FG256(th.Muted, "server URL:"))
		if d.llamaEd == nil {
			d.llamaEd = tui.NewEditor(th.AccentBar(th.Accent))
		}
		edLines, _, _ := d.llamaEd.Render(width - 2)
		lines = append(lines, edLines...)
		if d.message != "" {
			lines = append(lines, th.FG256(th.Error, d.message))
		}
		lines = append(lines, "", th.FG256(th.Muted, "enter continues - esc cancels"), frameRule(th, width))
	case loginStepLlamaKey:
		lines = append(lines, frameHeader(th, "login - api key - "+providerLabel(d.provider), width))
		lines = append(lines, th.FG256(th.Muted, "server: "+d.llamaURL), "", th.FG256(th.Muted, "API key (optional):"))
		if d.llamaEd == nil {
			d.llamaEd = tui.NewEditor(th.AccentBar(th.Accent))
		}
		d.llamaEd.Mask = true
		edLines, _, _ := d.llamaEd.Render(width - 2)
		lines = append(lines, edLines...)
		lines = append(lines, "", th.FG256(th.Muted, "enter saves - esc cancels"), frameRule(th, width))
	case loginStepWaiting:
		lines = append(lines, frameHeader(th, "login - "+d.method+" - "+providerLabel(d.provider), width))
		lines = append(lines, th.FG256(th.Muted, "open this URL in a browser:"))
		wrapW := width - 2
		if wrapW < 20 {
			wrapW = 20
		}
		for _, seg := range tui.WrapANSILine(d.url, wrapW) {
			lines = append(lines, th.FG256(th.Accent, seg))
		}
		lines = append(lines, "")
		if d.method == "oauth" && d.provider != "anthropic" && d.provider != "openai-codex" {
			lines = append(lines, th.FG256(th.Muted, "complete sign-in in the browser - esc cancels"))
			lines = append(lines, frameRule(th, width))
			break
		}
		lines = append(lines, th.FG256(th.Muted, "paste the authorization code (or full redirect URL / code#state):"))
		if d.codeEd == nil {
			d.codeEd = tui.NewEditor(th.AccentBar(th.Accent))
		}
		edLines, _, _ := d.codeEd.Render(width - 2)
		for _, l := range edLines {
			lines = append(lines, l)
		}
		lines = append(lines, "")
		lines = append(lines, th.FG256(th.Muted, "enter submits - esc cancels - waiting for browser callback in background"))
		lines = append(lines, frameRule(th, width))
	case loginStepPasteCode:
		lines = append(lines, frameHeader(th, "login - "+d.method+" - "+providerLabel(d.provider)+" - paste code", width))
		lines = append(lines, th.FG256(th.Muted, "open this URL in any browser:"))
		wrapW := width - 2
		if wrapW < 20 {
			wrapW = 20
		}
		for _, seg := range tui.WrapANSILine(d.url, wrapW) {
			lines = append(lines, th.FG256(th.Accent, seg))
		}
		lines = append(lines, "")
		lines = append(lines, th.FG256(th.Muted, "paste the authorization code (or full redirect URL / code#state):"))
		if d.codeEd == nil {
			d.codeEd = tui.NewEditor(th.AccentBar(th.Accent))
		}
		edLines, _, _ := d.codeEd.Render(width - 2)
		for _, l := range edLines {
			lines = append(lines, l)
		}
		lines = append(lines, "")
		lines = append(lines, th.FG256(th.Muted, "enter submits - esc cancels"))
		lines = append(lines, frameRule(th, width))
	case loginStepInfo:
		title := d.infoTitle
		if title == "" {
			title = "login - setup"
		}
		lines = append(lines, frameHeader(th, title, width))
		for _, l := range d.infoLines {
			lines = append(lines, l)
		}
		lines = append(lines, "")
		lines = append(lines, th.FG256(th.Muted, "press any key to close"))
		lines = append(lines, frameRule(th, width))
	case loginStepDone:
		title := "login - failed"
		body := th.FG256(th.Error, d.message)
		if d.success {
			title = "login - success"
			body = th.FG256(th.Tool, fmt.Sprintf("logged in to %s via %s", providerLabel(d.provider), d.method))
		}
		lines = append(lines, frameHeader(th, title, width))
		lines = append(lines, body)
		lines = append(lines, th.FG256(th.Muted, "press any key to close"))
		lines = append(lines, frameRule(th, width))
	}
	return lines
}

// providersForMethod returns the providers offered for a given login
// method. API-key is the universal path so it lists every provider
// (including custom providers from models.json); subscription/OAuth
// only lists providers that actually issue tokens usable against the
// same API the model picker drives (Google's consumer Gemini Advanced
// login does not, and DeepSeek has no subscription product at all).
func providersForMethod(method string) []string {
	var providers []string
	if method == "oauth" {
		providers = []string{"anthropic", "openai-codex", "kimi", "xai", "github-copilot"}
	} else {
		providers = auth.APIKeyProviders()
		// Append custom providers from models.json so they appear
		// in the /login picker alongside built-in providers. Dedupe in
		// case models.json also adds metadata for a built-in provider.
		seen := map[string]bool{}
		for _, name := range providers {
			seen[name] = true
		}
		for name := range provider.CustomProviders() {
			if !seen[name] {
				providers = append(providers, name)
				seen[name] = true
			}
		}
	}
	sort.Slice(providers, func(a, b int) bool {
		return providerLabel(providers[a]) < providerLabel(providers[b])
	})
	return providers
}

// providerLabel returns the user-facing label for a provider id.
func providerLabel(id string) string { return provider.ProviderLabel(id) }

func (d *loginDialog) providerOptions() []string {
	providers := providersForMethod(d.method)
	needle := normalizeModelQuery(d.providerQuery)
	if needle == "" {
		return providers
	}
	filtered := make([]string, 0, len(providers))
	for _, id := range providers {
		if strings.Contains(normalizeModelQuery(id+" "+providerLabel(id)), needle) {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

func providerPickerTag(method, status string) string {
	switch method {
	case "apikey":
		// In the API-key picker, only call out an existing subscription so
		// users know choosing this provider will add/replace API-key auth
		// while subscription auth is still configured. Unconfigured rows do
		// not need a redundant "api key" suffix.
		if status == "oauth" {
			return "  (subscription configured)"
		}
	case "oauth":
		// In the subscription picker, only call out an existing API key.
		if status == "apikey" {
			return "  (api key configured)"
		}
	}
	return ""
}

func (d *loginDialog) providerPage(total int) (start, end int) {
	if total <= loginProviderPageSize {
		return 0, total
	}
	if d.cursor < 0 {
		d.cursor = 0
	}
	if d.cursor >= total {
		d.cursor = total - 1
	}
	start = (d.cursor / loginProviderPageSize) * loginProviderPageSize
	end = start + loginProviderPageSize
	if end > total {
		end = total
	}
	return start, end
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
	codex := d.status["openai-codex"]
	kimi := d.status["kimi"]
	ds := d.status["deepseek"]
	goog := d.status["google"]
	gh := d.status["github-copilot"]
	if anth == "" && op == "" && codex == "" && kimi == "" && ds == "" && goog == "" && gh == "" {
		return nil
	}
	row := func(id, method string) string {
		label := providerLabel(id)
		var mark, body string
		switch method {
		case "apikey":
			mark = th.FG256(th.Tool, "✓")
			body = th.FG256(th.Muted, label+": api key")
		case "oauth":
			mark = th.FG256(th.Tool, "✓")
			body = th.FG256(th.Muted, label+": subscription")
		default:
			mark = th.FG256(th.Muted, "–")
			body = th.FG256(th.Muted, label+": not logged in")
		}
		return "  " + mark + " " + body
	}
	out := []string{
		row("anthropic", anth),
		row("openai", op),
		row("openai-codex", codex),
		row("kimi", kimi),
		row("deepseek", ds),
		row("google", goog),
		row("github-copilot", gh),
	}
	for _, p := range providersForMethod("apikey") {
		switch p {
		case "anthropic", "openai", "openai-codex", "kimi", "deepseek", "google", "github-copilot":
			continue
		}
		if method := d.status[p]; method != "" {
			out = append(out, row(p, method))
		}
	}
	out = append(out, "")
	return out
}

// Key is the result of handling a key press.
type loginDialogAction struct {
	StartAPIKey bool
	StartOAuth  bool
	StartManual bool
	SaveLlama   bool
	Provider    string
	LlamaURL    string
	LlamaAPIKey string
	Close       bool
	SubmitCode  string
}

// HandleKey advances the dialog and returns an action to apply, if any.
func (d *loginDialog) HandleKey(k tui.Key) loginDialogAction {
	switch d.step {
	case loginStepMethod:
		return d.handleMethodKey(k)
	case loginStepProvider:
		return d.handleProviderKey(k)
	case loginStepOAuthMethod:
		return d.handleOAuthMethodKey(k)
	case loginStepLlamaURL, loginStepLlamaKey:
		return d.handleLlamaKey(k)
	case loginStepWaiting:
		return d.handleWaitingKey(k)
	case loginStepPasteCode:
		return d.handlePasteCodeKey(k)
	case loginStepInfo:
		d.Close()
		return loginDialogAction{Close: true}
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
		d.providerQuery = ""
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
		providers := d.providerOptions()
		if d.cursor < len(providers)-1 {
			d.cursor++
		}
	case tui.KeyPageUp:
		d.cursor -= loginProviderPageSize
		if d.cursor < 0 {
			d.cursor = 0
		}
	case tui.KeyPageDown:
		providers := d.providerOptions()
		if len(providers) > 0 {
			d.cursor += loginProviderPageSize
			if d.cursor >= len(providers) {
				d.cursor = len(providers) - 1
			}
		}
	case tui.KeyBackspace:
		if len(d.providerQuery) > 0 {
			runes := []rune(d.providerQuery)
			d.providerQuery = string(runes[:len(runes)-1])
			d.cursor = 0
		}
	case tui.KeyPaste:
		d.providerQuery += singleLinePaste(k.Paste)
		d.cursor = 0
	case tui.KeyRune:
		if !k.Alt && !k.Ctrl && k.Rune >= 0x20 && k.Rune < 0x7f {
			d.providerQuery += string(k.Rune)
			d.cursor = 0
		}
	case tui.KeyEsc:
		d.Close()
		return loginDialogAction{Close: true}
	case tui.KeyEnter:
		providers := d.providerOptions()
		if d.cursor < 0 || d.cursor >= len(providers) {
			return loginDialogAction{}
		}
		d.provider = providers[d.cursor]
		if d.method == "apikey" && (d.provider == provider.LlamaCPPProviderID || d.provider == provider.LMStudioProviderID) {
			d.step = loginStepLlamaURL
			d.llamaEd = nil
			d.message = ""
			return loginDialogAction{}
		}
		if d.method == "oauth" && d.provider == "anthropic" {
			d.step = loginStepOAuthMethod
			d.cursor = 0
			return loginDialogAction{}
		}
		d.step = loginStepWaiting
		if d.method == "apikey" {
			return loginDialogAction{StartAPIKey: true, Provider: d.provider}
		}
		return loginDialogAction{StartOAuth: true, Provider: d.provider}
	}
	return loginDialogAction{}
}

func (d *loginDialog) handleOAuthMethodKey(k tui.Key) loginDialogAction {
	switch k.Kind {
	case tui.KeyUp:
		d.cursor = 0
	case tui.KeyDown:
		d.cursor = 1
	case tui.KeyEsc:
		d.step = loginStepProvider
		d.cursor = 0
	case tui.KeyEnter:
		if d.cursor == 1 {
			d.step = loginStepPasteCode
			return loginDialogAction{StartManual: true, Provider: d.provider}
		}
		d.step = loginStepWaiting
		return loginDialogAction{StartOAuth: true, Provider: d.provider}
	}
	return loginDialogAction{}
}

func (d *loginDialog) handleLlamaKey(k tui.Key) loginDialogAction {
	if k.Kind == tui.KeyEsc {
		d.Close()
		return loginDialogAction{Close: true}
	}
	if d.llamaEd == nil {
		d.llamaEd = tui.NewEditor("")
	}
	if !d.llamaEd.HandleKey(k) {
		return loginDialogAction{}
	}
	value := strings.TrimSpace(d.llamaEd.SubmitValue())
	d.llamaEd.Clear()
	if d.step == loginStepLlamaURL {
		normalize := provider.NormalizeLlamaCPPURL
		if d.provider == provider.LMStudioProviderID {
			normalize = provider.NormalizeLMStudioURL
		}
		normalized, err := normalize(value)
		if err != nil {
			d.message = err.Error()
			return loginDialogAction{}
		}
		d.llamaURL = normalized
		d.message = ""
		d.llamaEd.Mask = true
		d.step = loginStepLlamaKey
		return loginDialogAction{}
	}
	return loginDialogAction{
		SaveLlama: true, Provider: d.provider,
		LlamaURL: d.llamaURL, LlamaAPIKey: value,
	}
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

// ShowPasteCode displays the URL for a manual OAuth transaction.
func (d *loginDialog) ShowPasteCode(url string) {
	if d.step == loginStepClosed {
		return
	}
	d.step = loginStepPasteCode
	d.url = url
}

// ShowInfo transitions to an informational setup dialog.
// No-op if the user has already dismissed the dialog.
func (d *loginDialog) ShowInfo(title string, lines []string) {
	if d.step == loginStepClosed {
		return
	}
	d.step = loginStepInfo
	d.infoTitle = title
	d.infoLines = lines
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

func (d *loginDialog) handleWaitingKey(k tui.Key) loginDialogAction {
	if k.Kind == tui.KeyEsc {
		d.Close()
		return loginDialogAction{Close: true}
	}
	if d.codeEd == nil {
		return loginDialogAction{}
	}
	if submit := d.codeEd.HandleKey(k); submit {
		code := d.codeEd.SubmitValue()
		d.codeEd.Clear()
		return loginDialogAction{SubmitCode: code}
	}
	return loginDialogAction{}
}

func (d *loginDialog) handlePasteCodeKey(k tui.Key) loginDialogAction {
	if k.Kind == tui.KeyEsc {
		d.Close()
		return loginDialogAction{Close: true}
	}
	if d.codeEd == nil {
		return loginDialogAction{}
	}
	if submit := d.codeEd.HandleKey(k); submit {
		code := d.codeEd.SubmitValue()
		d.codeEd.Clear()
		return loginDialogAction{SubmitCode: code}
	}
	return loginDialogAction{}
}

// CursorPos returns the absolute row/col inside the dialog where the
// terminal cursor should sit for code, URL, and API-key input. Returns -1,
// -1 if the dialog is not in an input-expecting state. The host uses this to
// place the real blinking cursor on the active input.
func (d *loginDialog) CursorPos(width int) (row, col int) {
	if d.step == loginStepLlamaURL || d.step == loginStepLlamaKey {
		if d.llamaEd == nil {
			return -1, -1
		}
		_, eRow, eCol := d.llamaEd.Render(width - 2)
		baseOffset := 1 /*frameHeader*/ + 1 /*padDialogFrame blank*/ + 1 /*prompt*/
		if d.step == loginStepLlamaKey {
			baseOffset += 2 // router line + blank
		}
		return baseOffset + eRow, eCol
	}
	if d.codeEd == nil {
		return -1, -1
	}
	if d.step != loginStepPasteCode && d.step != loginStepWaiting {
		return -1, -1
	}
	_, eRow, eCol := d.codeEd.Render(width - 2)
	wrapW := width - 2
	if wrapW < 20 {
		wrapW = 20
	}
	urlLines := len(tui.WrapANSILine(d.url, wrapW))
	// interactive.redraw wraps dialog output with padDialogFrame, which
	// injects a blank row after the frame header. Count that row here so
	// the real terminal cursor lands on the editor input instead of the
	// prompt above it.
	baseOffset := 1 /*frameHeader*/ + 1 /*padDialogFrame blank*/ + 1 /*hint*/ + urlLines + 1 /*blank*/ + 1 /*prompt*/
	return baseOffset + eRow, eCol
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}
