package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/agent/tools"
	"github.com/patriceckhart/zot/internal/auth"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/tui"
)

// InteractiveConfig configures the interactive loop.
type InteractiveConfig struct {
	Terminal     tui.Terminal
	Theme        tui.Theme
	Model        string
	Provider     string
	AuthMethod   string // "apikey" | "oauth" — used to tag cost as (sub) in status bar
	BaseURL      string
	Reasoning    string
	SystemPrompt string
	Tools        core.Registry
	MaxSteps     int
	CWD          string

	// Agent is optional. If nil, zot opens without credentials; the
	// user must /login before they can prompt.
	Agent *core.Agent

	InitialInput string

	// Auth is required. When the user runs /login, Interactive talks to
	// AuthManager to open a browser and wait for the callback.
	AuthManager *auth.Manager
	// BuildAgent is called after a successful login to (re)construct the
	// agent with the fresh credential. It returns the new agent and
	// the concrete provider/model in use.
	BuildAgent func() (*core.Agent, string, string, error)

	// BuildAgentFor rebuilds the agent with an explicit provider/model
	// override (used by the /model picker when switching providers).
	// If providerOverride is empty, the current provider is kept.
	BuildAgentFor func(providerOverride, modelOverride string) (*core.Agent, string, string, error)

	// ZotHome is the root directory for sessions/, used by /sessions.
	ZotHome string

	// Sandbox is the shared sandbox pointer. Toggled by /lock and /unlock.
	Sandbox *tools.Sandbox

	// LoadSession swaps the current session for the one at path. The
	// callback returns the new agent message slice so the TUI can invalidate.
	LoadSession func(path string) error

	// PersistModel is called whenever the user switches model or provider.
	// It should update config.json and (if there's an active session)
	// write a new meta row so resume picks up the same model.
	PersistModel func(providerName, model string)

	OnAssistant  func(m provider.Message)
	OnToolResult func(id string, r core.ToolResult)
}

// Interactive is the TUI chat loop.
type Interactive struct {
	cfg  InteractiveConfig
	view *tui.View
	ed   *tui.Editor
	rend *tui.Renderer

	mu           sync.Mutex
	agent        *core.Agent
	streaming    strings.Builder
	streamOn     bool
	toolCalls    map[string]*tui.ToolCallView
	toolOrder    []string
	statusErr    string
	statusOK     string
	helpBlock    []string // rendered above the chat when /help was typed
	cumUsage     provider.Usage
	lastCtxInput int // input_tokens of the most recent turn — approximates current context size
	busy         bool
	dirty        chan struct{}
	cancelTurn   context.CancelFunc
	scrollOffset int // rows from the bottom; 0 = pinned to latest

	// Messages typed while a turn is in flight. Each is delivered as
	// its own follow-up turn once the current one finishes. Rendered
	// above the status bar as "sliding in: ..." chips.
	queued []string

	// runCtx is the top-level context passed to Run(). Follow-up turns
	// drained from `queued` are started against this context so they
	// survive past the ctx of the key event that enqueued them.
	runCtx context.Context

	// autoCompacting is true while a model-triggered compaction is in
	// flight. Surfaced in the status bar so the user can tell a
	// condense pass from a regular assistant turn.
	autoCompacting bool

	dialog        *loginDialog
	modelDialog   *modelDialog
	sessionDialog *sessionDialog
	suggest       *slashSuggester
	spin          *spinner
}

// NewInteractive constructs an Interactive from cfg.
func NewInteractive(cfg InteractiveConfig) *Interactive {
	i := &Interactive{
		cfg: cfg,
		view: &tui.View{
			Theme:      cfg.Theme,
			ImageProto: tui.DetectImageProtocol(),
		},
		ed:            tui.NewEditor(cfg.Theme.FG256(cfg.Theme.Accent, "▌ ")),
		rend:          tui.NewRenderer(cfg.Terminal),
		toolCalls:     map[string]*tui.ToolCallView{},
		dirty:         make(chan struct{}, 8),
		dialog:        newLoginDialog(),
		modelDialog:   newModelDialog(),
		sessionDialog: newSessionDialog(),
		suggest:       newSlashSuggester(),
		spin:          newSpinner(),
	}
	if cfg.Agent != nil {
		i.agent = cfg.Agent
		i.view.Messages = cfg.Agent.Messages()
	}
	return i
}

// Run blocks until the user quits.
func (i *Interactive) Run(ctx context.Context) error {
	i.runCtx = ctx
	term := i.cfg.Terminal
	restore, err := term.EnterRaw()
	if err != nil {
		return err
	}
	defer restore()

	_, _ = term.Write([]byte(tui.SeqBracketedPasteOn))
	_, _ = term.Write([]byte(tui.SeqAltScreenOn))
	defer term.Write([]byte(tui.SeqAltScreenOff + tui.SeqBracketedPasteOff + tui.SeqShowCursor))

	cols, rows := term.Size()
	i.rend.Resize(cols, rows)
	term.OnResize(func() {
		c, r := term.Size()
		i.rend.Resize(c, r)
		i.invalidate()
	})

	if i.cfg.InitialInput != "" {
		i.ed.SetValue(i.cfg.InitialInput)
	}

	// No credential at startup? Auto-open the login dialog, and mark
	// the status line. The user can Esc out of the dialog if they
	// want to dismiss it (e.g. to check /help or /exit first).
	if i.agent == nil {
		i.statusErr = "not logged in. pick a login method below or press esc to dismiss."
		i.dialog.Open()
	}

	// Input goroutine.
	keys := make(chan tui.Key, 8)
	go func() {
		reader := tui.NewReaderWithPeek(term.ReadByte, term.PeekByteTimeout)
		for {
			k, err := reader.Read()
			if err != nil {
				return
			}
			keys <- k
		}
	}()

	// Subscribe to auth events.
	var authEvents <-chan auth.Event
	if i.cfg.AuthManager != nil {
		authEvents = i.cfg.AuthManager.Events()
	}

	// Animation ticker: drives spinner and dialog-related redraws when
	// nothing else changed. 120ms is slow enough that highlighting a huge
	// transcript doesn't spin the cpu.
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()

	// Redraw throttle: coalesce bursts of invalidate() calls so we paint
	// at most once every redrawMinInterval. Huge tool-result dumps can
	// fire hundreds of invalidations while the user is typing; without
	// this, the input goroutine never gets CPU and keystrokes lag.
	const redrawMinInterval = 16 * time.Millisecond
	var lastRedraw time.Time
	var pendingRedraw bool
	var pendingTimer *time.Timer

	drainPending := func() {
		if pendingTimer != nil {
			pendingTimer.Stop()
			pendingTimer = nil
		}
		if pendingRedraw {
			pendingRedraw = false
			lastRedraw = time.Now()
			i.redraw()
		}
	}

	requestRedraw := func() {
		since := time.Since(lastRedraw)
		if since >= redrawMinInterval {
			lastRedraw = time.Now()
			i.redraw()
			return
		}
		if pendingRedraw {
			return // already scheduled
		}
		pendingRedraw = true
		wait := redrawMinInterval - since
		if pendingTimer == nil {
			pendingTimer = time.AfterFunc(wait, func() {
				// Poke the dirty channel; the main loop will call drainPending().
				i.invalidate()
			})
		} else {
			pendingTimer.Reset(wait)
		}
	}

	i.invalidate()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case k := <-keys:
			if done := i.handleKey(ctx, k); done {
				return nil
			}
			i.invalidate()
		case ev := <-authEvents:
			i.handleAuthEvent(ev)
			i.invalidate()
		case <-i.dirty:
			requestRedraw()
		case <-tick.C:
			if i.busy || i.dialog.Active() || i.modelDialog.Active() || i.sessionDialog.Active() {
				drainPending()
				requestRedraw()
			}
		}
	}
}

func (i *Interactive) invalidate() {
	select {
	case i.dirty <- struct{}{}:
	default:
	}
}

// lastCols returns the current terminal width in columns.
func (i *Interactive) lastCols() int {
	cols, _ := i.cfg.Terminal.Size()
	return cols
}

// chatPage returns the number of chat rows currently visible, used
// as the page size for PageUp/PageDown.
func (i *Interactive) chatPage() int {
	_, rows := i.cfg.Terminal.Size()
	p := rows - 6 // rough reservation for status + editor + a dialog line
	if p < 4 {
		p = 4
	}
	return p
}

// scrollBy adjusts the scroll offset. Positive = up (into history).
func (i *Interactive) scrollBy(delta int) {
	i.mu.Lock()
	i.scrollOffset += delta
	if i.scrollOffset < 0 {
		i.scrollOffset = 0
	}
	i.mu.Unlock()
	i.invalidate()
}

// scrollToBottom pins the view to the latest content.
func (i *Interactive) scrollToBottom() {
	i.mu.Lock()
	i.scrollOffset = 0
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) redraw() {
	i.mu.Lock()
	defer i.mu.Unlock()

	cols, _ := i.cfg.Terminal.Size()
	if i.agent != nil {
		i.view.Messages = i.agent.Messages()
	} else {
		i.view.Messages = nil
	}
	i.view.Streaming = i.streaming.String()
	i.view.StreamingActive = i.streamOn
	// Belt and suspenders: if the transcript's last message is already an
	// assistant message, the streaming view is stale (EvAssistantMessage may
	// not have fired yet) — suppress it to avoid double-rendering the reply.
	if n := len(i.view.Messages); n > 0 && i.view.Messages[n-1].Role == provider.RoleAssistant {
		i.view.StreamingActive = false
	}
	// Live tool-call view: only shown while a turn is in flight. Once
	// the agent is idle, every tool call has already been folded into
	// the transcript (as assistant.ToolCallBlock + a tool-role message),
	// so showing v.ToolCalls a second time would duplicate them below
	// the final assistant text — which looks like the summary came
	// "before" the tools.
	i.view.ToolCalls = i.view.ToolCalls[:0]
	if i.busy {
		for _, id := range i.toolOrder {
			if tc, ok := i.toolCalls[id]; ok {
				i.view.ToolCalls = append(i.view.ToolCalls, *tc)
			}
		}
	}
	i.view.Err = i.statusErr

	chat := i.view.Build(cols)

	// Welcome banner: shown at the top of the chat area when there is
	// no transcript yet. Disappears after the first message is sent.
	if len(i.view.Messages) == 0 && !i.streamOn && len(i.toolOrder) == 0 {
		chat = append(welcomeBanner(i.cfg.Theme), chat...)
	}

	// /help block: rendered above the welcome / transcript when active.
	if len(i.helpBlock) > 0 {
		chat = append(append([]string(nil), i.helpBlock...), chat...)
	}

	if i.statusOK != "" {
		chat = append(chat, i.cfg.Theme.FG256(i.cfg.Theme.Tool, "✓ "+i.statusOK), "")
	}

	// Dialogs (login or model picker) render between chat and the editor.
	var dialog []string
	switch {
	case i.dialog.Active():
		dialog = i.dialog.Render(i.cfg.Theme, cols)
	case i.modelDialog.Active():
		dialog = i.modelDialog.Render(i.cfg.Theme, cols)
	case i.sessionDialog.Active():
		dialog = i.sessionDialog.Render(i.cfg.Theme, cols)
	}

	// Slash-command autocomplete: popup above the status line, only
	// when the editor starts with "/" and no dialog is already open.
	var suggest []string
	currentInput := i.ed.Value()
	if len(dialog) == 0 && i.suggest.Active(currentInput) && !i.busy {
		suggest = i.suggest.Render(currentInput, i.cfg.Theme, cols)
	}

	// Busy prefix shown at the far left of the status bar.
	var busyPrefix string
	if i.busy {
		busyPrefix = fmt.Sprintf("%s %s · %s",
			i.cfg.Theme.FG256(i.cfg.Theme.Spinner, i.spin.Frame()),
			i.cfg.Theme.FG256(i.cfg.Theme.Spinner, i.spin.Message()),
			i.cfg.Theme.FG256(i.cfg.Theme.Muted, i.spin.Elapsed().String()),
		)
	}

	ctxMax := 0
	if m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil {
		ctxMax = m.ContextWindow
	}
	status := tui.StatusBar(tui.StatusBarParams{
		Theme:          i.cfg.Theme,
		Provider:       i.cfg.Provider,
		Model:          i.cfg.Model,
		Busy:           i.busy,
		BusyPrefix:     busyPrefix,
		CWD:            i.cfg.CWD,
		Locked:         i.cfg.Sandbox.Locked(),
		Usage:          i.cumUsage,
		Subscription:   i.cfg.AuthMethod == "oauth",
		ContextUsed:    i.lastCtxInput,
		ContextMax:     ctxMax,
		AutoCompacting: i.autoCompacting,
		Cols:           cols,
	})
	edLines, curR, curC := i.ed.Render(cols)

	// "Sliding in" chips for messages the user typed while a turn is
	// in flight. Shown directly above the status bar so they're close
	// to the editor but don't push the chat around.
	var queue []string
	if len(i.queued) > 0 {
		for _, q := range i.queued {
			label := i.cfg.Theme.FG256(i.cfg.Theme.Accent, "▸ sliding in: ")
			text := truncateLine(q, cols-15)
			queue = append(queue, label+i.cfg.Theme.FG256(i.cfg.Theme.Muted, text))
		}
	}

	// Bottom-sticky sections (always visible, never scroll).
	bottom := make([]string, 0, len(dialog)+len(suggest)+len(queue)+len(edLines)+1)
	bottom = append(bottom, dialog...)
	bottom = append(bottom, suggest...)
	bottom = append(bottom, queue...)
	bottom = append(bottom, status)
	bottom = append(bottom, edLines...)

	_, rows := i.cfg.Terminal.Size()
	chatRows := rows - len(bottom)
	if chatRows < 1 {
		chatRows = 1
	}

	// Apply scroll offset to the chat slice.
	maxOffset := len(chat) - chatRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if i.scrollOffset > maxOffset {
		i.scrollOffset = maxOffset
	}
	if i.scrollOffset < 0 {
		i.scrollOffset = 0
	}

	var visibleChat []string
	if len(chat) <= chatRows {
		visibleChat = chat
	} else {
		end := len(chat) - i.scrollOffset
		start := end - chatRows
		if start < 0 {
			start = 0
		}
		visibleChat = chat[start:end]
	}

	// A tiny "scrolled up" indicator in the top-right of the chat pane
	// so you know you're not at the bottom.
	if i.scrollOffset > 0 && len(visibleChat) > 0 {
		note := i.cfg.Theme.FG256(i.cfg.Theme.Muted,
			fmt.Sprintf("  ↑ %d lines more below (end to jump)", i.scrollOffset))
		visibleChat = append([]string{note}, visibleChat...)
		if len(visibleChat) > chatRows {
			visibleChat = visibleChat[:chatRows]
		}
	}

	frame := make([]string, 0, len(visibleChat)+len(bottom))
	frame = append(frame, visibleChat...)
	frame = append(frame, bottom...)

	cursorRow := len(visibleChat) + len(dialog) + len(suggest) + len(queue) + 1 + curR
	cursorCol := curC
	i.rend.Draw(frame, cursorRow, cursorCol)
}

// truncateLine shortens s so it fits within n display cells, with an
// ellipsis if trimmed. Used by the "sliding in" chips so a pasted
// novel doesn't blow past the status line.
func truncateLine(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Collapse newlines — chips are single line.
	s = strings.ReplaceAll(s, "\n", " ↩ ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}

func (i *Interactive) handleKey(ctx context.Context, k tui.Key) (done bool) {
	// Dialogs consume keys while open (except ctrl+c, which always closes them).
	if i.dialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.dialog.Close()
			if i.cfg.AuthManager != nil {
				i.cfg.AuthManager.CancelOAuth()
			}
			return false
		}
		act := i.dialog.HandleKey(k)
		if act.StartAPIKey {
			i.startAPIKeyFlow(act.Provider)
		}
		if act.StartOAuth {
			i.startOAuthFlow(act.Provider)
		}
		return false
	}
	if i.modelDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.modelDialog.Close()
			return false
		}
		act := i.modelDialog.HandleKey(k)
		if act.Select {
			i.applyModelSelection(act.Provider, act.Model)
		}
		return false
	}
	if i.sessionDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.sessionDialog.Close()
			return false
		}
		act := i.sessionDialog.HandleKey(k)
		if act.Select {
			i.applySessionSelection(act.Path)
		}
		return false
	}

	// Global keys.
	switch k.Kind {
	case tui.KeyCtrlC:
		if i.busy && i.cancelTurn != nil {
			i.cancelTurn()
			return false
		}
		return true
	case tui.KeyEsc:
		// Esc interrupts a running turn. When idle, fall through so the
		// editor can clear itself.
		if i.busy && i.cancelTurn != nil {
			i.cancelTurn()
			return false
		}
	case tui.KeyCtrlD:
		if i.ed.IsEmpty() && !i.busy {
			return true
		}
	case tui.KeyCtrlL:
		i.rend.Clear()
		i.invalidate()
		return false
	case tui.KeyCtrlO:
		// Toggle expansion of collapsed tool results. Affects every tool
		// call in the transcript — press again to re-collapse.
		i.mu.Lock()
		i.view.ExpandAll = !i.view.ExpandAll
		i.mu.Unlock()
		i.invalidate()
		return false
	case tui.KeyPageUp:
		i.scrollBy(+i.chatPage())
		return false
	case tui.KeyPageDown:
		i.scrollBy(-i.chatPage())
		return false
	case tui.KeyUp:
		// Wheel-up in alt screen sends Up arrows on most terminals. When the
		// editor is empty and we're not busy, use that for chat scroll-up.
		if i.ed.IsEmpty() && !i.busy {
			i.scrollBy(+3)
			return false
		}
	case tui.KeyDown:
		if i.ed.IsEmpty() && !i.busy {
			if i.scrollOffset > 0 {
				i.scrollBy(-3)
			}
			return false
		}
	}

	// Note: we intentionally do NOT gate the editor on i.busy here.
	// Typing while the agent is working is supported — submitted
	// messages are queued and delivered as follow-up turns when the
	// current turn ends. See the submit handler below.

	if k.Kind == tui.KeyEnter && k.Alt {
		i.ed.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '\n', Alt: true})
		return false
	}

	// Slash suggestions: intercept up/down/tab/enter when the popup is visible.
	if i.suggest.Active(i.ed.Value()) {
		switch k.Kind {
		case tui.KeyUp:
			i.suggest.Up()
			return false
		case tui.KeyDown:
			i.suggest.Down()
			return false
		case tui.KeyTab:
			if name := i.suggest.Selection(i.ed.Value()); name != "" {
				i.ed.SetValue(name)
				i.suggest.Reset()
			}
			return false
		case tui.KeyEnter:
			// Enter on an ambiguous or partial slash prefix: complete to the
			// currently highlighted command and run it. That way typing
			// "/lo" + enter picks whichever of /login or /logout is selected
			// in the popup instead of submitting "/lo" as unknown. Also
			// clear the editor so the command doesn't linger after the
			// dialog opens/closes.
			if name := i.suggest.Selection(i.ed.Value()); name != "" {
				i.ed.PushHistory(name)
				i.ed.Clear()
				i.suggest.Reset()
				return i.runSlash(ctx, name)
			}
		case tui.KeyEsc:
			i.ed.Clear()
			i.suggest.Reset()
			return false
		}
	}

	if submit := i.ed.HandleKey(k); submit {
		text := strings.TrimRight(i.ed.Value(), "\n")
		if text == "" {
			return false
		}
		i.ed.PushHistory(text)
		i.ed.Clear()
		i.suggest.Reset()

		if looksLikeSlashCommand(text) {
			if !isKnownSlashCommand(text) {
				head := text
				if idx := strings.IndexAny(text, " \t"); idx >= 0 {
					head = text[:idx]
				}
				i.mu.Lock()
				i.statusErr = "unknown command " + head + " — type /help to see the list"
				i.statusOK = ""
				i.mu.Unlock()
				return false
			}
			// Slash commands need a quiet state (they may swap models,
			// compact the transcript, open dialogs, etc). Refuse while
			// a turn is in flight — esc / ctrl+c cancels first.
			i.mu.Lock()
			busy := i.busy
			i.mu.Unlock()
			if busy {
				i.mu.Lock()
				i.statusErr = "cancel the current turn (esc) before running a slash command"
				i.mu.Unlock()
				return false
			}
			return i.runSlash(ctx, text)
		}

		if i.agent == nil {
			i.mu.Lock()
			i.statusErr = "not logged in. type /login first."
			i.mu.Unlock()
			return false
		}
		// If a turn is already in flight, queue this prompt instead of
		// starting a second one. The drain loop at the end of startTurn
		// will pick it up when the current turn finishes.
		i.mu.Lock()
		if i.busy {
			i.queued = append(i.queued, text)
			i.mu.Unlock()
			i.invalidate()
			return false
		}
		i.mu.Unlock()
		i.startTurn(ctx, text)
	}
	return false
}

func (i *Interactive) runSlash(ctx context.Context, cmd string) (done bool) {
	parts := strings.Fields(cmd)
	switch parts[0] {
	case "/exit":
		return true
	case "/clear":
		if i.agent != nil {
			i.agent.SetMessages(nil)
		}
		i.mu.Lock()
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.statusErr = ""
		i.statusOK = ""
		i.helpBlock = nil
		i.mu.Unlock()
	case "/help":
		i.mu.Lock()
		i.helpBlock = renderHelpBlock(i.cfg.Theme, i.lastCols())
		i.statusErr = ""
		i.statusOK = ""
		i.mu.Unlock()
	case "/login":
		i.dialog.Open()
	case "/logout":
		target := "all"
		if len(parts) >= 2 {
			target = parts[1]
		}
		i.doLogout(target)
	case "/model":
		if len(parts) >= 2 {
			i.applyModelSelection("", parts[1])
		} else {
			i.modelDialog.Open(i.cfg.Model)
		}
	case "/sessions":
		i.sessionDialog.Open(i.cfg.ZotHome, i.cfg.CWD)
	case "/compact":
		i.runCompact(ctx, false)
	case "/lock":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Lock()
		i.mu.Lock()
		i.statusOK = "locked to " + i.cfg.CWD + " (tools cannot touch paths outside this directory)"
		i.statusErr = ""
		i.mu.Unlock()
	case "/unlock":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Unlock()
		i.mu.Lock()
		i.statusOK = "unlocked"
		i.statusErr = ""
		i.mu.Unlock()
	default:
		i.mu.Lock()
		i.statusErr = "unknown command: " + parts[0]
		i.mu.Unlock()
	}
	return false
}

// doLogout clears credentials for the given provider (or all providers)
// from auth.json. If the active agent was using those credentials, it
// is torn down so the user is forced through /login before their next
// prompt.
//
// target: "anthropic" | "openai" | "all"
func (i *Interactive) doLogout(target string) {
	if i.cfg.AuthManager == nil {
		i.mu.Lock()
		i.statusErr = "no auth manager configured"
		i.mu.Unlock()
		return
	}
	store := i.cfg.AuthManager.Store()
	if store == nil {
		i.mu.Lock()
		i.statusErr = "auth store is not available"
		i.mu.Unlock()
		return
	}

	var providers []string
	switch target {
	case "", "all":
		providers = []string{"anthropic", "openai"}
	case "anthropic", "openai":
		providers = []string{target}
	default:
		i.mu.Lock()
		i.statusErr = "unknown provider: " + target + " (use anthropic, openai, or all)"
		i.mu.Unlock()
		return
	}

	var errs []string
	clearedCurrent := false
	for _, p := range providers {
		if err := store.Clear(p); err != nil {
			errs = append(errs, p+": "+err.Error())
			continue
		}
		if p == i.cfg.Provider {
			clearedCurrent = true
		}
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if len(errs) > 0 {
		i.statusErr = "logout errors: " + strings.Join(errs, "; ")
		return
	}
	i.statusErr = ""
	if clearedCurrent {
		// The running agent was using a credential we just wiped. Drop
		// it so prompts can't go out with the stale client, and hint at
		// /login.
		i.agent = nil
		i.statusOK = "logged out of " + strings.Join(providers, ", ") + ". type /login to sign back in."
	} else {
		i.statusOK = "logged out of " + strings.Join(providers, ", ")
	}
}

func (i *Interactive) startAPIKeyFlow(provider string) {
	url, err := i.cfg.AuthManager.StartAPIKey(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.ShowWaiting(url)
}

func (i *Interactive) startOAuthFlow(provider string) {
	url, err := i.cfg.AuthManager.StartOAuth(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.ShowWaiting(url)
}

// applyModelSelection switches the active model (and provider, if the
// new model belongs to a different one). It rebuilds the underlying
// client when needed so the provider wire-protocol matches.
// applySessionSelection loads the given session via the cli-provided callback.
func (i *Interactive) applySessionSelection(path string) {
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusErr = "session loading is not wired in this build"
		i.mu.Unlock()
		return
	}
	if err := i.cfg.LoadSession(path); err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	i.statusOK = "resumed session: " + path
	i.statusErr = ""
	i.scrollOffset = 0
	i.mu.Unlock()
}

func (i *Interactive) applyModelSelection(prov, model string) {
	if model == "" {
		return
	}
	m, err := provider.FindModel(prov, model)
	if err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return
	}
	// Same provider? Just swap the model on the existing agent.
	if i.agent != nil && m.Provider == i.cfg.Provider {
		i.mu.Lock()
		i.cfg.Model = m.ID
		i.agent.Model = m.ID
		i.statusOK = "model: " + m.ID
		i.statusErr = ""
		i.mu.Unlock()
		if i.cfg.PersistModel != nil {
			i.cfg.PersistModel(i.cfg.Provider, m.ID)
		}
		return
	}
	// Different provider: rebuild agent (needs credentials for target provider).
	if i.cfg.BuildAgentFor == nil {
		i.mu.Lock()
		i.statusErr = "cannot switch provider: no builder configured"
		i.mu.Unlock()
		return
	}
	ag, p, md, err := i.cfg.BuildAgentFor(m.Provider, m.ID)
	if err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	i.agent = ag
	i.cfg.Provider = p
	i.cfg.Model = md
	i.statusOK = "switched to " + p + " / " + md
	i.statusErr = ""
	i.mu.Unlock()
	if i.cfg.PersistModel != nil {
		i.cfg.PersistModel(p, md)
	}
}

func (i *Interactive) handleAuthEvent(ev auth.Event) {
	switch ev.Kind {
	case "started":
		i.dialog.ShowWaiting(ev.URL)
	case "browser_open":
		// no-op
	case "error":
		i.dialog.ShowResult(false, ev.Message)
	case "success":
		// Rebuild the agent with the fresh credential.
		ag, prov, model, err := i.cfg.BuildAgent()
		if err != nil {
			i.dialog.ShowResult(false, err.Error())
			return
		}
		i.mu.Lock()
		i.agent = ag
		i.cfg.Provider = prov
		i.cfg.Model = model
		i.statusErr = ""
		i.statusOK = "logged in to " + ev.Provider + " via " + ev.Method
		i.mu.Unlock()
		i.dialog.ShowResult(true, "")
	}
}

// runCompact invokes core.Agent.Compact and reflects the progress in
// the tui. It runs in a goroutine so the ui stays responsive; esc/ctrl+c
// cancel via the same cancelTurn channel used for normal turns.
//
// When auto is true the spinner message is pinned to "condensing
// history" and the status bar surfaces "(auto)" next to the context
// percentage so it's obvious the system triggered this, not the user.
func (i *Interactive) runCompact(parent context.Context, auto bool) {
	if i.agent == nil {
		i.mu.Lock()
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	i.mu.Lock()
	i.busy = true
	if auto {
		i.spin.StartFixed("condensing history")
		i.autoCompacting = true
		i.statusOK = "condensing history… (esc to cancel)"
	} else {
		i.spin.Start()
		i.statusOK = "compacting..."
	}
	i.cancelTurn = cancel
	i.statusErr = ""
	i.streaming.Reset()
	i.streamOn = true
	i.scrollOffset = 0
	i.helpBlock = nil
	i.mu.Unlock()
	i.invalidate()

	go func() {
		sink := func(delta string) {
			i.mu.Lock()
			i.streaming.WriteString(delta)
			i.mu.Unlock()
			i.invalidate()
		}
		summary, err := i.agent.Compact(ctx, 4, sink)
		i.mu.Lock()
		i.busy = false
		i.streamOn = false
		i.streaming.Reset()
		i.cancelTurn = nil
		i.autoCompacting = false
		switch {
		case err != nil && ctx.Err() != nil:
			i.statusErr = ""
			if auto {
				i.statusOK = "auto-condense cancelled"
			} else {
				i.statusOK = "compaction cancelled"
			}
		case err != nil:
			i.statusErr = "compaction failed: " + err.Error()
			i.statusOK = ""
		default:
			i.statusErr = ""
			i.statusOK = fmt.Sprintf("compacted transcript (%d chars of summary)", len(summary))
			i.lastCtxInput = 0 // reset; next turn will get a fresh measurement
			i.toolCalls = map[string]*tui.ToolCallView{}
			i.toolOrder = nil
		}
		i.mu.Unlock()
		i.invalidate()
	}()
}

func (i *Interactive) startTurn(parent context.Context, prompt string) {
	if i.agent == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	i.mu.Lock()
	i.busy = true
	i.spin.Start()
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	i.streaming.Reset()
	i.streamOn = true
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.scrollOffset = 0 // jump back to the bottom on new turn
	i.helpBlock = nil  // hide the help block once the user asks something
	i.mu.Unlock()
	i.invalidate()

	sink := func(ev core.AgentEvent) {
		i.handleEvent(ev)
		i.invalidate()
	}

	go func() {
		err := i.agent.Prompt(ctx, prompt, nil, sink)
		i.mu.Lock()
		i.busy = false
		i.streamOn = false
		i.cancelTurn = nil
		if err != nil && ctx.Err() == nil {
			i.statusErr = err.Error()
		}
		// Pop the next queued message, if any, and relaunch.
		var next string
		var hasNext bool
		if len(i.queued) > 0 && ctx.Err() == nil && err == nil {
			next, i.queued = i.queued[0], i.queued[1:]
			hasNext = true
		}
		// If the turn was cancelled or errored, drop the queue so the
		// user isn't bombarded with stale messages after an interrupt.
		if ctx.Err() != nil || err != nil {
			i.queued = nil
		}
		// Decide whether the next thing to do is an auto-compaction.
		// Only fires when the turn completed cleanly AND the queue is
		// empty (otherwise a queued message would race the condense).
		shouldAutoCompact := !hasNext && err == nil && ctx.Err() == nil && i.shouldAutoCompactLocked()
		i.mu.Unlock()
		i.invalidate()
		parent := i.runCtx
		if parent == nil {
			parent = context.Background()
		}
		switch {
		case hasNext:
			i.startTurn(parent, next)
		case shouldAutoCompact:
			i.runCompact(parent, true)
		}
	}()
}

// autoCompactThreshold is the context-window fraction at which the
// agent will auto-compact after a turn ends. 0.85 leaves enough
// headroom for one more user prompt + response before we bump the
// hard limit.
const autoCompactThreshold = 0.85

// shouldAutoCompactLocked reports whether the last turn pushed context
// usage past the auto-compact threshold. Must be called with i.mu
// held; it reads lastCtxInput and the current model's context window.
func (i *Interactive) shouldAutoCompactLocked() bool {
	if i.agent == nil {
		return false
	}
	if i.autoCompacting {
		return false
	}
	m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model)
	if err != nil || m.ContextWindow <= 0 {
		return false
	}
	if i.lastCtxInput <= 0 {
		return false
	}
	return float64(i.lastCtxInput)/float64(m.ContextWindow) >= autoCompactThreshold
}

func (i *Interactive) handleEvent(ev core.AgentEvent) {
	i.mu.Lock()
	defer i.mu.Unlock()
	switch e := ev.(type) {
	case core.EvTextDelta:
		i.streaming.WriteString(e.Delta)
	case core.EvAssistantMessage:
		i.streaming.Reset()
		i.streamOn = false
		if i.cfg.OnAssistant != nil {
			i.cfg.OnAssistant(e.Message)
		}
	case core.EvToolCall:
		tcv := &tui.ToolCallView{
			ID:   e.ID,
			Name: e.Name,
			Args: shortArgs(e.Args),
		}
		i.toolCalls[e.ID] = tcv
		i.toolOrder = append(i.toolOrder, e.ID)
	case core.EvToolResult:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Done = true
			tc.Error = e.Result.IsError
			var text strings.Builder
			for _, c := range e.Result.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(tb.Text)
				}
			}
			tc.Result = text.String()
		}
		if i.cfg.OnToolResult != nil {
			i.cfg.OnToolResult(e.ID, e.Result)
		}
	case core.EvUsage:
		i.cumUsage = e.Cumulative
		if e.Usage.InputTokens > 0 {
			i.lastCtxInput = e.Usage.InputTokens + e.Usage.CacheReadTokens + e.Usage.CacheWriteTokens
		}
	case core.EvTurnEnd:
		if e.Stop == provider.StopAborted {
			// Aborted turn: discard the partial streaming text (it is not
			// persisted in the transcript) and clear any transient error.
			i.streaming.Reset()
			i.streamOn = false
			i.statusErr = ""
			i.statusOK = "cancelled"
			return
		}
		if e.Err != nil && !strings.Contains(e.Err.Error(), "context canceled") {
			i.statusErr = e.Err.Error()
		}
	}
}

// Agent returns the current agent, if any. Used by cli.go to flush the
// final transcript to the session file.
func (i *Interactive) Agent() *core.Agent {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.agent
}

func shortArgs(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"path", "file_path", "command"} {
			if s, ok := m[k].(string); ok {
				if len(s) > 60 {
					s = s[:57] + "..."
				}
				return s
			}
		}
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// silence unused import in some build configs
var _ = fmt.Sprintf
