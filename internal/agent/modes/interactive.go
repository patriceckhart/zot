package modes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/agent/extensions"
	"github.com/patriceckhart/zot/internal/agent/modes/telegram"
	"github.com/patriceckhart/zot/internal/agent/tools"
	"github.com/patriceckhart/zot/internal/auth"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/extproto"
	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/skills"
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

	// ZotHome is the root directory for sessions/, used by /sessions
	// and the update-check cache.
	ZotHome string

	// Version is the binary's current version (from main.version).
	// Used only for display; the update check itself is done outside
	// this package to avoid an import cycle.
	Version string

	// UpdateInfoChan is an optional channel that delivers the result
	// of the github-release update check. Interactive reads at most
	// one value, drops it if the check reported nothing, and otherwise
	// surfaces a yellow "update available" banner at the top of the
	// chat. Nil channel = no banner, no startup cost.
	UpdateInfoChan <-chan UpdateInfo

	// Sandbox is the shared sandbox pointer. Toggled by /jail and /unjail.
	Sandbox *tools.Sandbox

	// LoadSession swaps the current session for the one at path. The
	// callback returns the new agent message slice so the TUI can invalidate.
	LoadSession func(path string) error

	// CurrentSessionPath returns the path of the live session file
	// on disk (the one every AppendMessage writes to). Used by
	// /session export so the exporter ships the exact bytes on
	// disk. Returns an empty string when --no-session is set or
	// no session is open.
	CurrentSessionPath func() string

	// FlushSession writes any in-memory agent messages to the
	// session file that haven't been persisted yet. Called by
	// /session export right before reading the file so the
	// exported bytes reflect the full current conversation, not
	// just the rows the agent happened to write synchronously.
	// The default WriteNewTranscript-at-exit strategy means most
	// of a running session lives only in memory until the tui
	// closes; without a flush hook, /session export writes a
	// file that only has the meta row.
	FlushSession func()

	// PersistModel is called whenever the user switches model or provider.
	// It should update config.json and (if there's an active session)
	// write a new meta row so resume picks up the same model.
	PersistModel func(providerName, model string)

	OnAssistant  func(m provider.Message)
	OnToolResult func(id string, r core.ToolResult)

	// Extensions, if non-nil, lets users invoke extension-registered
	// slash commands. Commands declared by extensions are looked up
	// AFTER the built-in catalog so a built-in name always wins.
	Extensions *extensions.Manager

	// SkillSnapshot, if non-nil, returns the current list of
	// discovered SKILL.md files. Re-invoked each time /skills opens
	// so the picker reflects edits made during the session.
	SkillSnapshot func() []*skills.Skill

	// ChangelogChan, if non-nil, delivers release-notes for the
	// current binary version once at startup. Interactive opens a
	// dismissible overlay when the channel produces a non-empty
	// body. Receiver fires at most once.
	ChangelogChan <-chan ChangelogPayload

	// OnChangelogDismiss, if non-nil, is called once the user
	// closes the changelog overlay. The cli wires this to a
	// MarkChangelogShown call so the same version doesn't show
	// again on the next launch.
	OnChangelogDismiss func()

	// NoYolo is true when --no-yolo was passed. Interactive opens
	// a confirmation dialog before every tool call and blocks the
	// tool until the user picks yes / always-this-tool /
	// always-all / no. When false (default), tools run freely.
	NoYolo bool

	// ConfirmGate is the session-scoped gate wrapping this
	// interactive's Confirmer. When non-nil, /yolo can call
	// AllowAll() on it to disable confirmation for the rest of the
	// session. When nil (yolo mode), /yolo reports that there's
	// nothing to disable.
	ConfirmGate *core.ConfirmGate
}

// ChangelogPayload mirrors agent.ChangelogInfo without the import
// cycle. The cli builds one from the http response, the tui opens
// the overlay when one arrives.
type ChangelogPayload struct {
	Version string
	Body    string
	URL     string
}

// Interactive is the TUI chat loop.
type Interactive struct {
	cfg  InteractiveConfig
	view *tui.View
	ed   *tui.Editor
	rend *tui.Renderer

	mu        sync.Mutex
	agent     *core.Agent
	streaming strings.Builder // what's currently painted on screen
	streamOn  bool

	// streamPending is the runes buffered after each EvTextDelta that
	// haven't yet been promoted into `streaming` for rendering. It
	// exists because some provider paths (notably Anthropic via the
	// oauth/subscription channel) coalesce the model's output into a
	// few fat chunks instead of drip-streaming. Painting those fat
	// chunks verbatim looks like the summary "just appears". The
	// paintPace goroutine drains a handful of runes per tick from
	// this buffer into `streaming`, giving every path the same
	// typewriter feel regardless of upstream chunk size.
	streamPending []rune
	// streamFlushPending is set when EvAssistantMessage fires while
	// streamPending still has unrendered runes. The ticker flushes
	// them, then closes out the stream (clearing flags, resetting
	// buffers) so the final paint matches the on-disk message.
	streamFlushPending bool
	toolCalls          map[string]*tui.ToolCallView
	toolOrder          []string
	statusErr          string
	statusOK           string
	helpBlock          []string // rendered above the chat when /help was typed
	cumUsage           provider.Usage
	lastCtxInput       int // input_tokens of the most recent turn — approximates current context size
	busy               bool
	dirty              chan struct{}
	cancelTurn         context.CancelFunc
	scrollOffset       int // rows from the bottom; 0 = pinned to latest

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

	// updateInfo is the result of the async update check. Zero value
	// while the check hasn't completed or nothing is available.
	updateInfo UpdateInfo

	dialog            *loginDialog
	modelDialog       *modelDialog
	sessionDialog     *sessionDialog
	jumpDialog        *jumpDialog
	btwDialog         *btwDialog
	skillsDialog      *skillsDialog
	changelogDialog   *changelogDialog
	confirmDialog     *confirmDialog
	logoutDialog      *logoutDialog
	telegramDialog    *telegramDialog
	telegramBridge    *telegram.Bridge
	sessionOpsDialog  *sessionOpsDialog
	sessionTreeDialog *sessionTreeDialog
	extPanel          *extPanelDialog

	// pendingFork is true when the user ran /session fork: the next
	// jump-picker selection should branch off that message instead
	// of scrolling. Flag resets after the action fires or the dialog
	// is dismissed, so repeated /jump calls don't turn into forks.
	pendingFork bool
	suggest     *slashSuggester
	fileSuggest *fileSuggester
	spin        *spinner

	// parkedTurn is the 1-based turn number the viewport is currently
	// scrolled to by /jump. 0 = not parked, showing the tail as usual.
	// Rendered as a muted footer at the bottom of the chat so users
	// don't forget they're looking at history.
	parkedTurn  int
	parkedTotal int

	// lastCtrlC is when the user last pressed ctrl+c. The first press
	// clears the editor / cancels a turn / shows a hint; a second press
	// within ctrlCExitWindow exits. Mirrors the python-repl convention.
	lastCtrlC time.Time

	// welcomeStart is when the interactive run began. The welcome
	// banner shows the binary version for welcomeVersionDuration
	// after this point and reverts to plain text after.
	welcomeStart time.Time

	// extNotes are one-shot styled lines pushed by extensions via
	// Notify / Display. They live above the editor (just below the
	// transcript) until cleared by /clear or another reset.
	extNotes []string
}

// welcomeVersionDuration is how long the welcome banner shows the
// version suffix before reverting to the plain headline. 1.5s is
// enough to read at a glance and keeps the splash short.
const welcomeVersionDuration = 1500 * time.Millisecond

// NewInteractive constructs an Interactive from cfg.
func NewInteractive(cfg InteractiveConfig) *Interactive {
	i := &Interactive{
		cfg: cfg,
		view: &tui.View{
			Theme:      cfg.Theme,
			ImageProto: tui.DetectImageProtocol(),
		},
		ed:                tui.NewEditor(cfg.Theme.FG256(cfg.Theme.Accent, "▌ ")),
		rend:              tui.NewRenderer(cfg.Terminal),
		toolCalls:         map[string]*tui.ToolCallView{},
		dirty:             make(chan struct{}, 8),
		dialog:            newLoginDialog(),
		modelDialog:       newModelDialog(),
		sessionDialog:     newSessionDialog(),
		jumpDialog:        newJumpDialog(),
		btwDialog:         newBtwDialog(),
		skillsDialog:      newSkillsDialog(),
		changelogDialog:   newChangelogDialog(),
		confirmDialog:     newConfirmDialog(),
		logoutDialog:      newLogoutDialog(),
		telegramDialog:    newTelegramDialog(),
		sessionOpsDialog:  newSessionOpsDialog(),
		sessionTreeDialog: newSessionTreeDialog(),
		extPanel:          newExtPanelDialog(),
		suggest:           newSlashSuggester(),
		fileSuggest:       newFileSuggester(),
		spin:              newSpinner(),
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
	defer func() {
		if i.telegramBridge != nil {
			i.telegramBridge.Stop()
		}
	}()

	_, _ = term.Write([]byte(tui.SeqBracketedPasteOn))
	_, _ = term.Write([]byte(tui.SeqAltScreenOn))
	defer term.Write([]byte(tui.SeqAltScreenOff + tui.SeqBracketedPasteOff + tui.SeqShowCursor))

	// Streaming pacer: drains buffered text deltas at a steady rate
	// so typewriter feel is identical across providers regardless of
	// upstream chunk size. Starts here so it lives for the whole
	// session and exits with ctx.
	go i.runStreamPacer(ctx)

	cols, rows := term.Size()
	i.rend.Resize(cols, rows)
	term.OnResize(func() {
		c, r := term.Size()
		i.rend.Resize(c, r)
		// Force an immediate redraw on resize. The throttled invalidate
		// path is fine for animation, but a window resize is a discrete
		// user action where any visible delay (or stale frame) reads as
		// brokenness. redraw() is mutex-safe; the worst that happens is
		// a duplicate paint if the throttler is mid-flight, which is
		// invisible.
		i.redraw()
	})

	if i.cfg.InitialInput != "" {
		i.ed.SetValue(i.cfg.InitialInput)
	}

	// Stamp the welcome time and schedule a one-shot redraw at the
	// expiry so the version suffix disappears on its own even if the
	// user hasn't typed anything yet.
	i.welcomeStart = time.Now()
	time.AfterFunc(welcomeVersionDuration, i.invalidate)

	// If the agent was constructed with a pre-loaded transcript
	// (--continue, --resume, --session) park the viewport on the
	// most recent turn so the user lands looking at where the
	// previous session left off rather than at the bottom of an
	// already-rendered final reply.
	if i.agent != nil {
		if msgs := i.agent.Messages(); len(msgs) > 0 {
			i.scrollToLastTurn(msgs)
		}
	}

	// No credential at startup? Auto-open the login dialog, and mark
	// the status line. The user can Esc out of the dialog if they
	// want to dismiss it (e.g. to check /help or /exit first).
	if i.agent == nil {
		i.statusErr = "not logged in. pick a login method below or press esc to dismiss."
		i.dialog.Open(i.cfg.ZotHome)
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
			// Redrawing right now subsumes any pending redraw, so clear
			// the throttle state. Without this, a pending flag stays
			// stuck at true and subsequent invalidate() calls within
			// redrawMinInterval get dropped — which is exactly how the
			// final "turn finished" frame went missing until the user
			// nudged the ui by typing or scrolling.
			if pendingTimer != nil {
				pendingTimer.Stop()
			}
			pendingRedraw = false
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
				// Poke the dirty channel so the main loop wakes and
				// drains the pending redraw on its own goroutine. We
				// can't call drainPending here directly — it touches
				// closure state shared with the main loop.
				i.invalidate()
			})
		} else {
			pendingTimer.Reset(wait)
		}
	}

	i.invalidate()

	updates := i.cfg.UpdateInfoChan  // nil-safe; nil channel blocks forever in select
	changelog := i.cfg.ChangelogChan // single-shot, see case below

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
		case info, ok := <-updates:
			if ok && info.Available {
				i.mu.Lock()
				i.updateInfo = info
				i.mu.Unlock()
				i.invalidate()
			}
			updates = nil // single-shot; subsequent iterations skip this case
		case cl, ok := <-changelog:
			if ok && cl.Body != "" {
				i.changelogDialog.Open(cl.Version, cl.URL, cl.Body)
				i.invalidate()
			}
			changelog = nil // single-shot
		case <-i.dirty:
			requestRedraw()
		case <-tick.C:
			// Always drain a pending redraw on the tick. This is the
			// safety net that catches the case where the dirty channel
			// was saturated when the final "turn finished" invalidate
			// fired, or where the throttle scheduled a deferred redraw
			// and the AfterFunc-driven invalidate got dropped on a
			// full channel.
			drainPending()
			// Only force a periodic redraw when something is actually
			// animating (the main spinner during a busy turn, or the
			// btw side-chat spinner while it's awaiting a response).
			// Static pickers (model, session, jump, etc.) don't need
			// the tick and firing it cancels the terminal's cursor
			// blink inside dialogs that host their own editor (btw),
			// because each frame re-emits hide-cursor + show-cursor.
			if i.busy || i.btwDialog.Loading() {
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
// Clearing the parked-turn label when we're back at the bottom means
// the "viewing turn N" footer goes away automatically as soon as you
// scroll back to the live tail.
func (i *Interactive) scrollBy(delta int) {
	i.mu.Lock()
	i.scrollOffset += delta
	if i.scrollOffset < 0 {
		i.scrollOffset = 0
	}
	if i.scrollOffset == 0 {
		i.parkedTurn = 0
		i.parkedTotal = 0
	}
	if i.rend != nil {
		// VS Code's terminal is especially prone to leaving stray
		// wrapped-character fragments behind during scroll-driven
		// viewport changes. Force a full repaint on scroll, but
		// avoid a whole-screen clear because that visibly flickers.
		i.rend.Invalidate()
	}
	i.mu.Unlock()
	i.invalidate()
}

// scrollToBottom pins the view to the latest content.
func (i *Interactive) scrollToBottom() {
	i.mu.Lock()
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	if i.rend != nil {
		i.rend.Invalidate()
	}
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) redraw() {
	i.mu.Lock()
	defer i.mu.Unlock()

	cols, _ := i.cfg.Terminal.Size()
	if i.agent != nil {
		i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
	} else {
		i.view.Messages = nil
	}
	// Pacer flush: while the streaming pacer is still draining the
	// buffer (i.e. EvAssistantMessage already fired but more runes
	// are queued), the final assistant message is already in
	// i.agent.Messages() in full. Painting it in the transcript
	// AND the streaming block at the same time shows the user the
	// complete text immediately — which defeats the whole pacer.
	// Hide the last message until the pacer catches up; once the
	// flush-pending latch clears, the message is revealed (the
	// streaming block disappears the same frame because streamOn
	// flips off, so the transition is seamless).
	if i.streamFlushPending && len(i.view.Messages) > 0 {
		i.view.Messages = i.view.Messages[:len(i.view.Messages)-1]
	}
	i.view.Streaming = i.streaming.String()
	i.view.StreamingActive = i.streamOn
	// Guard against the narrow race where EvAssistantMessage has
	// just promoted a streaming reply into the transcript but a
	// render tick hasn't flipped streamOn off yet. Without the
	// guard, the same text would appear twice (once as the
	// in-flight streaming block, once as the last transcript
	// message). We detect the duplicate strictly: the last
	// assistant message's visible text must equal the streaming
	// buffer. Just matching on role is too broad — it also hides
	// the next round's typewriter streaming after a tool turn,
	// because the last transcript message is always assistant
	// (the tool-use block) until the follow-up summary lands.
	if i.streamOn && i.streaming.Len() > 0 {
		if n := len(i.view.Messages); n > 0 && i.view.Messages[n-1].Role == provider.RoleAssistant {
			if assistantText(i.view.Messages[n-1]) == i.streaming.String() {
				i.view.StreamingActive = false
			}
		}
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
	// The version suffix is shown for welcomeVersionDuration after
	// startup, then drops off automatically.
	if len(i.view.Messages) == 0 && !i.streamOn && len(i.toolOrder) == 0 {
		showVer := !i.welcomeStart.IsZero() && time.Since(i.welcomeStart) < welcomeVersionDuration
		chat = append(welcomeBanner(i.cfg.Theme, i.cfg.Version, showVer), chat...)
	}

	// Update-available banner: prepended above everything else so it's
	// the first thing the user sees when opening a new zot session.
	// Once rendered, it stays until the user updates to a newer
	// version — we don't persist a "dismissed" flag because this is
	// cheap and re-showing it is how most users remember to update.
	if i.updateInfo.Available {
		banner := renderUpdateBanner(i.cfg.Theme, i.updateInfo, cols)
		chat = append(banner, chat...)
	}

	// /help block: appended to the transcript so it appears at the
	// bottom of the chat area (right above the status bar / editor).
	// Prepending it would push long conversations off the top of the
	// viewport, which users would miss entirely.
	if len(i.helpBlock) > 0 {
		chat = append(chat, i.helpBlock...)
	}

	if i.statusOK != "" {
		// Hard-truncate the OK line to the visible width so a long
		// session path ("resumed session: /Users/.../sessions/...")
		// doesn't overflow past the right edge and look broken on a
		// narrow terminal.
		line := "✓ " + i.statusOK
		if cols > 4 && len(line) > cols {
			line = line[:cols-1] + "…"
		}
		chat = append(chat, i.cfg.Theme.FG256(i.cfg.Theme.Tool, line), "")
	}

	// Extension notes (notify / display) live just under the
	// transcript, above the dialog/editor band. Cleared by /clear.
	if len(i.extNotes) > 0 {
		chat = append(chat, i.extNotes...)
		chat = append(chat, "")
	}

	// Dialogs (login or model picker) render between chat and the editor.
	var dialog []string
	switch {
	case i.dialog.Active():
		dialog = i.dialog.Render(i.cfg.Theme, cols)
	case i.modelDialog.Active():
		dialog = i.modelDialog.Render(i.cfg.Theme, cols)
	case i.sessionDialog.Active():
		// Reserve rows for the editor (~3), status line (1-2),
		// dialog chrome (header + hint + rule + indicators, ~5),
		// and leave the remainder for session rows. Minimum of 3
		// rows so even a very small terminal shows something.
		_, rows := i.cfg.Terminal.Size()
		avail := rows - 12
		if avail < 3 {
			avail = 3
		}
		i.sessionDialog.MaxRows = avail
		dialog = i.sessionDialog.Render(i.cfg.Theme, cols)
	case i.jumpDialog.Active():
		dialog = i.jumpDialog.Render(i.cfg.Theme, cols)
	case i.btwDialog.Active():
		dialog = i.btwDialog.Render(i.cfg.Theme, cols)
	case i.skillsDialog.Active():
		dialog = i.skillsDialog.Render(i.cfg.Theme, cols)
	case i.changelogDialog.Active():
		dialog = i.changelogDialog.Render(i.cfg.Theme, cols)
	case i.confirmDialog.Active():
		dialog = i.confirmDialog.Render(i.cfg.Theme, cols)
	case i.logoutDialog.Active():
		dialog = i.logoutDialog.Render(i.cfg.Theme, cols)
	case i.telegramDialog.Active():
		dialog = i.telegramDialog.Render(i.cfg.Theme, cols)
	case i.sessionOpsDialog.Active():
		dialog = i.sessionOpsDialog.Render(i.cfg.Theme, cols)
	case i.sessionTreeDialog.Active():
		dialog = i.sessionTreeDialog.Render(i.cfg.Theme, cols)
	case i.extPanel.Active():
		dialog = i.extPanel.Render(i.cfg.Theme, cols)
	}

	// Slash-command autocomplete: popup above the status line, only
	// when the editor starts with "/" and no dialog is already open.
	// Feed extension-registered commands into the suggester first so
	// they show up in tab-complete + the popup alongside the built-ins.
	if i.cfg.Extensions != nil {
		catalog := i.cfg.Extensions.Commands()
		extra := make([]slashCommand, 0, len(catalog))
		for _, c := range catalog {
			// The popup renders extension commands under a dedicated
			// "── extensions ───" divider, so the description doesn't
			// need to repeat the source. If the description is empty,
			// fall back to the extension name so the row isn't blank.
			desc := c.Description
			if strings.TrimSpace(desc) == "" {
				desc = "(" + c.Extension + ")"
			}
			extra = append(extra, slashCommand{
				Name: "/" + c.Name,
				Desc: desc,
			})
		}
		i.suggest.SetExtra(extra)
	}
	var suggest []string
	currentInput := i.ed.Value()
	// Slash popup renders even while the agent is busy so the user
	// can queue a destructive command (/clear, /compact, /logout,
	// /model) or a read-only one (/help, /jump, /sessions, etc.)
	// without waiting for the current turn to finish. The dispatcher
	// in runSlash already handles the busy case per-command: safe
	// ones run immediately, destructive ones cancel the turn first.
	i.fileSuggest.SetCWD(i.cfg.CWD)
	if len(dialog) == 0 && i.suggest.Active(currentInput) {
		suggest = i.suggest.Render(currentInput, i.cfg.Theme, cols)
	} else if len(dialog) == 0 && i.fileSuggest.Active(currentInput) {
		suggest = i.fileSuggest.Render(currentInput, i.cfg.Theme, cols)
	}

	// Busy prefix shown at the far left of the status bar. The
	// spinner glyph and its funny-line message share the `zot`
	// label colour (Theme.Assistant) so the whole "who's working"
	// band reads at a glance. Elapsed time stays muted because it
	// drifts every second and shouldn't grab focus.
	var busyPrefix string
	if i.busy {
		busyPrefix = fmt.Sprintf("%s %s %s %s",
			i.cfg.Theme.FG256(i.cfg.Theme.Assistant, i.spin.Frame()),
			i.cfg.Theme.FG256(i.cfg.Theme.Assistant, i.spin.Message()),
			i.cfg.Theme.FG256(i.cfg.Theme.Muted, "-"),
			i.cfg.Theme.FG256(i.cfg.Theme.Muted, i.spin.Elapsed().String()),
		)
	}

	ctxMax := 0
	if m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil {
		ctxMax = m.ContextWindow
	}
	statusLines := tui.StatusBar(tui.StatusBarParams{
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
		Telegram:       i.telegramBridge != nil && i.telegramBridge.Active(),
		Cols:           cols,
	})
	edLines, curR, curC := i.ed.Render(cols)

	// "Sliding in" chips for messages the user typed while a turn is
	// in flight. Shown directly above the status bar so they're close
	// to the editor but don't push the chat around.
	var queue []string
	if len(i.queued) > 0 {
		for _, q := range i.queued {
			label := i.cfg.Theme.FG256(i.cfg.Theme.Accent, "sliding in: ")
			text := truncateLine(q, cols-15)
			queue = append(queue, label+i.cfg.Theme.FG256(i.cfg.Theme.Muted, text))
		}
	}

	// Bottom-sticky sections (always visible, never scroll).
	bottom := make([]string, 0, len(dialog)+len(suggest)+len(queue)+len(edLines)+1)
	bottom = append(bottom, dialog...)
	bottom = append(bottom, suggest...)
	bottom = append(bottom, queue...)
	bottom = append(bottom, statusLines...)
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
		start = alignSliceStartToImageBlock(chat, start, end)
		visibleChat = chat[start:end]
	}
	visibleChat = clipBottomClippedImages(visibleChat)

	// A tiny "scrolled up" indicator in the top-right of the chat pane
	// so you know you're not at the bottom. When the viewport was
	// parked by /jump we include the turn number so the user remembers
	// they're reading history rather than the live conversation.
	if i.scrollOffset > 0 && len(visibleChat) > 0 {
		var text string
		if i.parkedTurn > 0 && i.parkedTotal > 0 {
			text = fmt.Sprintf("  ↑ viewing turn %d of %d - %d lines more below (pgdn / end)",
				i.parkedTurn, i.parkedTotal, i.scrollOffset)
		} else {
			text = fmt.Sprintf("  ↑ %d lines more below (end to jump)", i.scrollOffset)
		}
		note := i.cfg.Theme.FG256(i.cfg.Theme.Muted, text)
		visibleChat = append([]string{note}, visibleChat...)
		if len(visibleChat) > chatRows {
			visibleChat = visibleChat[:chatRows]
		}
	}

	frame := make([]string, 0, len(visibleChat)+len(bottom))
	frame = append(frame, visibleChat...)
	frame = append(frame, bottom...)

	// Default: the real terminal cursor sits on the main editor's
	// input position. When an overlay dialog has its own input
	// field (today just /btw), route the cursor there instead so
	// the blinking cursor shows where the user is actually typing.
	// Dialogs without a cursor (model picker, /help, /login, etc.)
	// return -1 and the main editor keeps the cursor.
	cursorRow := len(visibleChat) + len(dialog) + len(suggest) + len(queue) + len(statusLines) + curR
	cursorCol := curC
	if i.btwDialog.Active() {
		if r, c := i.btwDialog.CursorPos(cols); r >= 0 {
			cursorRow = len(visibleChat) + r
			cursorCol = c
		}
	}
	if i.dialog.Active() {
		if r, c := i.dialog.CursorPos(cols); r >= 0 {
			cursorRow = len(visibleChat) + r
			cursorCol = c
		}
	}
	if i.extPanel.Active() {
		cursorRow = -1
		cursorCol = 0
	}
	i.rend.Draw(frame, cursorRow, cursorCol)
}

func alignSliceStartToImageBlock(chat []string, start, end int) int {
	if start <= 0 || start >= len(chat) || start >= end {
		return start
	}
	// If the slice already starts on an image row, keep it.
	if strings.Contains(chat[start], "\x1b]1337;File=") || strings.Contains(chat[start], "\x1b_G") {
		return start
	}
	// When the viewport begins inside an image block, the image escape row
	// sits just above a run of blank reservation rows, followed by an
	// "image - ..." info line. In that case, snap the slice start up to the
	// escape row so the image actually renders instead of showing only the
	// reserved blank area and metadata.
	j := start
	for j < end && strings.TrimSpace(chat[j]) == "" {
		j++
	}
	if j >= end || !strings.Contains(chat[j], "image - ") {
		return start
	}
	for k := start - 1; k >= 0; k-- {
		line := chat[k]
		if strings.Contains(line, "\x1b]1337;File=") || strings.Contains(line, "\x1b_G") {
			return k
		}
		if strings.TrimSpace(line) != "" {
			break
		}
	}
	return start
}

const hiddenOpenAIImageMirrorPrefix = "Tool output included the following image content:"

func filterHiddenTranscriptMessages(msgs []provider.Message) []provider.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if isHiddenTranscriptMessage(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func isHiddenTranscriptMessage(m provider.Message) bool {
	if m.Role != provider.RoleUser || len(m.Content) == 0 {
		return false
	}
	tb, ok := m.Content[0].(provider.TextBlock)
	if !ok {
		return false
	}
	return strings.TrimSpace(tb.Text) == hiddenOpenAIImageMirrorPrefix
}

func clipBottomClippedImages(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := append([]string(nil), lines...)
	for i, line := range out {
		if !strings.Contains(line, "\x1b]1337;File=") && !strings.Contains(line, "\x1b_G") {
			continue
		}
		// Image blocks render as: image escape, zero or more blank
		// reservation rows, then the muted "image - ..." info line,
		// then one trailing blank. If the info line isn't visible in
		// the current chat slice, the image would paint down into the
		// fixed status bar area. Suppress that image for this frame.
		foundInfo := false
		for j := i + 1; j < len(out); j++ {
			if strings.Contains(out[j], "image - ") {
				foundInfo = true
				break
			}
			if strings.TrimSpace(out[j]) != "" {
				break
			}
		}
		if !foundInfo {
			out[i] = ""
		}
	}
	return out
}

// truncateLine shortens s so it fits within n display cells, with an
// ellipsis if trimmed. Used by the "sliding in" chips so a pasted
// novel doesn't blow past the status line.
func panelKeyName(k tui.Key) string {
	switch k.Kind {
	case tui.KeyUp:
		return "up"
	case tui.KeyDown:
		return "down"
	case tui.KeyLeft:
		return "left"
	case tui.KeyRight:
		return "right"
	case tui.KeyEnter:
		return "enter"
	case tui.KeyEsc:
		return "esc"
	case tui.KeyTab:
		return "tab"
	case tui.KeyBackspace:
		return "backspace"
	case tui.KeyDelete:
		return "delete"
	case tui.KeyHome:
		return "home"
	case tui.KeyEnd:
		return "end"
	case tui.KeyPageUp:
		return "pageup"
	case tui.KeyPageDown:
		return "pagedown"
	case tui.KeyRune:
		return "rune"
	default:
		return "unknown"
	}
}

func panelKeyText(k tui.Key) string {
	if k.Kind == tui.KeyRune {
		return string(k.Rune)
	}
	return ""
}

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

// ctrlCExitWindow is how long after a ctrl+c press a *second* press
// will exit instead of just clearing input. Long enough to be
// deliberate (rules out accidental key chord), short enough that the
// hint stays meaningful.
const ctrlCExitWindow = 2 * time.Second

// armCtrlCExit records the timestamp of the current ctrl+c so the next
// one within ctrlCExitWindow exits.
func (i *Interactive) armCtrlCExit() {
	i.mu.Lock()
	i.lastCtrlC = time.Now()
	i.mu.Unlock()
}

// ctrlCExitArmed reports whether a previous ctrl+c was recent enough
// that another press should now exit.
func (i *Interactive) ctrlCExitArmed() bool {
	i.mu.Lock()
	t := i.lastCtrlC
	i.mu.Unlock()
	return !t.IsZero() && time.Since(t) <= ctrlCExitWindow
}

func (i *Interactive) handleKey(ctx context.Context, k tui.Key) (done bool) {
	// Any key that isn't ctrl+c invalidates an armed ctrl+c-exit, so
	// pressing ctrl+c then typing then ctrl+c much later doesn't quit
	// unexpectedly. The hint message also goes stale; clear it.
	if k.Kind != tui.KeyCtrlC {
		i.mu.Lock()
		if !i.lastCtrlC.IsZero() {
			i.lastCtrlC = time.Time{}
			if strings.HasPrefix(i.statusOK, "input cleared") || strings.HasPrefix(i.statusOK, "press ctrl+c") {
				i.statusOK = ""
			}
		}
		i.mu.Unlock()
	}

	// Dialogs consume keys while open (except ctrl+c, which always closes them).

	// Confirm dialog has highest priority: the agent goroutine is
	// blocked waiting for an answer, so we must not let keys leak
	// anywhere else while it's up.
	if i.confirmDialog.Active() {
		i.confirmDialog.HandleKey(k)
		i.invalidate()
		return false
	}
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
		if act.StartManual {
			i.startManualOAuthFlow(act.Provider)
		}
		if act.SubmitCode != "" {
			i.submitManualOAuthCode(act.SubmitCode)
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
	if i.logoutDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.logoutDialog.Close()
			i.invalidate()
			return false
		}
		act := i.logoutDialog.HandleKey(k)
		if act.Select {
			i.doLogout(act.Target)
		}
		i.invalidate()
		return false
	}
	if i.telegramDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.telegramDialog.Close()
			i.invalidate()
			return false
		}
		act := i.telegramDialog.HandleKey(k)
		if act.Select {
			i.doTelegram(act.Action)
		}
		i.invalidate()
		return false
	}
	if i.sessionOpsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.sessionOpsDialog.Close()
			i.invalidate()
			return false
		}
		act := i.sessionOpsDialog.HandleKey(k)
		if act.Select {
			i.doSessionOp(act.Action, "")
		}
		i.invalidate()
		return false
	}
	if i.sessionTreeDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.sessionTreeDialog.Close()
			i.invalidate()
			return false
		}
		act := i.sessionTreeDialog.HandleKey(k)
		if act.Select {
			i.applySessionTreeSelection(act.Path)
		}
		i.invalidate()
		return false
	}
	if i.extPanel.Active() {
		if k.Kind == tui.KeyCtrlC || k.Kind == tui.KeyEsc {
			if i.cfg.Extensions != nil {
				_ = i.cfg.Extensions.SendPanelClose(i.extPanel.ext, i.extPanel.id)
			}
			i.extPanel.Close()
			i.invalidate()
			return false
		}
		if i.cfg.Extensions != nil {
			_ = i.cfg.Extensions.SendPanelKey(i.extPanel.ext, i.extPanel.id, panelKeyName(k), panelKeyText(k))
		}
		return false
	}
	if i.jumpDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.jumpDialog.Close()
			i.pendingFork = false
			return false
		}
		act := i.jumpDialog.HandleKey(k)
		if act.Select {
			if i.pendingFork {
				i.applyForkSelection(act.MessageIdx)
			} else {
				i.applyJumpSelection(act.MessageIdx, act.TurnNo)
			}
		}
		// If the user dismissed the dialog without selecting, also
		// clear the pending-fork flag so a later plain /jump isn't
		// hijacked.
		if act.Close {
			i.pendingFork = false
		}
		return false
	}
	if i.btwDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.btwDialog.Close()
			i.invalidate()
			return false
		}
		i.btwDialog.HandleKey(k, i.invalidate)
		return false
	}
	if i.skillsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.skillsDialog.Close()
			i.invalidate()
			return false
		}
		i.skillsDialog.HandleKey(k)
		i.invalidate()
		return false
	}
	if i.changelogDialog.Active() {
		if closed := i.changelogDialog.HandleKey(k); closed {
			// User dismissed; let the parent persist the
			// LastChangelogShown marker via the close callback.
			if i.cfg.OnChangelogDismiss != nil {
				i.cfg.OnChangelogDismiss()
			}
		}
		i.invalidate()
		return false
	}

	// Global keys.
	switch k.Kind {
	case tui.KeyCtrlC:
		// While busy: do NOT cancel the turn. ctrl+c during a
		// running turn is almost always reflex muscle memory
		// ("be quiet" in a shell) rather than a deliberate
		// decision to kill a multi-minute model call that's
		// already cost tokens. Use esc to interrupt a turn; use
		// a deliberate double-ctrl+c to exit zot entirely. First
		// press arms the exit hint, second press within
		// ctrlCExitWindow quits.
		if i.busy {
			if i.ctrlCExitArmed() {
				return true
			}
			i.mu.Lock()
			i.statusOK = "press ctrl+c again to exit, esc to cancel the turn"
			i.statusErr = ""
			i.mu.Unlock()
			i.armCtrlCExit()
			return false
		}
		// Idle: first press clears the editor (and any queued
		// follow-up messages); a second press within ctrlCExitWindow
		// exits. With both an empty editor and no queue the first
		// press still just arms — require a deliberate double-tap.
		hadInput := !i.ed.IsEmpty() || len(i.queued) > 0
		if hadInput {
			i.ed.Clear()
			i.suggest.Reset()
			i.mu.Lock()
			i.queued = nil
			i.statusOK = "input cleared"
			i.statusErr = ""
			i.mu.Unlock()
			i.armCtrlCExit()
			return false
		}
		if i.ctrlCExitArmed() {
			return true
		}
		i.mu.Lock()
		i.statusOK = "press ctrl+c again to exit"
		i.statusErr = ""
		i.mu.Unlock()
		i.armCtrlCExit()
		return false
	case tui.KeyEsc:
		// Esc interrupts a running turn — but only when nothing
		// else on screen wants to consume the key first. The slash
		// popup has its own Esc behaviour (close + clear editor),
		// and transient overlays like the /help block and extension
		// notes should dismiss on Esc before we even consider the
		// turn. Without these guards, a casual Esc press after
		// running /help on a busy turn rips the turn away.
		if i.suggest.Active(i.ed.Value()) || i.fileSuggest.Active(i.ed.Value()) {
			break
		}
		i.mu.Lock()
		hadHelp := len(i.helpBlock) > 0
		hadNotes := len(i.extNotes) > 0
		if hadHelp {
			i.helpBlock = nil
		}
		if hadNotes {
			i.extNotes = nil
		}
		i.mu.Unlock()
		if hadHelp || hadNotes {
			i.invalidate()
			return false
		}
		if i.busy && i.cancelTurn != nil {
			i.cancelTurn()
			// If a confirm dialog is pending, refuse it so the agent
			// goroutine unblocks and the context cancellation can
			// actually take effect.
			i.confirmDialog.CancelAll("turn cancelled")
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
		// Always use up/down for chat scrolling, even when the editor
		// contains text. This makes keyboard scrolling consistent with
		// a draft present at the cost of disabling vertical cursor
		// motion inside the multi-line editor. Keep slash-popup
		// navigation working by letting it intercept later when active.
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
			i.scrollBy(+3)
			return false
		}
	case tui.KeyDown:
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
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

	// File suggestions: intercept up/down/tab/enter when the @-popup is visible.
	if i.fileSuggest.Active(i.ed.Value()) {
		switch k.Kind {
		case tui.KeyUp:
			i.fileSuggest.Up()
			return false
		case tui.KeyDown:
			i.fileSuggest.Down()
			return false
		case tui.KeyRight:
			// Open selected directory.
			i.fileSuggest.Right()
			return false
		case tui.KeyLeft:
			// Go back to parent directory.
			i.fileSuggest.Left()
			return false
		case tui.KeyEnter:
			if entry, ok := i.fileSuggest.SelectedEntry(i.ed.Value()); ok {
				var chip string
				if entry.isDir {
					chip = "[dir:" + entry.rel + "/]"
				} else {
					chip = "[file:" + entry.rel + "]"
				}
				val := i.ed.Value()
				if idx := strings.LastIndex(val, "@"); idx >= 0 {
					val = val[:idx]
				}
				i.ed.SetValue(val + chip + " ")
				i.fileSuggest.Reset()
			}
			return false
		case tui.KeyEsc:
			val := i.ed.Value()
			if idx := strings.LastIndex(val, "@"); idx >= 0 {
				i.ed.SetValue(val[:idx])
			}
			i.fileSuggest.Reset()
			return false
		}
	}

	if submit := i.ed.HandleKey(k); submit {
		// SubmitValue() expands any [pasted text #N +L lines]
		// placeholders back into their bodies; the raw Value()
		// is only what the user sees on screen.
		text := strings.TrimRight(i.ed.SubmitValue(), "\n")
		// Expand [file:name] and [dir:name/] chips to full paths.
		text = expandFileChips(text, i.cfg.CWD)
		if text == "" {
			return false
		}
		i.ed.Clear()
		i.suggest.Reset()
		i.fileSuggest.Reset()

		if looksLikeSlashCommand(text) {
			head := text
			rest := ""
			if idx := strings.IndexAny(text, " \t"); idx >= 0 {
				head = text[:idx]
				rest = strings.TrimSpace(text[idx:])
			}
			if !isKnownSlashCommand(text) {
				// Try extensions before giving up. Extensions register
				// commands by bare name (no leading slash); strip it here.
				extName := strings.TrimPrefix(head, "/")
				if i.cfg.Extensions != nil && i.cfg.Extensions.HasCommand(extName) {
					go i.invokeExtensionCommand(ctx, extName, rest)
					return false
				}
				i.mu.Lock()
				i.statusErr = "unknown command " + head + " — type /help to see the list"
				i.statusOK = ""
				i.mu.Unlock()
				return false
			}
			// Slash commands run regardless of busy state. Commands that
			// would mutate the transcript or replace the agent (/clear,
			// /compact, /logout, /login, /model) cancel the active turn
			// first and wait for the goroutine to wind down so they don't
			// race with a streaming response. Safe commands (/help,
			// /jump, /sessions, /jail, /unjail, /exit) run immediately
			// without disturbing the active turn.
			if slashCancelsTurn(head) {
				i.cancelAndWaitForIdle()
			}
			return i.runSlash(ctx, text)
		}

		if i.agent == nil {
			i.mu.Lock()
			i.statusErr = "not logged in. type /login first."
			i.mu.Unlock()
			return false
		}
		// Mirror the user's typed prompt into the paired Telegram
		// chat (when the bridge is active) so the Telegram thread
		// stays a complete record of the session, not just the half
		// that originated on the phone. On a goroutine so the
		// network write doesn't delay the local turn.
		if i.telegramBridge != nil && i.telegramBridge.Active() {
			go i.telegramBridge.OnUserTyped(text)
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

// invokeExtensionCommand fires an extension-registered slash command
// in a background goroutine, awaits the response, and applies the
// requested action (prompt / insert / display / noop). Errors and
// timeouts surface as a status_err line.
func (i *Interactive) invokeExtensionCommand(ctx context.Context, name, args string) {
	resp, err := i.cfg.Extensions.Invoke(ctx, name, args, 30*time.Second)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": " + err.Error()
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if resp.Error != "" {
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": " + resp.Error
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	switch resp.Action {
	case "open_panel":
		if resp.OpenPanel != nil {
			extName := name
			if i.cfg.Extensions != nil {
				if owner := i.cfg.Extensions.CommandOwner(name); owner != "" {
					extName = owner
				}
			}
			i.OpenPanel(extName, *resp.OpenPanel)
		}
	case "prompt":
		if strings.TrimSpace(resp.Prompt) == "" {
			return
		}
		i.startTurn(i.runCtx, resp.Prompt)
	case "insert":
		i.ed.Insert(resp.Insert)
		i.invalidate()
	case "display":
		i.appendExtensionNote(name, resp.Display, "info")
	case "noop", "":
		// nothing
	default:
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": unknown action " + resp.Action
		i.mu.Unlock()
		i.invalidate()
	}
}

// appendExtensionNote renders an extension-originated note in the
// chat. Levels: "info" (muted), "warn" (warning), "error" (error),
// "success" (tool/ok green).
func (i *Interactive) appendExtensionNote(extName, msg, level string) {
	if msg == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	color := i.cfg.Theme.Muted
	switch level {
	case "warn":
		color = i.cfg.Theme.Warning
	case "error":
		color = i.cfg.Theme.Error
	case "success":
		color = i.cfg.Theme.Tool
	}
	prefix := i.cfg.Theme.FG256(i.cfg.Theme.Accent, "["+extName+"] ")
	for _, line := range strings.Split(msg, "\n") {
		i.statusOK = "" // clear any stale ok
		i.statusErr = ""
		i.extNotes = append(i.extNotes, prefix+i.cfg.Theme.FG256(color, line))
	}
}

// HostHooks implementation for the extension manager. The manager
// holds an interface, not a concrete *Interactive, so these methods
// are the only thing the manager sees.

// Notify is the manager's NotifyFromExt entry point.
func (i *Interactive) Notify(extName, level, message string) {
	i.appendExtensionNote(extName, message, level)
	i.invalidate()
}

// Submit feeds text through the agent loop as if the user had typed it.
func (i *Interactive) Submit(text string) {
	i.startTurn(i.runCtx, text)
}

// SubmitOrQueue runs text immediately if the agent is idle, or
// appends it to the pending queue if a turn is already in flight.
// Used by the telegram bridge (and by the editor submit path) so
// both input sources share the same "queue behind an active turn"
// semantics. Images are ignored for now — only the text prompt is
// forwarded — because the queued-prompt path is text-only; a
// follow-up can expand the queue entry to carry images.
func (i *Interactive) SubmitOrQueue(text string, images []provider.ImageBlock) {
	_ = images // reserved for future queued-with-images support
	i.mu.Lock()
	if i.agent == nil {
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.busy {
		i.queued = append(i.queued, text)
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Unlock()
	i.startTurn(i.runCtx, text)
}

// CancelTurn aborts the active turn if one is running. Used by the
// telegram bridge when the paired user sends /stop.
// ChangelogVersion returns the version string of the changelog
// currently shown (or last shown). Used by the dismiss callback
// to store the correct version for dev builds.
func (i *Interactive) ChangelogVersion() string {
	if i.changelogDialog != nil {
		return i.changelogDialog.version
	}
	return ""
}

func (i *Interactive) CancelTurn() {
	i.mu.Lock()
	cancel := i.cancelTurn
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Insert places text at the cursor in the editor.
func (i *Interactive) Insert(text string) {
	i.ed.Insert(text)
	i.invalidate()
}

// Display appends a styled note from extName to the chat without a
// model call.
func (i *Interactive) Display(extName, text string) {
	i.appendExtensionNote(extName, text, "info")
	i.invalidate()
}

func (i *Interactive) OpenPanel(extName string, spec extproto.PanelSpec) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.extPanel.Open(extName, spec.ID, spec.Title, spec.Lines, spec.Footer)
	if i.cfg.Extensions != nil {
		cols, rows := i.cfg.Terminal.Size()
		_ = cols
		_ = rows
	}
	i.invalidate()
}

func (i *Interactive) UpdatePanel(extName, panelID, title string, lines []string, footer string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.extPanel.Active() && i.extPanel.ext == extName && i.extPanel.id == panelID {
		i.extPanel.Update(title, lines, footer)
		i.invalidate()
	}
}

func (i *Interactive) ClosePanel(extName, panelID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.extPanel.Active() && i.extPanel.ext == extName && i.extPanel.id == panelID {
		i.extPanel.Close()
		i.invalidate()
	}
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
		i.parkedTurn = 0
		i.parkedTotal = 0
		i.scrollOffset = 0
		i.extNotes = nil
		i.view.InvalidateRenderCache()
		i.mu.Unlock()
	case "/help":
		i.mu.Lock()
		i.helpBlock = renderHelpBlock(i.cfg.Theme, i.lastCols())
		i.statusErr = ""
		i.statusOK = ""
		// Pin the viewport to the newest content so the help block,
		// which we just appended to the end of the transcript, is
		// what the user actually sees.
		i.scrollOffset = 0
		i.mu.Unlock()
	case "/login":
		i.dialog.Open(i.cfg.ZotHome)
	case "/logout":
		if len(parts) >= 2 {
			// Explicit target: /logout anthropic | openai | all
			i.doLogout(parts[1])
			break
		}
		// No arg: open the picker over whichever providers are
		// currently logged in. If nothing's logged in, bail with a
		// status line.
		i.openLogoutDialog()
	case "/model":
		if len(parts) >= 2 {
			i.applyModelSelection("", parts[1])
		} else {
			i.modelDialog.Open(i.cfg.Model)
		}
	case "/sessions":
		i.sessionDialog.Open(i.cfg.ZotHome, i.cfg.CWD)
	case "/jump":
		i.openJumpDialog(parts[1:])
	case "/btw":
		i.openBtwDialog(parts[1:])
	case "/skills":
		i.openSkillsDialog()
	case "/compact":
		i.runCompact(ctx, false)
	case "/study":
		// Canned prompt that tells the agent to read every file
		// in the current directory so its later turns have the
		// whole project in context. Dispatched through the normal
		// queue-or-start path so it behaves identically to
		// typing the prompt by hand.
		const studyPrompt = "Read and understand everything in the current directory."
		i.mu.Lock()
		if i.busy {
			i.queued = append(i.queued, studyPrompt)
			i.mu.Unlock()
			i.invalidate()
			break
		}
		i.mu.Unlock()
		i.startTurn(ctx, studyPrompt)
	case "/jail":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Lock()
		i.mu.Lock()
		i.statusOK = "jailed to " + i.cfg.CWD + " (tools cannot touch paths outside this directory)"
		i.statusErr = ""
		i.mu.Unlock()
	case "/unjail":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Unlock()
		i.mu.Lock()
		i.statusOK = "unjailed"
		i.statusErr = ""
		i.mu.Unlock()
	case "/reload-ext":
		i.runReloadExt(ctx)
	case "/yolo":
		i.runYoloOn()
	case "/telegram", "/tg":
		if len(parts) >= 2 {
			i.doTelegram(parts[1])
			break
		}
		i.openTelegramDialog()
	case "/session":
		if len(parts) >= 2 {
			action := parts[1]
			arg := ""
			if len(parts) >= 3 {
				arg = strings.Join(parts[2:], " ")
			}
			i.doSessionOp(action, arg)
			break
		}
		i.openSessionOpsDialog()
	default:
		// Last-resort fallback: try the extension manager. Built-in
		// cases above always win; this branch only fires for slash
		// commands the extension manager registered. Same routing as
		// the editor's submit-handler dispatch path so the autocomplete
		// "enter on highlighted suggestion" flow also works.
		extName := strings.TrimPrefix(parts[0], "/")
		if i.cfg.Extensions != nil && i.cfg.Extensions.HasCommand(extName) {
			rest := ""
			if len(parts) > 1 {
				rest = strings.Join(parts[1:], " ")
			}
			go i.invokeExtensionCommand(ctx, extName, rest)
			return false
		}
		i.mu.Lock()
		i.statusErr = "unknown command: " + parts[0]
		i.mu.Unlock()
	}
	return false
}

// openLogoutDialog shows the provider picker for `/logout` with no
// argument. Only providers the user is currently logged into are
// listed, plus an "all" entry when more than one is present. If
// nothing's logged in, writes a status line instead of opening an
// empty dialog.
func (i *Interactive) openLogoutDialog() {
	if i.cfg.AuthManager == nil {
		i.mu.Lock()
		i.statusErr = "no auth manager configured"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	store := i.cfg.AuthManager.Store()
	if store == nil {
		i.mu.Lock()
		i.statusErr = "auth store is not available"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	creds, err := store.Load()
	if err != nil {
		i.mu.Lock()
		i.statusErr = "read auth store: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}

	var items []logoutItem
	for _, p := range []string{"anthropic", "openai"} {
		if creds.Has(p) {
			items = append(items, logoutItem{
				label:  p,
				target: p,
				method: creds.Method(p),
			})
		}
	}
	if len(items) == 0 {
		i.mu.Lock()
		i.statusOK = "no credentials stored; already logged out"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if len(items) > 1 {
		items = append(items, logoutItem{label: "all", target: "all"})
	}

	i.logoutDialog.Open(items)
	i.invalidate()
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
	// Always run the manual/copy-code flow in parallel with the local
	// callback server so headless environments (docker, SSH) can paste
	// the authorization code directly without first pressing 'p'.
	_, err := i.cfg.AuthManager.StartOAuth(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	manualURL, mErr := i.cfg.AuthManager.StartManualOAuth(provider)
	if mErr == nil {
		i.dialog.ShowWaiting(manualURL)
	} else {
		i.dialog.ShowResult(false, mErr.Error())
	}
}

func (i *Interactive) startManualOAuthFlow(provider string) {
	if i.cfg.AuthManager == nil {
		return
	}
	i.cfg.AuthManager.CancelOAuth()
	url, err := i.cfg.AuthManager.StartManualOAuth(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.url = url
	i.invalidate()
}

func (i *Interactive) submitManualOAuthCode(code string) {
	if i.cfg.AuthManager == nil {
		return
	}
	go func() {
		if err := i.cfg.AuthManager.CompleteManualOAuth(i.runCtx, code); err != nil {
			i.dialog.ShowResult(false, err.Error())
			i.invalidate()
		}
	}()
}

// applyModelSelection switches the active model (and provider, if the
// new model belongs to a different one). It rebuilds the underlying
// client when needed so the provider wire-protocol matches.
// cancelAndWaitForIdle cancels the active turn (if any) and blocks
// briefly until the turn goroutine has updated i.busy = false. Used
// before destructive slash commands so transcript-mutating work
// (/clear, /compact, /logout, /login completion, cross-provider
// /model swap) doesn't race with the still-running stream.
//
// The wait is bounded; if the turn doesn't release within the timeout
// we proceed anyway. Worst case is a brief overlap that the agent's
// own mutex protects against.
func (i *Interactive) cancelAndWaitForIdle() {
	i.mu.Lock()
	busy := i.busy
	cancel := i.cancelTurn
	i.mu.Unlock()
	if !busy {
		return
	}
	if cancel != nil {
		cancel()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		done := !i.busy
		i.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openBtwDialog opens the side-chat overlay with a frozen snapshot
// of the current main session. The optional argument is auto-
// submitted as the first question, so '/btw does X work?' fires the
// model call immediately instead of just opening an empty dialog.
func (i *Interactive) openBtwDialog(args []string) {
	if i.agent == nil {
		i.mu.Lock()
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		return
	}
	seed := strings.TrimSpace(strings.Join(args, " "))
	i.btwDialog.Open(i.cfg.Theme, i.agent, i.agent.System, i.cfg.Model, seed, i.invalidate)
	i.invalidate()
}

// openSkillsDialog opens the skill inspector. The picker reflects
// whatever SkillSnapshot returns at call time, so edits to a
// SKILL.md made during a session show up on the next /skills.
func (i *Interactive) openSkillsDialog() {
	var list []*skills.Skill
	if i.cfg.SkillSnapshot != nil {
		list = i.cfg.SkillSnapshot()
	}
	i.skillsDialog.Open(list)
	i.invalidate()
}

// openJumpDialog builds a /jump picker from the current transcript.
// If the user typed "/jump foo" with a filter and it matches exactly
// one turn, jump there directly without showing the dialog.
func (i *Interactive) openJumpDialog(args []string) {
	if i.view == nil || len(i.view.Messages) == 0 {
		i.mu.Lock()
		i.statusErr = "nothing to jump to \u2014 the session is empty"
		i.mu.Unlock()
		return
	}
	filter := strings.TrimSpace(strings.Join(args, " "))
	i.jumpDialog.Open(i.view.Messages, filter)
	// Shortcut: with a filter argument that matches exactly one turn,
	// jump immediately and skip the picker.
	if filter != "" {
		if tgts := i.jumpDialog.Targets(); len(tgts) == 1 {
			t := tgts[0]
			i.jumpDialog.Close()
			i.applyJumpSelection(t.MessageIdx, t.TurnNo)
		}
	}
}

// applyJumpSelection scrolls the chat viewport so the user message at
// msgIdx is visible at (or near) the top of the chat area. Uses the
// anchor slice returned by view.BuildWithAnchors so the mapping from
// message index to row is exact, regardless of variable-height tool
// blocks above the target.
func (i *Interactive) applyJumpSelection(msgIdx, turnNo int) {
	cols := i.lastCols()
	chat, anchors := i.view.BuildWithAnchors(cols)
	var row int
	found := false
	for _, a := range anchors {
		if a.MessageIdx == msgIdx {
			row = a.Row
			found = true
			break
		}
	}
	if !found {
		i.mu.Lock()
		i.statusErr = "could not resolve jump target"
		i.mu.Unlock()
		return
	}

	chatLen := len(chat)
	page := i.chatPage()
	if page < 1 {
		page = 1
	}
	// scrollOffset is measured from the bottom of the chat slice, so
	// to place `row` at the top of the viewport we want:
	//     chatLen - scrollOffset - page == row
	// Solve for scrollOffset and clamp to [0, chatLen-page].
	offset := chatLen - (row + page)
	if offset < 0 {
		offset = 0
	}
	maxOffset := chatLen - page
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}

	i.mu.Lock()
	i.scrollOffset = offset
	i.parkedTurn = turnNo
	i.parkedTotal = totalTurnsLocked(i.view.Messages)
	i.statusOK = fmt.Sprintf("jumped to turn %d", turnNo)
	i.statusErr = ""
	i.mu.Unlock()
}

// totalTurnsLocked counts user messages in the transcript. Caller is
// assumed to hold i.mu (the name is a mild reminder; this function
// itself doesn't touch shared state beyond the slice it's handed).
func totalTurnsLocked(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == provider.RoleUser {
			n++
		}
	}
	return n
}

// applySessionSelection loads the given session via the cli-provided
// callback and parks the viewport on the last turn so the user lands
// looking at where the conversation left off (their last prompt at the
// top of the chat, the assistant's last reply right below). Older
// history is one scroll up; pgdn or end snaps to the current tail.
//
// Without this, scrollOffset stayed at 0 (pinned to the live tail),
// which on a long resumed session showed only the last few rows of
// the final assistant message — the user read that as "only one liner
// happened, the resume didn't work".
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
	i.parkedTurn = 0
	i.parkedTotal = 0
	i.view.InvalidateRenderCache()
	// Pull the freshly-loaded transcript into the view so the anchor
	// math below sees the post-resume messages, not the empty pre-load
	// state. redraw() does the same on its next pass; we just front-run
	// it here to compute the scroll target.
	if i.agent != nil {
		i.view.Messages = i.agent.Messages()
	}
	msgs := i.view.Messages
	i.mu.Unlock()

	i.scrollToLastTurn(msgs)
}

// scrollToLastTurn parks the viewport at the most recent user turn,
// or at the top if the transcript has no user messages. Used after
// resume so the user lands looking at where they left off.
func (i *Interactive) scrollToLastTurn(msgs []provider.Message) {
	if len(msgs) == 0 {
		i.mu.Lock()
		i.scrollOffset = 0
		i.mu.Unlock()
		return
	}
	// Find the last user message index.
	lastUser := -1
	turnNo, totalTurns := 0, 0
	for idx, m := range msgs {
		if m.Role == provider.RoleUser {
			totalTurns++
			lastUser = idx
		}
	}
	if lastUser < 0 {
		i.mu.Lock()
		i.scrollOffset = 0
		i.mu.Unlock()
		return
	}
	turnNo = totalTurns

	cols := i.lastCols()
	chat, anchors := i.view.BuildWithAnchors(cols)
	var row int
	found := false
	for _, a := range anchors {
		if a.MessageIdx == lastUser {
			row = a.Row
			found = true
			break
		}
	}
	if !found {
		i.mu.Lock()
		i.scrollOffset = 0
		i.mu.Unlock()
		return
	}

	chatLen := len(chat)
	page := i.chatPage()
	if page < 1 {
		page = 1
	}
	offset := chatLen - (row + page)
	if offset < 0 {
		offset = 0
	}
	maxOffset := chatLen - page
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}

	i.mu.Lock()
	i.scrollOffset = offset
	// Mark the parked-turn footer so the user sees "viewing turn N of
	// M - pgdn to catch up" — same affordance as /jump. Tells them at
	// a glance that they're looking at history, not the live tail.
	if offset > 0 {
		i.parkedTurn = turnNo
		i.parkedTotal = totalTurns
	}
	i.mu.Unlock()
	i.invalidate()
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
	// Snapshot the current transcript and cumulative usage BEFORE we
	// build the replacement agent so we can hand them off. Without
	// this the user perceives the entire session as wiped on a
	// cross-provider /model swap.
	var carryMsgs []provider.Message
	var carryCost provider.Usage
	if i.agent != nil {
		carryMsgs = i.agent.Messages()
		carryCost = i.agent.Cost()
	}

	ag, p, md, err := i.cfg.BuildAgentFor(m.Provider, m.ID)
	if err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return
	}

	// Replay the transcript and seed the cost on the freshly-built
	// agent. Messages travel cleanly between providers because they
	// use the same provider.Message shape; tool-call ids are local
	// to a turn so cross-provider continuation never confuses the
	// new model (it just sees the assistant's reply, no orphan
	// tool_use blocks because /model swaps are gated to idle state).
	if len(carryMsgs) > 0 {
		ag.SetMessages(carryMsgs)
	}
	ag.SeedCost(carryCost)

	i.mu.Lock()
	i.agent = ag
	i.cfg.Provider = p
	i.cfg.Model = md
	i.statusOK = "switched to " + p + " / " + md
	i.statusErr = ""
	// Render cache keys are width+content based, so the new agent's
	// identical messages will reuse the existing entries. Nothing
	// to invalidate.
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
	} else {
		i.spin.StartFixed("compacting")
	}
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	// Do NOT set streamOn: the summary text should not be visible
	// in the chat while compacting. The user just sees the spinner
	// and can keep typing / queue prompts.
	i.scrollOffset = 0
	i.helpBlock = nil
	i.mu.Unlock()
	i.invalidate()

	go func() {
		// Sink discards deltas — we don't stream the summary to the UI.
		sink := func(delta string) {}
		summary, err := i.agent.Compact(ctx, 4, sink)
		_ = summary
		i.mu.Lock()
		i.busy = false
		i.resetStreamingStateLocked()
		i.cancelTurn = nil
		i.autoCompacting = false

		// Drain the queue: if the user typed a prompt while compacting,
		// fire it now that the transcript is clean.
		var next string
		var hasNext bool

		switch {
		case err != nil && ctx.Err() != nil:
			i.statusErr = ""
			if auto {
				i.statusOK = "auto-condense cancelled"
			} else {
				i.statusOK = "compaction cancelled"
			}
			i.queued = nil // drop queue on cancel
		case err != nil:
			i.statusErr = "compaction failed: " + err.Error()
			i.statusOK = ""
			i.queued = nil // drop queue on error
		default:
			i.statusErr = ""
			// Read token count from the compaction message meta.
			tokens := ""
			msgs := i.agent.Messages()
			if len(msgs) > 0 && msgs[0].Meta["compaction"] == "true" {
				tokens = msgs[0].Meta["tokens_before"]
			}
			if tokens != "" {
				i.statusOK = fmt.Sprintf("compacted from ~%s tokens (ctrl+o to expand)", tokens)
			} else {
				i.statusOK = "compacted (ctrl+o to expand)"
			}
			i.lastCtxInput = 0
			i.toolCalls = map[string]*tui.ToolCallView{}
			i.toolOrder = nil
			i.view.InvalidateRenderCache()
			// Pop queued prompt if any.
			if len(i.queued) > 0 {
				next, i.queued = i.queued[0], i.queued[1:]
				hasNext = true
			}
		}
		i.mu.Unlock()
		i.invalidate()

		if hasNext {
			p := i.runCtx
			if p == nil {
				p = context.Background()
			}
			i.startTurn(p, next)
		}
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
	i.parkedTurn = 0   // starting a turn clears the /jump parked state
	i.parkedTotal = 0
	i.helpBlock = nil // hide the help block once the user asks something
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
		// Don't touch streamPending / streamFlushPending here — the
		// pacer may still be draining the final deltas and needs to
		// paint them even though Prompt has returned. It will reset
		// streamOn on its own once the buffer empties.
		if len(i.streamPending) == 0 {
			i.streamOn = false
		}
		i.cancelTurn = nil
		if err != nil && ctx.Err() == nil {
			i.statusErr = err.Error()
		}
		// Persist the assistant's reply (and every tool row before
		// it) to the session file while the turn memory is hot.
		// Without this, WriteNewTranscript only fires at zot exit,
		// meaning a crash or ungraceful kill drops the whole
		// conversation. FlushSession is idempotent (it advances the
		// baseline so subsequent flushes only write new rows).
		flush := i.cfg.FlushSession
		i.mu.Unlock()
		if flush != nil {
			flush()
		}
		i.mu.Lock()
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
	case core.EvAssistantStart:
		// Fires at the top of every oneTurn, including follow-up
		// turns after tool use. Without this, the streaming buffer
		// is still marked off from the previous assistant message
		// and the final summary text pops in all at once instead
		// of typewriter-streaming delta by delta.
		i.streaming.Reset()
		i.streamPending = i.streamPending[:0]
		i.streamFlushPending = false
		i.streamOn = true
		// Clear the live tool-call overlay. Any tools from the
		// previous round are now fully folded into the transcript
		// (assistant tool_use block + tool role message with the
		// result), so keeping them in the overlay would duplicate
		// them in the view — once inside the finalised transcript
		// and once below the streaming block, with the streaming
		// summary sandwiched in between. The next EvToolUseStart
		// will populate fresh entries for this turn's tools.
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
	case core.EvTextDelta:
		// Buffer into streamPending; the paintPace ticker drains
		// it into i.streaming a few runes at a time for a smooth
		// typewriter effect independent of upstream chunk size.
		i.streamPending = append(i.streamPending, []rune(e.Delta)...)
		i.streamOn = true
	case core.EvAssistantMessage:
		// OnAssistant + telegram mirroring always fire on message
		// arrival — they read the FINAL message content, which is
		// complete regardless of what's still in the pacer.
		i.assistantMessageSideEffects(e.Message)
		// If the pacer still has characters to drain, keep streamOn
		// true and mark flush pending; the paintPace ticker will
		// drain the remainder and reset streaming state when done.
		// Otherwise (rare: full-replay sessions, abort paths) clear
		// synchronously so a later render doesn't show stale text.
		if len(i.streamPending) > 0 {
			i.streamFlushPending = true
			return
		}
		i.resetStreamingStateLocked()
		// Force full repaint: assistant text committed, layout shifts.
		if i.rend != nil {
			i.rend.Invalidate()
		}
	case core.EvToolUseStart:
		// Live streaming: pre-create the view so the user sees the
		// tool call being composed in real time. Any subsequent
		// EvToolCall for the same ID updates the same struct (the
		// final parsed args + name are already known here).
		if _, exists := i.toolCalls[e.ID]; !exists {
			i.toolCalls[e.ID] = &tui.ToolCallView{
				ID:        e.ID,
				Name:      e.Name,
				Streaming: true,
			}
			i.toolOrder = append(i.toolOrder, e.ID)
		}
	case core.EvToolUseArgs:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.RawJSONBuf += e.Delta
			// Refresh the live path as soon as it parses; used in
			// the header (write /Users/pat/Desktop/demo.ts)
			// while the content is still streaming.
			if p, pok, _ := tui.ExtractPartialStringField(tc.RawJSONBuf, "path"); pok {
				tc.LivePath = p
			} else if p, pok, _ := tui.ExtractPartialStringField(tc.RawJSONBuf, "file_path"); pok {
				tc.LivePath = p
			}
		}
	case core.EvToolUseEnd:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Streaming = false
		}
	case core.EvToolCall:
		// If we already pre-created the view during streaming, just
		// refresh the final Args summary. Otherwise create a new one
		// (non-streaming providers or legacy paths).
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Args = tui.ShortArgs(e.Name, e.Args)
			tc.Streaming = false
		} else {
			i.toolCalls[e.ID] = &tui.ToolCallView{
				ID:   e.ID,
				Name: e.Name,
				Args: tui.ShortArgs(e.Name, e.Args),
			}
			i.toolOrder = append(i.toolOrder, e.ID)
		}
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
		// Force full repaint: tool completion shifts the layout
		// (collapsed body replaces streaming) and the diff renderer
		// may leave stale code fragments on status bar rows.
		if i.rend != nil {
			i.rend.Invalidate()
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
		// Force full repaint: the turn ending shifts layout (streaming
		// buffer flushed, status bar updates) and the diff renderer
		// may leave stale content on reused rows.
		if i.rend != nil {
			i.rend.Invalidate()
		}
		if e.Stop == provider.StopAborted {
			i.resetStreamingStateLocked()
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

// silence unused import in some build configs
var _ = fmt.Sprintf

// runReloadExt triggers a live reload of every extension (discovered
// + explicit). Runs on a goroutine so the TUI stays responsive; the
// Manager.Reload takes a couple of hundred ms to shut down subprocs
// and respawn them. Shows a status line throughout.
func (i *Interactive) runReloadExt(ctx context.Context) {
	if i.cfg.Extensions == nil {
		i.mu.Lock()
		i.statusErr = "no extension manager in this build"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "reloading extensions…"
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()

	go func() {
		stats := i.cfg.Extensions.Reload(ctx, 2*time.Second)
		msg := fmt.Sprintf("reloaded: %d stopped, %d loaded (%d ready)", stats.Stopped, stats.Loaded, stats.Ready)
		if len(stats.Errors) > 0 {
			msg += fmt.Sprintf(", %d error(s)", len(stats.Errors))
		}
		i.mu.Lock()
		i.statusOK = msg
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
	}()
}

// Confirm implements core.Confirmer. The agent goroutine calls
// this synchronously before every tool invocation when --no-yolo is
// active. We push the request onto the confirmDialog queue, trigger
// a redraw, and block the caller until the user answers.
//
// If the session is cancelled or the TUI exits mid-prompt, any
// pending request is refused via CancelAll so the agent doesn't
// deadlock.
func (i *Interactive) Confirm(toolName string, preview string) core.ConfirmDecision {
	resp := make(chan core.ConfirmDecision, 1)
	i.confirmDialog.Enqueue(&confirmRequest{
		toolName: toolName,
		preview:  preview,
		resp:     resp,
	})
	i.invalidate()
	return <-resp
}

// runYoloOn disables --no-yolo for the rest of the session. Tool
// calls run without prompting after this; there's intentionally no
// way to re-enable gating mid-session, if the user wants that back
// they can exit and restart with --no-yolo.
func (i *Interactive) runYoloOn() {
	if i.cfg.ConfirmGate == nil {
		i.mu.Lock()
		i.statusOK = "yolo mode is already on (no --no-yolo in this session)"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.cfg.ConfirmGate.AllowAll()
	// Also auto-allow any currently pending confirmation so the
	// agent doesn't deadlock if /yolo is typed while a prompt is
	// open.
	i.confirmDialog.AllowAllPending()
	i.mu.Lock()
	i.statusOK = "yolo engaged: no more tool-call confirmations this session"
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// openTelegramDialog shows the picker for `/telegram` with no arg.
// Items depend on current state: disconnect + status when running,
// connect + status when stopped.
func (i *Interactive) openTelegramDialog() {
	items := i.telegramMenuItems()
	if len(items) == 0 {
		i.mu.Lock()
		i.statusErr = "telegram not configured. run `zot telegram-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.telegramDialog.Open(items)
	i.invalidate()
}

// telegramMenuItems builds the dialog entries for the current
// bridge state. Returns empty when no bot.json exists so the
// caller can show a helpful status line instead of an empty menu.
func (i *Interactive) telegramMenuItems() []telegramItem {
	cfg, err := telegram.LoadConfig(i.cfg.ZotHome)
	if err != nil || cfg.BotToken == "" {
		return nil
	}
	var items []telegramItem
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		items = append(items, telegramItem{label: "disconnect", action: "disconnect", hint: "stop mirroring"})
		st := i.telegramBridge.State()
		hint := "active"
		if st.Username != "" {
			hint += " as @" + st.Username
		}
		items = append(items, telegramItem{label: "status", action: "status", hint: hint})
	} else {
		label := "connect"
		hint := "start mirroring dms into this session"
		if cfg.AllowedUserID == 0 {
			hint = "awaiting pairing (send /start to the bot once connected)"
		}
		items = append(items, telegramItem{label: label, action: "connect", hint: hint})
		items = append(items, telegramItem{label: "status", action: "status", hint: "disconnected"})
	}
	return items
}

// doTelegram dispatches one of the three explicit actions. Called
// from /telegram <action> or after the picker selects a row.
func (i *Interactive) doTelegram(action string) {
	switch action {
	case "connect":
		i.telegramConnect()
	case "disconnect":
		i.telegramDisconnect()
	case "status":
		i.telegramStatus()
	default:
		i.mu.Lock()
		i.statusErr = "unknown telegram action: " + action + " (use connect, disconnect, or status)"
		i.mu.Unlock()
		i.invalidate()
	}
}

// telegramConnect starts the bridge. Refuses if it's already
// running or if the on-disk bot.json is missing a token.
func (i *Interactive) telegramConnect() {
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		i.mu.Lock()
		i.statusOK = "telegram already connected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	cfg, err := telegram.LoadConfig(i.cfg.ZotHome)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "telegram: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if cfg.BotToken == "" {
		i.mu.Lock()
		i.statusErr = "telegram: no bot token configured. run `zot telegram-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Refuse to start when a background daemon is already polling
	// the same bot. Two concurrent long-poll consumers race each
	// update and one always loses, so DMs get half-delivered. The
	// user can `zot telegram-bot stop` first, then /telegram connect.
	if pid, alive, _ := telegram.IsRunning(i.cfg.ZotHome); alive && pid > 0 {
		i.mu.Lock()
		i.statusErr = fmt.Sprintf("telegram: bot daemon already running (pid %d). stop it with `zot telegram-bot stop` first.", pid)
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.telegramBridge = &telegram.Bridge{
		Client: telegram.NewClient(cfg.BotToken),
		Config: cfg,
		Save: func(next telegram.Config) error {
			return telegram.SaveConfig(i.cfg.ZotHome, next)
		},
		Host: &telegramHost{iv: i},
	}
	if err := i.telegramBridge.Start(i.runCtx); err != nil {
		i.telegramBridge = nil
		i.mu.Lock()
		i.statusErr = "telegram connect failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	state := i.telegramBridge.State()
	label := "telegram connected"
	if state.Username != "" {
		label += " as @" + state.Username
	}
	if state.PairedID == 0 {
		label += " — send /start to the bot from your phone to claim it"
	}
	i.mu.Lock()
	i.statusOK = label
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// telegramDisconnect stops the bridge. No-op when already stopped.
func (i *Interactive) telegramDisconnect() {
	if i.telegramBridge == nil || !i.telegramBridge.Active() {
		i.mu.Lock()
		i.statusOK = "telegram already disconnected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.telegramBridge.Stop()
	i.mu.Lock()
	i.statusOK = "telegram disconnected"
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// telegramStatus writes a one-liner describing the bridge state.
// Reports on both the in-tui bridge and the background daemon so
// the user isn't confused when the daemon owns the poll loop.
func (i *Interactive) telegramStatus() {
	var msg string
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		s := i.telegramBridge.State()
		msg = "telegram: connected (tui bridge)"
		if s.Username != "" {
			msg += " as @" + s.Username
		}
		if s.PairedID != 0 {
			msg += fmt.Sprintf(" - paired with user %d", s.PairedID)
		} else {
			msg += " - awaiting pairing"
		}
	} else if pid, alive, _ := telegram.IsRunning(i.cfg.ZotHome); alive && pid > 0 {
		msg = fmt.Sprintf("telegram: background daemon running (pid %d) - /telegram connect won't work until you stop it", pid)
	} else {
		cfg, _ := telegram.LoadConfig(i.cfg.ZotHome)
		if cfg.BotToken == "" {
			msg = "telegram: not configured. run `zot telegram-bot setup` first."
		} else {
			msg = "telegram: disconnected"
			if cfg.BotUsername != "" {
				msg += " (@" + cfg.BotUsername + " ready to connect)"
			}
		}
	}
	i.mu.Lock()
	i.statusOK = msg
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// telegramHost adapts *Interactive to telegram.Host so the bridge
// can call back into the TUI without importing modes directly.
type telegramHost struct{ iv *Interactive }

func (h *telegramHost) SubmitOrQueue(prompt string, images []provider.ImageBlock) {
	h.iv.SubmitOrQueue(prompt, images)
}

func (h *telegramHost) CancelTurn() { h.iv.CancelTurn() }

func (h *telegramHost) Notify(level, message string) {
	h.iv.mu.Lock()
	switch level {
	case "error", "warn":
		h.iv.statusErr = message
		h.iv.statusOK = ""
	default:
		h.iv.statusOK = message
		h.iv.statusErr = ""
	}
	h.iv.mu.Unlock()
	h.iv.invalidate()
}

// openSessionOpsDialog shows the picker for `/session` with no arg.
// Always offers export, import, fork, tree; the handlers bail with
// a clear status message when the precondition isn't met (empty
// transcript on fork; no parent/siblings on tree).
func (i *Interactive) openSessionOpsDialog() {
	items := []sessionOpsItem{
		{label: "export", action: "export", hint: "write the current session to a .zotsession file"},
		{label: "import", action: "import", hint: "load a .zotsession file into this directory"},
		{label: "fork", action: "fork", hint: "branch from a past user message into a new session"},
		{label: "tree", action: "tree", hint: "switch between branches in this directory"},
	}
	i.sessionOpsDialog.Open(items)
	i.invalidate()
}

// doSessionOp dispatches export, import, fork, or tree. arg is the
// optional positional argument from e.g. /session export <path>
// or /session import <path>; fork and tree ignore it.
func (i *Interactive) doSessionOp(action, arg string) {
	switch action {
	case "export":
		i.doSessionExport(arg)
	case "import":
		i.doSessionImport(arg)
	case "fork":
		i.doSessionFork()
	case "tree":
		i.doSessionTree()
	default:
		i.mu.Lock()
		i.statusErr = "unknown /session action: " + action + " (use export, import, fork, or tree)"
		i.mu.Unlock()
		i.invalidate()
	}
}

// doSessionExport writes the live session file to destination path
// dst. When dst is empty we default to ~/Downloads (falling back to
// the user's home directory if it doesn't exist). The helper
// expands a leading `~` and creates any missing parent directories.
func (i *Interactive) doSessionExport(dst string) {
	if i.cfg.CurrentSessionPath == nil {
		i.mu.Lock()
		i.statusErr = "export: no session is active (running with --no-session?)"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	src := i.cfg.CurrentSessionPath()
	if src == "" {
		i.mu.Lock()
		i.statusErr = "export: no session is active (running with --no-session?)"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Persist any in-memory agent messages to the session file so
	// the export carries the full conversation. Without this, the
	// default lazy-flush-at-exit strategy leaves most of a running
	// session unwritten and the export ends up with only the meta.
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	dst = unquotePath(dst)
	if dst == "" {
		dst = defaultExportDir()
	} else {
		dst = expandTilde(dst)
	}
	out, err := core.ExportSession(src, dst)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "export: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "exported session to " + friendlyPath(out)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// doSessionImport copies the .zotsession file at src into the
// running cwd's sessions directory and loads it as the active
// session, same as `/sessions` -> pick. When src is empty we ask
// the user to pass a path (no usable default here).
func (i *Interactive) doSessionImport(src string) {
	src = unquotePath(src)
	if src == "" {
		i.mu.Lock()
		i.statusErr = "import: pass a path — e.g. /session import ~/Downloads/work.zotsession"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	src = expandTilde(src)
	if _, err := os.Stat(src); err != nil {
		i.mu.Lock()
		i.statusErr = "import: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	newPath, err := core.ImportSession(src, i.cfg.ZotHome, i.cfg.CWD, i.cfg.Version)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "import: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusOK = "imported session at " + friendlyPath(newPath) + " (run /sessions to resume it)"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.mu.Lock()
		i.statusErr = "import: load failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "imported and switched to session " + friendlyPath(newPath)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// defaultExportDir returns ~/Downloads when it exists, or ~ as a
// fallback, or /tmp on exotic machines with no home dir.
func defaultExportDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return os.TempDir()
	}
	downloads := filepath.Join(home, "Downloads")
	if fi, err := os.Stat(downloads); err == nil && fi.IsDir() {
		return downloads
	}
	return home
}

// expandTilde turns a leading ~ into the user's home directory.
// Returns the input unchanged when there's no tilde or no home.
func expandTilde(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if len(p) == 1 {
		return home
	}
	if p[1] == '/' || p[1] == filepath.Separator {
		return filepath.Join(home, p[2:])
	}
	return p
}

// unquotePath strips a matching pair of surrounding single or
// double quotes. Drag-drop paste in the tui auto-quotes dropped
// file paths so the shell-like `/session import 'foo bar.zs'`
// stays well-formed; when the TUI's own slash handler consumes
// the arg, we want the raw path back.
func unquotePath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 {
		first := p[0]
		last := p[len(p)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			p = p[1 : len(p)-1]
		}
	}
	return p
}

// friendlyPath collapses the user's home directory to a leading ~
// so status messages read cleanly. Falls back to the raw path when
// the home dir is unknown.
func friendlyPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// doSessionFork opens the /jump turn picker in "fork mode". The
// next selection branches the current session at that user turn
// instead of scrolling the viewport to it.
func (i *Interactive) doSessionFork() {
	if i.cfg.CurrentSessionPath == nil || i.cfg.CurrentSessionPath() == "" {
		i.mu.Lock()
		i.statusErr = "fork: no session is active (running with --no-session?)"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	msgs := []provider.Message{}
	if i.agent != nil {
		msgs = i.agent.Messages()
	}
	if len(msgs) == 0 {
		i.mu.Lock()
		i.statusErr = "fork: transcript is empty; nothing to fork from"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.pendingFork = true
	i.jumpDialog.Open(msgs, "")
	i.invalidate()
}

// doSessionTree shows the branch topology picker for this cwd.
// Pick an entry to switch into it (same semantics as /sessions
// picking a past session, but with the parent/child indentation).
func (i *Interactive) doSessionTree() {
	if i.cfg.ZotHome == "" || i.cfg.CWD == "" {
		i.mu.Lock()
		i.statusErr = "tree: session storage not configured"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Flush the running agent's transcript first so its message
	// count + latest preview are accurate in the tree.
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	roots := core.BuildSessionTree(i.cfg.ZotHome, i.cfg.CWD)
	if len(roots) == 0 {
		i.mu.Lock()
		i.statusErr = "tree: no sessions in this directory yet"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	current := ""
	if i.cfg.CurrentSessionPath != nil {
		current = i.cfg.CurrentSessionPath()
	}
	if !i.sessionTreeDialog.Open(roots, current) {
		i.mu.Lock()
		i.statusErr = "tree: no branches to show"
		i.mu.Unlock()
	}
	i.invalidate()
}

// applySessionTreeSelection switches the running agent to the
// session file at path. Thin wrapper around LoadSession that also
// writes a status line.
func (i *Interactive) applySessionTreeSelection(path string) {
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusErr = "tree: session swap not available in this build"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if err := i.cfg.LoadSession(path); err != nil {
		i.mu.Lock()
		i.statusErr = "tree: load failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "switched to branch " + friendlyPath(path)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// applyForkSelection branches the current session at msgIdx+1 (so
// the selected user message and everything before it is included
// in the new branch), then switches the running agent to the new
// file. Called from the jump-dialog handler when pendingFork=true.
func (i *Interactive) applyForkSelection(msgIdx int) {
	i.pendingFork = false
	if i.cfg.CurrentSessionPath == nil {
		i.mu.Lock()
		i.statusErr = "fork: no session is active"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	src := i.cfg.CurrentSessionPath()
	if src == "" {
		i.mu.Lock()
		i.statusErr = "fork: no session is active"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	// msgIdx is 0-indexed message position; copy msgIdx+1 rows so
	// the selected user message is included.
	upTo := msgIdx + 1
	newPath, err := core.BranchSession(src, i.cfg.ZotHome, i.cfg.CWD, i.cfg.Version, upTo)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "fork: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusOK = "forked at message " + formatInt(upTo) + " (run /sessions to resume)"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.mu.Lock()
		i.statusErr = "fork: switch failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "forked and switched to new branch at " + friendlyPath(newPath)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// formatInt is a tiny strconv.Itoa shim; keeps the handler above
// from needing a strconv import just for one call.
func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
}

// assistantText returns the concatenated text of every TextBlock in
// m. Used by the streaming-view dedupe guard to tell when a live
// streamed reply has already been promoted into the transcript.
func assistantText(m provider.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// resetStreamingStateLocked clears every piece of streaming state
// in one shot. Used by abort paths (turn cancel, compact finish,
// queue drain) so the pacer doesn't keep draining stale runes from
// a prior turn. Must be called with i.mu held.
func (i *Interactive) resetStreamingStateLocked() {
	i.streaming.Reset()
	i.streamPending = i.streamPending[:0]
	i.streamFlushPending = false
	i.streamOn = false
}

// assistantMessageSideEffects runs the non-visual hooks attached to
// EvAssistantMessage: the host-provided OnAssistant callback and the
// telegram-bridge mirror. Called with i.mu held.
//
// Factored out of handleEvent because the streaming pacer may defer
// visual reset until after the last buffered rune has painted, but
// the callbacks themselves must fire on message arrival so
// downstream observers (session persistence, telegram, cost panels)
// don't wait on a UI animation to catch up.
func (i *Interactive) assistantMessageSideEffects(m provider.Message) {
	if i.cfg.OnAssistant != nil {
		i.cfg.OnAssistant(m)
	}
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		var sb strings.Builder
		for _, c := range m.Content {
			if tb, ok := c.(provider.TextBlock); ok {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(tb.Text)
			}
		}
		if text := sb.String(); strings.TrimSpace(text) != "" {
			go i.telegramBridge.OnAssistantText(text)
		}
	}
}

// paintPaceRate is how many runes the streaming pacer releases per
// tick. With a 16ms tick, 6 runes/tick is ~375 runes/s — fast enough
// that a 500-rune summary finishes in ~1.3s, slow enough to look
// like a human typing. Empirically matches the feel of provider
// paths that already drip-stream natively.
const paintPaceRate = 6

// paintPaceInterval is the tick interval for the streaming pacer.
// 16ms lines up with the redraw throttle so we never paint faster
// than the terminal can keep up.
const paintPaceInterval = 16 * time.Millisecond

// runStreamPacer drains buffered deltas from streamPending into
// streaming a small batch per tick, invalidating after each move.
// It stops when the context cancels (tui shutdown).
//
// Why a pacer: providers differ wildly in how they chunk their
// text_delta events. The API-key path on Anthropic emits ~30 drips
// for a 400-token summary; the OAuth path can coalesce the same
// summary into 3 fat chunks, visually indistinguishable from "the
// whole reply just appeared". The pacer normalizes that so every
// path looks the same on screen.
func (i *Interactive) runStreamPacer(ctx context.Context) {
	t := time.NewTicker(paintPaceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			i.mu.Lock()
			if len(i.streamPending) == 0 {
				// EvAssistantMessage already fired but the pacer
				// was still draining a tick ago. Everything is now
				// painted; clear the streaming flags so the next
				// redraw shows the finalised transcript message
				// and hides the streaming overlay.
				if i.streamFlushPending {
					i.streamFlushPending = false
					i.streaming.Reset()
					i.streamOn = false
					i.mu.Unlock()
					i.invalidate()
					continue
				}
				i.mu.Unlock()
				continue
			}
			n := paintPaceRate
			if n > len(i.streamPending) {
				n = len(i.streamPending)
			}
			i.streaming.WriteString(string(i.streamPending[:n]))
			i.streamPending = i.streamPending[n:]
			i.mu.Unlock()
			i.invalidate()
		}
	}
}
