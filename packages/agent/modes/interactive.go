package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/agent/modes/telegram"
	"github.com/patriceckhart/zot/packages/agent/modes/matrix"
	"github.com/patriceckhart/zot/packages/agent/modes/bot"
	"github.com/patriceckhart/zot/packages/agent/skills"
	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
	"github.com/patriceckhart/zot/packages/tui"
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

	// InlineImagesEnabled overrides terminal image rendering. nil means
	// auto-detect and render when supported; false disables; true uses
	// the detected protocol when available.
	InlineImagesEnabled *bool

	// AutoSwarmEnabled mirrors the persisted config flag at startup so
	// the /settings dialog can render the current state without
	// re-reading config.json on every open.
	AutoSwarmEnabled *bool

	// RecursiveFileSuggest mirrors the persisted recursive_file_suggest
	// flag at startup. When true the @-mention picker fuzzy-searches the
	// whole project tree instead of browsing one directory at a time.
	RecursiveFileSuggest *bool

	// RespectGitignore mirrors the persisted respect_gitignore flag at
	// startup. nil means the default (on); when false the @-mention
	// picker shows files matched by the project's root .gitignore.
	RespectGitignore *bool

	// ThemeName mirrors the persisted config theme value. Empty means auto.
	ThemeName string

	// FlatTools renders tool calls without the bordered panel (a quiet
	// header line plus indented, frameless output). Mirrors the
	// resolved tool_render config / ZOT_FLAT_TOOLS env at startup.
	FlatTools bool

	// CompactUser renders sent user messages as a single quiet gutter
	// line instead of a padded, tinted bubble. Mirrors the resolved
	// compact_input config / ZOT_COMPACT_INPUT env at startup.
	CompactUser bool

	// CompactMode mirrors the persisted compact_mode flag at startup.
	// When true, it enables both flat tool calls and compact user turns.
	CompactMode *bool

	// TUIInputStyle controls the main input rendering: plain or lines.
	TUIInputStyle string

	// TUIStatusPosition controls whether the status block renders above
	// or below the main input.
	TUIStatusPosition string

	// TUIWorkingPosition controls whether the busy/working spinner renders
	// above or below the main input.
	TUIWorkingPosition string

	// QuickModelShortcuts maps slots 1-9 to provider/model pairs. The
	// shortcuts are Ctrl+1..9. Cmd+1..9 may also work when the terminal
	// forwards Command/Super keypresses, but Ctrl is the displayed chord.
	QuickModelShortcuts []QuickModelShortcut

	// ExtensionThemes returns themes bundled with loaded extensions.
	ExtensionThemes func() []tui.ThemeOption

	// AutoSwarmSystemAddendum is the system-prompt block that gets
	// appended/stripped when the user toggles auto-swarm at runtime.
	// Plumbed in from the cli so this package doesn't have to import
	// agent (cycle).
	AutoSwarmSystemAddendum string
	SettingsStore           SettingsStore

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

	// SetKimiCLIFallbackDisabled controls whether zot may fall back to
	// the official Kimi Code CLI token when zot has no stored Kimi token.
	SetKimiCLIFallbackDisabled func(disabled bool) error

	// BuildAgentFor rebuilds the agent with an explicit provider/model
	// override (used by the /model picker when switching providers).
	// If providerOverride is empty, the current provider is kept.
	BuildAgentFor func(providerOverride, modelOverride string) (*core.Agent, string, string, error)

	// BuildAgentForRescue rebuilds the agent for the rescue picker that
	// opens after a recoverable provider failure. Unlike BuildAgentFor,
	// this builder drops launch-time --api-key and --base-url overrides
	// because those are usually the reason rescue triggered. Re-resolves
	// credentials from env vars / auth.json / provider defaults so the
	// retry has a real chance of succeeding. Falls back to BuildAgentFor
	// when nil so embedders that don't wire it keep working.
	BuildAgentForRescue func(providerOverride, modelOverride string) (*core.Agent, string, string, error)

	// LoggedInProviders returns the list of provider names that
	// currently have credentials. Used by /model to filter the
	// picker to only show reachable models.
	LoggedInProviders func() []string

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

	// ChangeCWD switches the running zot session's working directory
	// to path. The host closes the current session, rebuilds the
	// agent so tools / AGENTS.md / sandbox bind to the new cwd, and
	// opens a fresh session there. Returns an error if path doesn't
	// exist, isn't a directory, or the host can't rebuild the agent.
	//
	// Optional: not wired by every embedder. When nil the hidden /cd
	// command surfaces a clear error rather than no-oping.
	ChangeCWD func(path string) error

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

	// Swarm, if non-nil, enables the /swarm slash command and the
	// dashboard dialog. The cli constructs the Swarm once per
	// interactive run and tears it down on exit. nil disables the
	// feature entirely (used by embedders / tests that don't want
	// subprocesses).
	Swarm *swarm.Swarm

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
type chatCacheKey struct {
	cols            int
	agentRev        uint64
	statusOK        string
	statusErr       string
	help            string
	extNotes        string
	shellBlock      string
	updateAvailable bool
	updateCurrent   string
	updateLatest    string
	updateURL       string
	welcomeShowVer  bool
	expandAll       bool
	tailLimit       int
}

// QuickModelShortcut is one configured quick model switch slot.
type QuickModelShortcut struct {
	Provider string
	Model    string
}

// SettingsStore persists user-toggleable settings surfaced by /settings.
type SettingsStore interface {
	SetQuickModelShortcut(slot int, providerName, model string) error
	SetInlineImages(enabled bool) error
	SetAutoSwarm(enabled bool) error
	SetRecursiveFileSuggest(enabled bool) error
	SetRespectGitignore(enabled bool) error
	SetCompactMode(enabled bool) error
	SetTUIInputStyle(style string) error
	SetTUIStatusPosition(position string) error
	SetTUIWorkingPosition(position string) error
	SetReasoning(level string) error
	SetTheme(name string) error
}

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
	// toolGate records, per tool-call id, how many runes of paced
	// assistant text must have drained into `streaming` before that
	// tool block may appear. It exists to make stream ordering
	// deterministic: a tool call can arrive from the provider while
	// the prose that logically precedes it is still being typed out
	// by the pacer. Without gating, the tool block would render
	// immediately while the intro paragraph keeps filling in below
	// it. We snapshot the total expected stream length (already
	// streamed + still pending) at the moment the tool starts, and
	// hold the block back until the pacer reaches it.
	toolGate         map[string]int
	statusErr        string
	statusOK         string
	liveBlock        []string // live streaming/tool progress rendered outside scrollback
	helpBlock        []string // rendered above the chat when /help was typed
	cumUsage         provider.Usage
	lastCtxInput     int // input_tokens of the most recent turn — approximates current context size
	busy             bool
	dirty            chan struct{}
	cancelTurn       context.CancelFunc
	scrollOffset     int // rows from the bottom; 0 = pinned to latest
	prevScrollOffset int // last value redraw snapped against; tracks intent

	// prevChatLen and prevChatCols track the chat buffer's size at the
	// last redraw so that when content grows below the user's viewport
	// while they're scrolled up reading history, we can bump
	// scrollOffset by exactly the growth and keep the visible content
	// pinned. Without this, every streamed line shifts the visible
	// window down through the buffer (because scrollOffset is measured
	// from the bottom) and the user's reading position drifts upward
	// and off the top.
	prevChatLen     int
	prevChatCols    int
	prevChatRows    int
	prevOverlayOpen bool

	// chatCache stores the built transcript/status-note rows for idle
	// frames. Editor typing changes only the bottom input region, so
	// reusing this cache avoids copying/walking/reassembling a long
	// session on every keypress.
	chatCache      []string
	chatCacheKey   chatCacheKey
	chatCacheValid bool

	// Messages typed while a turn is in flight. Each is delivered as
	// its own follow-up turn once the current one finishes. Rendered
	// above the status bar as "sliding in: ..." chips.
	queued []string

	// runCtx is the top-level context passed to Run(). Follow-up turns
	// drained from `queued` are started against this context so they
	// survive past the ctx of the key event that enqueued them.
	runCtx context.Context

	// pendingPostCompactNote is a status_ok message to surface after
	// a successful auto-compact pass triggered by a 413 or by the
	// pre-turn fraction guard. Cleared by runCompact once shown.
	pendingPostCompactNote string

	// autoCompacting is true while a model-triggered compaction is in
	// flight. Surfaced in the status bar so the user can tell a
	// condense pass from a regular assistant turn.
	autoCompacting bool

	// updateInfo is the result of the async update check. Zero value
	// while the check hasn't completed or nothing is available.
	updateInfo UpdateInfo

	dialog            *loginDialog
	modelDialog       *modelDialog
	rescueDialog      *rescueDialog
	sessionDialog     *sessionDialog
	swarmDialog       *swarmDialog
	jumpDialog        *jumpDialog
	btwDialog         *btwDialog
	skillsDialog      *skillsDialog
	changelogDialog   *changelogDialog
	confirmDialog     *confirmDialog
	logoutDialog      *logoutDialog
	telegramDialog    *telegramDialog
	settingsDialog    *settingsDialog
	quickModelAssign  int
	telegramBridge    *telegram.Bridge
	matrixDialog      *matrixDialog
	matrixBridge      *matrix.Bridge
	sessionOpsDialog  *sessionOpsDialog
	sessionTreeDialog *sessionTreeDialog
	extPanel          *extPanelDialog

	// swarmWatch tracks auto-swarm sub-agents the main agent spawned
	// via swarm_spawn. Each entry holds the agent + the task text;
	// a per-entry goroutine waits on the agent's terminal state. When
	// every tracked entry has finished, the watcher flushes a single
	// summary turn into the main chat (queued if the main agent is
	// busy, run immediately if idle).
	swarmWatchMu sync.Mutex
	swarmWatch   []*swarmWatchEntry

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

	// inputHistoryIndex is -1 when not browsing history. When the
	// editor is empty, Up/Down can walk previous user prompts without
	// stealing normal vertical cursor movement in non-empty input.
	inputHistoryIndex int

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

	// shellBlock holds the rendered terminal-log lines of the most
	// recent !command shell escape. It lives below the transcript
	// (under extNotes) until the user sends their next prompt or runs
	// /clear. shellRunning is true while a !command is executing; it
	// shares i.busy/i.cancelTurn so esc cancels it and no turn or
	// other shell escape can start while one is in flight.
	shellBlock   []string
	shellRunning bool

	// sessionLoading is true while a /sessions selection is being read
	// on a background goroutine. Keeping this off the input goroutine
	// lets ctrl+c/exit remain responsive for very large JSONL sessions.
	sessionLoading bool

	// pendingRescuePrompt / pendingRescueImages stash the prompt and
	// images that should be re-run after the user picks a model in
	// the rescue dialog. Cleared once applyRescueSelection consumes
	// them (or when the dialog is dismissed via esc).
	pendingRescuePrompt string
	pendingRescueImages []provider.ImageBlock

	clipboardImages []clipboardImageAttachment
}

// welcomeVersionDuration is how long the welcome banner shows the
// version suffix before reverting to the plain headline. 1.5s is
// enough to read at a glance and keeps the splash short.
const welcomeVersionDuration = 1500 * time.Millisecond

// initialResumeTailLimit caps how many messages from a freshly-resumed
// transcript we render on the first paint. The full transcript is
// still in memory; older messages are rendered (and their cached
// lines kept for the lifetime of the View) as soon as the user
// scrolls past the rendered tail. Picked to comfortably fill the
// largest realistic terminal viewport while keeping first paint
// snappy on multi-thousand-message sessions where markdown / syntax
// highlighting dominates the redraw cost.
const initialResumeTailLimit = 80

// resumeTailExpandStep is how many additional messages the tail
// limit grows by each time the user scrolls past the currently
// rendered top. Pre-rendering this many messages on a single tick
// keeps scroll-up smooth without falling back to a one-by-one
// reveal that would feel jerky.
const resumeTailExpandStep = 80

// NewInteractive constructs an Interactive from cfg.
func NewInteractive(cfg InteractiveConfig) *Interactive {
	renderer := tui.NewRenderer(cfg.Terminal)
	renderer.SetTheme(cfg.Theme)
	i := &Interactive{
		cfg: cfg,
		view: &tui.View{
			Theme:       cfg.Theme,
			ImageProto:  effectiveImageProtocol(cfg.InlineImagesEnabled),
			FlatTools:   cfg.FlatTools,
			CompactUser: cfg.CompactUser,
			CompactMode: cfg.CompactMode != nil && *cfg.CompactMode,
		},
		// Prompt is the standard half-block accent bar used by chat
		// speaker labels too, so the input gutter matches the rest
		// of the UI.
		ed:                tui.NewEditor(cfg.Theme.AccentBar(cfg.Theme.Accent)),
		rend:              renderer,
		toolCalls:         map[string]*tui.ToolCallView{},
		toolGate:          map[string]int{},
		dirty:             make(chan struct{}, 8),
		dialog:            newLoginDialog(),
		modelDialog:       newModelDialog(),
		rescueDialog:      newRescueDialog(),
		sessionDialog:     newSessionDialog(),
		swarmDialog:       newSwarmDialog(),
		jumpDialog:        newJumpDialog(),
		btwDialog:         newBtwDialog(),
		skillsDialog:      newSkillsDialog(),
		changelogDialog:   newChangelogDialog(),
		confirmDialog:     newConfirmDialog(),
		logoutDialog:      newLogoutDialog(),
		telegramDialog:    newTelegramDialog(),
		matrixDialog:      newMatrixDialog(),
		settingsDialog:    newSettingsDialog(),
		sessionOpsDialog:  newSessionOpsDialog(),
		sessionTreeDialog: newSessionTreeDialog(),
		extPanel:          newExtPanelDialog(),
		suggest:           newSlashSuggester(),
		fileSuggest:       newFileSuggester(),
		spin:              newSpinner(cfg.Theme),
		inputHistoryIndex: -1,
	}
	i.fileSuggest.SetRecursive(cfg.RecursiveFileSuggest != nil && *cfg.RecursiveFileSuggest)
	i.fileSuggest.SetRespectGitignore(cfg.RespectGitignore == nil || *cfg.RespectGitignore)
	if cfg.Agent != nil {
		i.agent = cfg.Agent
		i.view.Messages = cfg.Agent.Messages()
		i.cumUsage = cfg.Agent.Cost()
		// Rehydrate the "context used" gauge from the last persisted
		// turn. Without this the status bar reads 0.0% after a resume
		// until the next turn lands a usage event.
		if last := cfg.Agent.LastTurnUsage(); last.InputTokens > 0 || last.CacheReadTokens > 0 || last.CacheWriteTokens > 0 {
			i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
		}
		// Cap the first paint at the tail of the transcript so
		// resuming a multi-thousand-message session doesn't block
		// on rendering every prior turn before showing anything.
		if len(i.view.Messages) > initialResumeTailLimit {
			i.view.TailLimit = initialResumeTailLimit
		}
	}
	if cfg.AutoSwarmEnabled != nil && *cfg.AutoSwarmEnabled {
		i.applyAutoSwarmTool(true)
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
		if i.matrixBridge != nil {
			i.matrixBridge.Stop()
		}
	}()

	// Enabling mouse reporting steals click-drag selection from the
	// host terminal (VS Code, Ghostty, iTerm). The user prefers native
	// selection over the wheel-speed boost, so we no longer turn it
	// on automatically. Wheel events fall through to the terminal's
	// own scrollback handler.
	// Keep zot on the terminal's main screen. We intentionally do not
	// enter the alternate-screen buffer (CSI ?1049h). The renderer emits
	// chat as normal terminal flow/scrollback and redraws only the live
	// input/status block on normal typing.
	_, _ = term.Write([]byte(tui.SeqBracketedPasteOn + tui.SeqEnhancedKeyboardOn + tui.SeqResetScrollRegion + tui.SeqDeleteKittyImages + tui.SeqClearScreenNoHome + tui.SeqClearScrollback + tui.MoveTo(1, 1)))
	// Tell the terminal our working directory (OSC 7) so "new tab /
	// split here" opens in the launch cwd instead of inheriting a stale
	// directory from an extension subprocess (see issue #38). Harmless
	// on terminals that ignore it.
	if seq := tui.ReportCWD(i.cfg.CWD); seq != "" {
		_, _ = term.Write([]byte(seq))
	}
	defer term.Write([]byte(tui.SeqResetScrollRegion + tui.SeqDeleteKittyImages + tui.SeqEnhancedKeyboardOff + tui.SeqBracketedPasteOff + tui.ResetCursorColor() + tui.ResetCursorShape() + tui.SeqShowCursor))
	i.applyInputCursorColor()

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
	// (--continue, --resume, --session) pin the viewport at the
	// bottom so the most recent reply (and any prompt the user just
	// typed) is fully visible. Earlier behaviour parked the view at
	// the last user turn, which could leave the latest message clipped
	// off the bottom of the page on long sessions.
	if i.agent != nil {
		if msgs := i.agent.Messages(); len(msgs) > 0 {
			i.scrollToBottom()
		}
	}

	// No credential at startup? Auto-open the login dialog, and mark
	// the status line. The user can Esc out of the dialog if they
	// want to dismiss it (e.g. to check /help or /exit first).
	if i.agent == nil {
		i.statusErr = "not logged in. pick a login method below or press esc to dismiss."
		i.dialog.Open(i.cfg.ZotHome)
	}

	// Input goroutine. Buffered generously so a drag-drop that the
	// terminal delivers as a burst of single-character key events
	// (no bracketed paste) can be drained in one main-loop pass
	// instead of triggering a redraw per character.
	keys := make(chan tui.Key, 256)
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
			// Drain any keystrokes that arrived during this iteration.
			// VS Code (and other terminals that don't bracket drops as
			// paste) deliver a path one rune at a time — without this
			// loop the editor would render between every rune and a
			// long path on a heavy transcript would visibly type in.
		drain:
			for {
				select {
				case k2 := <-keys:
					if done := i.handleKey(ctx, k2); done {
						return nil
					}
				default:
					break drain
				}
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
			//
			// The swarm dashboard is also animated: its rows reflect
			// background subprocesses whose activity / age change
			// without any user input. Without the tick redraw the
			// dashboard freezes on the snapshot taken when the user
			// last pressed a key. We exclude the dashboard when one
			// of its inline editors (spawn task or prompt composer)
			// is active so the cursor blink in those editors works
			// the same way it does inside btw.
			if i.busy || i.btwDialog.Loading() || i.swarmDialog.NeedsTickRefresh() {
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

func (i *Interactive) cachedChatLocked(cols int) []string {
	key, cacheable := i.chatCacheKeyLocked(cols)
	if cacheable && i.chatCacheValid && i.chatCacheKey == key {
		return append([]string(nil), i.chatCache...)
	}
	chat := i.buildChatLocked(cols)
	if cacheable {
		i.chatCache = append(i.chatCache[:0], chat...)
		i.chatCacheKey = key
		i.chatCacheValid = true
	} else {
		i.chatCacheValid = false
	}
	return chat
}

func (i *Interactive) chatCacheKeyLocked(cols int) (chatCacheKey, bool) {
	// Live turns mutate streaming/tool-call state at high frequency;
	// keep those on the old rebuild path. The cache targets the common
	// idle case where only the editor contents changed between redraws.
	if i.busy || i.streamOn || i.streamFlushPending {
		return chatCacheKey{}, false
	}
	var rev uint64
	if i.agent != nil {
		rev = i.agent.Revision()
	}
	showVer := len(i.view.Messages) == 0 && !i.streamOn && len(i.toolOrder) == 0 && !i.welcomeStart.IsZero() && time.Since(i.welcomeStart) < welcomeVersionDuration
	return chatCacheKey{
		cols:            cols,
		agentRev:        rev,
		statusOK:        i.statusOK,
		statusErr:       i.statusErr,
		help:            strings.Join(i.helpBlock, "\n"),
		extNotes:        strings.Join(i.extNotes, "\n"),
		shellBlock:      strings.Join(i.shellBlock, "\n"),
		updateAvailable: i.updateInfo.Available,
		updateCurrent:   i.updateInfo.Current,
		updateLatest:    i.updateInfo.Latest,
		updateURL:       i.updateInfo.URL,
		welcomeShowVer:  showVer,
		expandAll:       i.view.ExpandAll,
		tailLimit:       i.view.TailLimit,
	}, true
}

func (i *Interactive) buildChatLocked(cols int) []string {
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
			// Deterministic ordering: a tool block stays hidden until
			// the paced assistant text that preceded it has finished
			// typing out. toolOrder is append-only in arrival order,
			// so once one tool is still gated, every later tool is too,
			// so stop here to avoid showing a tool out of sequence.
			if !i.toolGateOpenLocked(id) {
				break
			}
			if tc, ok := i.toolCalls[id]; ok {
				i.view.ToolCalls = append(i.view.ToolCalls, *tc)
			}
		}
	}
	i.view.Err = i.statusErr
	// Live streaming/tool rows are appended to the chat buffer (not
	// hoisted into a separate live block above the editor). That keeps
	// the renderer's diff view append-only: when a tool finishes the
	// rows update in place at the end of the buffer, instead of the
	// whole bottom band shrinking and shifting chat lines around.
	i.liveBlock = nil
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
			line = line[:cols-3] + "..."
		}
		chat = append(chat, i.cfg.Theme.FG256(i.cfg.Theme.Tool, line), "")
	}

	// Extension notes (notify / display) live just under the
	// transcript, above the dialog/editor band. Cleared by /clear.
	if len(i.extNotes) > 0 {
		chat = append(chat, i.extNotes...)
		chat = append(chat, "")
	}

	// Shell-escape terminal-log block (!command). Rendered below the
	// transcript and extension notes; cleared when the next prompt is
	// sent or on /clear so it never leaks into the model conversation.
	if len(i.shellBlock) > 0 {
		chat = append(chat, i.shellBlock...)
		chat = append(chat, "")
	}

	// Strip trailing blank rows so the chat content sits flush
	// against the new "blank above status bar" row added by the
	// bottom-region assembly. Build() ends every message with a
	// blank separator; without this trim, the final message in
	// the transcript would have its own trailing blank plus the
	// status block's leading blank, doubling the gap.
	for len(chat) > 0 && strings.TrimSpace(chat[len(chat)-1]) == "" {
		chat = chat[:len(chat)-1]
	}
	return chat
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

// anchorScrollOffset keeps the user's reading position pinned when the
// chat buffer grows/shrinks or the viewport height changes between two
// redraws while they're scrolled up.
//
// scrollOffset is measured from the bottom of the chat buffer, so the
// top visible row is start = chatLen - scrollOffset - chatRows. To hold
// `start` constant we adjust the offset by the buffer-length delta minus
// the viewport-height delta. The result is clamped to [0, newLen] so a
// shrinking buffer can't push it negative.
func anchorScrollOffset(offset, prevLen, newLen, prevRows, newRows int) int {
	adj := (newLen - prevLen) - (newRows - prevRows)
	offset += adj
	if offset < 0 {
		offset = 0
	}
	if offset > newLen {
		offset = newLen
	}
	return offset
}

// scrollToBottom pins the view to the latest content.
func (i *Interactive) scrollToBottom() {
	i.mu.Lock()
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	// Reset the auto-follow baseline. scrollToBottom is the resume /
	// session-swap snap point, where the chat buffer changes length
	// wholesale. Without zeroing these, the next render's follow guard
	// compares the fresh transcript's length against a stale baseline
	// and nudges scrollOffset, which reads as a viewport jump right
	// after resume. See commit 43da5e5 for the same fix on new turns.
	i.prevChatLen = 0
	i.prevChatCols = 0
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
	chat := i.cachedChatLocked(cols)

	// Dialogs (login or model picker) render between chat and the editor.
	var dialog []string
	switch {
	case i.dialog.Active():
		dialog = i.dialog.Render(i.cfg.Theme, cols)
	case i.modelDialog.Active():
		dialog = i.modelDialog.Render(i.cfg.Theme, cols)
	case i.rescueDialog.Active():
		dialog = i.rescueDialog.Render(i.cfg.Theme, cols)
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
	case i.swarmDialog.Active():
		dialog = i.swarmDialog.Render(i.cfg.Theme, cols)
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
	case i.matrixDialog.Active():
		dialog = i.matrixDialog.Render(i.cfg.Theme, cols)
	case i.settingsDialog.Active():
		dialog = i.settingsDialog.Render(i.cfg.Theme, cols)
	case i.sessionOpsDialog.Active():
		dialog = i.sessionOpsDialog.Render(i.cfg.Theme, cols)
	case i.sessionTreeDialog.Active():
		dialog = i.sessionTreeDialog.Render(i.cfg.Theme, cols)
	case i.extPanel.Active():
		dialog = i.extPanel.Render(i.cfg.Theme, cols)
	}
	if len(dialog) > 0 {
		dialog = padDialogFrame(dialog)
	}

	// Slash-command autocomplete: popup above the status line, only
	// when the editor starts with "/" and no dialog is already open.
	// Feed extension-registered commands into the suggester first so
	// they show up in tab-complete + the popup alongside the built-ins.
	i.suggest.SetJailed(i.cfg.Sandbox.Locked())
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

	// Detect overlay close (any dialog or slash/file suggestion popup
	// just transitioned from open to closed). Force a full redraw so
	// the rows the overlay occupied are guaranteed to be repainted
	// from the chat below, instead of the diff path leaving stale
	// dialog content behind. Equivalent to the user pressing ctrl+l.
	overlayOpen := len(dialog) > 0 || len(suggest) > 0
	if i.rend != nil && i.prevOverlayOpen && !overlayOpen {
		// An overlay (dialog or slash/file popup) just closed, so the
		// bottom band shrinks. On terminals where we can drop
		// scrollback, a full Clear is the simplest way to guarantee
		// the vacated rows are repainted from the chat below.
		//
		// On VS Code's terminal closing a dialog leaves the stale
		// overlay rows in the retained scrollback (we can't drop them
		// with the quiet in-place diff). Run the same full Clear() that
		// Ctrl+L uses so the scrollback is purged and the conversation
		// is repainted clean, matching what the user expects after
		// dismissing a picker. Clear() is keepScrollback-aware and
		// emits \x1b[3J there.
		i.rend.Clear()
	}
	i.prevOverlayOpen = overlayOpen
	if len(suggest) > 0 {
		// One blank row above the popup so it doesn't sit flush
		// against the chat / welcome content above.
		suggest = append([]string{""}, suggest...)
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
	statusPosition := tui.NormalizeStatusPosition(i.cfg.TUIStatusPosition)
	workingPosition := tui.NormalizeWorkingPosition(i.cfg.TUIWorkingPosition)
	workingWithStatus := statusPosition == workingPosition
	statusBusyPrefix := ""
	if workingWithStatus {
		statusBusyPrefix = busyPrefix
	}
	statusLines := tui.StatusBar(tui.StatusBarParams{
		Theme:          i.cfg.Theme,
		Provider:       i.cfg.Provider,
		Model:          i.cfg.Model,
		Reasoning:      i.cfg.Reasoning,
		Busy:           i.busy,
		BusyPrefix:     statusBusyPrefix,
		CWD:            i.cfg.CWD,
		Locked:         i.cfg.Sandbox.Locked(),
		NoYolo:         i.cfg.NoYolo,
		Usage:          i.cumUsage,
		Subscription:   i.cfg.AuthMethod == "oauth",
		ContextUsed:    i.lastCtxInput,
		ContextMax:     ctxMax,
		AutoCompacting: i.autoCompacting,
		Telegram:       i.telegramBridge != nil && i.telegramBridge.Active(),
		Matrix:         i.matrixBridge != nil && i.matrixBridge.Active(),
		Cols:           cols,
	})
	inputStyle := tui.NormalizeInputStyle(i.cfg.TUIInputStyle)
	if inputStyle == tui.InputStyleLines || inputStyle == tui.InputStyleBlock {
		i.ed.Prompt = ""
	} else {
		i.ed.Prompt = i.cfg.Theme.AccentBar(i.cfg.Theme.Accent)
	}
	edLines, curR, curC := i.ed.Render(cols)
	var workingLines []string
	if busyPrefix != "" && !workingWithStatus {
		workingLines = []string{"  " + busyPrefix}
	}
	inputCursorOffset := 0
	inputCursorColOffset := 0
	switch inputStyle {
	case tui.InputStyleLines:
		edLines = tui.InputLines(i.cfg.Theme, edLines, cols)
		inputCursorOffset = 1
	case tui.InputStyleBlock:
		edLines = tui.InputBlock(i.cfg.Theme, edLines, cols)
		inputCursorOffset = 1
		inputCursorColOffset = 2
	}

	// "Sliding in" chips for messages the user typed while a turn is
	// in flight. Shown directly above the status bar so they're close
	// to the editor but don't push the chat around.
	var queue []string
	queued := append([]string(nil), i.queued...)
	if i.agent != nil {
		queued = append(queued, i.agent.PendingQueuedMessages()...)
	}
	if len(queued) > 0 {
		queue = append(queue, "")
		for _, q := range queued {
			label := i.cfg.Theme.FG256(i.cfg.Theme.Accent, "  sliding in: ")
			text := truncateLine(q, cols-17)
			queue = append(queue, label+i.cfg.Theme.FG256(i.cfg.Theme.Muted, text))
		}
		// Hint row, rendered in the same muted tone as the model
		// info on the status bar so it reads as ambient metadata
		// rather than a chip. Tells the user how to recover the
		// most recent queued message back into the editor.
		hint := "  Press " + slideBackChordHint() + " to slide back into input"
		queue = append(queue, i.cfg.Theme.FG256(i.cfg.Theme.Muted, hint))
	}

	// Bottom-sticky sections (always visible, never scroll). Each
	// non-empty subsection (dialog, suggest popup, sliding-in queue)
	// is preceded by one blank row so it has air above the chat
	// content. The status block and editor get their own dedicated
	// blanks so spacing stays consistent whether or not a dialog or
	// popup is showing.
	bottom := make([]string, 0, len(dialog)+len(suggest)+len(queue)+len(statusLines)+len(edLines)+9)
	inputStartRow := -1
	if len(dialog) > 0 {
		bottom = append(bottom, "")
	}
	bottom = append(bottom, dialog...)
	// The swarm dashboard owns the bottom of the screen while it's
	// active: it has its own inline editors for spawn (`n`) and
	// prompt (`p`), so the main input would be a confusing second
	// caret. The suggest popup, sliding-in queue, status block, and
	// main editor are all hidden underneath it. Keystrokes still
	// reach handleKey — it routes them to swarmDialog.HandleKey
	// before the editor ever sees them — so the only effect of this
	// branch is visual.
	if !i.swarmDialog.Active() {
		bottom = append(bottom, suggest...)
		bottom = append(bottom, queue...)
		lineInput := inputStyle == tui.InputStyleLines
		statusBelow := statusPosition == tui.StatusPositionBelowInput
		workingBelow := workingPosition == tui.WorkingPositionBelowInput

		var aboveInput []string
		if !statusBelow {
			aboveInput = append(aboveInput, statusLines...)
		}
		if !workingBelow {
			aboveInput = append(aboveInput, workingLines...)
		}
		var belowInput []string
		if workingBelow {
			belowInput = append(belowInput, workingLines...)
		}
		if statusBelow {
			belowInput = append(belowInput, statusLines...)
		}

		bottom = append(bottom, "")
		bottom = append(bottom, aboveInput...)
		needsGapAboveInput := len(aboveInput) > 0 && !lineInput
		if lineInput && statusBelow && !workingBelow && len(workingLines) > 0 {
			needsGapAboveInput = true
		}
		if needsGapAboveInput {
			bottom = append(bottom, "")
		}
		inputStartRow = len(bottom)
		bottom = append(bottom, edLines...)
		if len(belowInput) > 0 {
			if !lineInput {
				bottom = append(bottom, "")
			}
			bottom = append(bottom, belowInput...)
		}
	}

	_, rows := i.cfg.Terminal.Size()
	chatRows := rows - len(bottom)
	if chatRows < 1 {
		chatRows = 1
	}

	// Auto-follow guard: when the user has scrolled up (scrollOffset
	// > 0) and the agent appends new content below the viewport while
	// they're reading, compensate so the visible content stays
	// anchored. scrollOffset is measured from the bottom of `chat`,
	// so without compensation a growing buffer pushes the window
	// downward through the content and the lines the user was
	// reading scroll off the top.
	//
	// Skip compensation when the terminal width changed (a resize
	// reflows the whole buffer and the line-count delta no longer
	// corresponds to appended content) and when scrollOffset is 0
	// (the user is following the tail and wants new content to push
	// the view down as usual).
	//
	// The window the user sees starts at row
	//   start = len(chat) - scrollOffset - chatRows
	// so to keep `start` fixed across a redraw we must offset by both
	// the buffer growth (len delta) AND the viewport-height change
	// (chatRows delta, e.g. the status band or sliding-in queue
	// appearing during a turn). Compensating only for the len delta
	// let a shrinking chatRows pull the window toward the tail, which
	// read as the viewport jumping to the bottom whenever the agent
	// streamed text or a tool call grew the bottom band.
	if i.scrollOffset > 0 && i.prevChatCols == cols && i.prevChatLen > 0 {
		i.scrollOffset = anchorScrollOffset(i.scrollOffset,
			i.prevChatLen, len(chat), i.prevChatRows, chatRows)
	}
	i.prevChatLen = len(chat)
	i.prevChatCols = cols
	i.prevChatRows = chatRows

	// Apply scroll offset to the chat slice.
	maxOffset := len(chat) - chatRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	// Tail-render expansion: if the user has scrolled to (or above)
	// the top of the currently rendered tail and there are still
	// truncated messages above, widen view.TailLimit and rebuild.
	// The chat cache is keyed on tailLimit so the next cachedChatLocked
	// will re-issue Build instead of returning the stale slice. We
	// rebuild immediately so the same redraw shows the freshly-revealed
	// rows; otherwise the user would have to scroll again to see them.
	if i.scrollOffset >= maxOffset && i.view.TailLimit > 0 && i.view.TailLimit < len(i.view.Messages) {
		prevLen := len(chat)
		i.view.TailLimit += resumeTailExpandStep
		if i.view.TailLimit >= len(i.view.Messages) {
			i.view.TailLimit = 0 // unbounded
		}
		i.chatCacheValid = false
		chat = i.cachedChatLocked(cols)
		for len(chat) > 0 && strings.TrimSpace(chat[len(chat)-1]) == "" {
			chat = chat[:len(chat)-1]
		}
		// Newly-revealed rows are older messages prepended at the top.
		// scrollOffset counts rows from the bottom, so to keep the
		// viewport visually anchored on the same content the user was
		// looking at we shift it up by the number of rows added. Keep
		// the auto-follow baseline (prevChatLen) in sync with the
		// post-expansion length too, so the next render's follow guard
		// doesn't see this growth as a synthetic delta and yank the
		// viewport again.
		if grew := len(chat) - prevLen; grew > 0 {
			i.scrollOffset += grew
		}
		i.prevChatLen = len(chat)
		i.prevChatCols = cols
		maxOffset = len(chat) - chatRows
		if maxOffset < 0 {
			maxOffset = 0
		}
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
		rawStart := end - chatRows
		if rawStart < 0 {
			rawStart = 0
		}
		start := snapViewportStartToImageBlock(chat, rawStart)
		// If the snap pulled start upward (an image-block was atomic) while
		// the user is scrolling downward, the viewport would sit on the same
		// image until the user mashes down past every reserved row. Bump
		// scrollOffset past the image so one keypress always clears it.
		if start < rawStart && i.scrollOffset < i.prevScrollOffset {
			jump := rawStart - start
			i.scrollOffset -= jump
			if i.scrollOffset < 0 {
				i.scrollOffset = 0
			}
			end = len(chat) - i.scrollOffset
			rawStart = end - chatRows
			if rawStart < 0 {
				rawStart = 0
			}
			start = snapViewportStartToImageBlock(chat, rawStart)
		}
		end = start + chatRows
		if end > len(chat) {
			end = len(chat)
			start = end - chatRows
			if start < 0 {
				start = 0
			}
		}
		visibleChat = chat[start:end]
	}
	i.prevScrollOffset = i.scrollOffset
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

	// Default: the real terminal cursor sits on the main editor's
	// input position. In main-screen log mode cursor rows are relative
	// to the fixed bottom band, not the chat transcript.
	// dialogLead is 1 when the bottom region prepends a blank above
	// the dialog block (whenever a dialog is showing) so popup-side
	// cursor positions still land in the right cell.
	dialogLead := 0
	if len(dialog) > 0 {
		dialogLead = 1
	}
	cursorRow := inputStartRow + inputCursorOffset + curR
	cursorCol := curC + inputCursorColOffset
	if i.btwDialog.Active() {
		if r, c := i.btwDialog.CursorPos(cols); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
		}
	}
	if i.dialog.Active() {
		if r, c := i.dialog.CursorPos(cols); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
		}
	}
	if i.sessionDialog.Active() {
		if r, c := i.sessionDialog.CursorPos(); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
		}
	}
	if i.swarmDialog.Active() {
		if r, c := i.swarmDialog.CursorPos(cols); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
		} else {
			// Dashboard list / transcript view has no caret. Without
			// this branch the default cursorRow points at the
			// (hidden) main editor row, so the terminal would draw
			// a stray block somewhere in the chat region.
			cursorRow = -1
			cursorCol = 0
		}
	}
	if i.extPanel.Active() {
		cursorRow = -1
		cursorCol = 0
	}
	_ = visibleChat // maintained for legacy scroll state/indicators; DrawLog owns chat viewport.
	i.rend.DrawLog(chat, bottom, cursorRow, cursorCol)
}

func hasImageEscape(line string) bool {
	return strings.Contains(line, "\x1b]1337;File=") || strings.Contains(line, "\x1b_G")
}

// snapViewportStartToImageBlock treats inline images as atomic blocks for
// scrolling. Terminal image protocols draw from a single escape row into a
// separate graphics layer; the following blank rows are only zot's reserved
// footprint. If the viewport starts on one of those blank rows, there is no
// correct partial-image state to render. Snap back to the escape row instead
// so the image is either shown from its beginning or skipped entirely.
func snapViewportStartToImageBlock(chat []string, start int) int {
	if start <= 0 || start >= len(chat) {
		return start
	}
	if hasImageEscape(chat[start]) || !isBoxBlankLine(chat[start]) {
		return start
	}
	for k := start - 1; k >= 0; k-- {
		line := chat[k]
		if hasImageEscape(line) {
			return k
		}
		if !isBoxBlankLine(line) {
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
		if !hasImageEscape(line) {
			continue
		}
		// Image blocks render as: image escape, zero or more blank
		// reservation rows, then the muted "image - ..." info line,
		// then one trailing blank. If the info line isn't visible in
		// the current chat slice, the image would paint down into the
		// fixed status bar area. Suppress that image for this frame.
		//
		// When the image lives inside a tool box, the reservation rows
		// are wrapped in vertical box edges ("│  ...  │"); those rows
		// look non-blank under a naive whitespace check but are still
		// reservation rows for this scan, so treat them as blank.
		foundInfo := false
		for j := i + 1; j < len(out); j++ {
			if strings.Contains(out[j], "image - ") {
				foundInfo = true
				break
			}
			if !isBoxBlankLine(out[j]) {
				break
			}
		}
		if !foundInfo {
			out[i] = ""
		}
	}
	return out
}

// isBoxBlankLine reports whether line is visually empty after
// stripping ANSI escape sequences, surrounding whitespace, and the
// vertical box edges drawn by the tool-box renderer. Used by
// clipBottomClippedImages so an image's reservation rows still count
// as blank when those rows are wrapped in "│  ...  │" inside a tool box.
func isBoxBlankLine(line string) bool {
	stripped := stripANSIBytes(line)
	stripped = strings.TrimSpace(stripped)
	stripped = strings.Trim(stripped, "│")
	stripped = strings.TrimSpace(stripped)
	return stripped == ""
}

// stripANSIBytes removes ANSI CSI escape sequences (ESC '[' ... final
// byte) from s without pulling in the regexp package. Mirrors the
// internal helper in package tui; the duplicated copy avoids exporting
// it just for one caller.
func stripANSIBytes(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			end := i + 2
			for end < len(s) {
				c := s[end]
				end++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			i = end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
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
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	return string(runes[:n-3]) + "..."
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

// clearFileSuggestQuery strips the filter the user typed after the
// last "@", leaving the bare "@" so the picker stays open. Called when
// navigating between directory levels (Right/Left): the filter applied
// to the level the user was on, not the one being entered, so carrying
// it forward would wrongly hide the new directory's contents.
func (i *Interactive) clearFileSuggestQuery() {
	val := i.ed.Value()
	if idx := strings.LastIndex(val, "@"); idx >= 0 {
		i.ed.SetValue(val[:idx+1])
	}
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
			i.quickModelAssign = 0
			return false
		}
		act := i.modelDialog.HandleKey(k)
		if act.Close {
			i.quickModelAssign = 0
		}
		if act.Select {
			if i.quickModelAssign > 0 {
				i.applyQuickModelSelection(i.quickModelAssign, act.Provider, act.Model)
				i.quickModelAssign = 0
			} else {
				i.applyModelSelection(act.Provider, act.Model)
			}
		}
		return false
	}
	if i.rescueDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.rescueDialog.Close()
			i.invalidate()
			return false
		}
		act := i.rescueDialog.HandleKey(k)
		if act.Select {
			i.applyRescueSelection(act.Provider, act.Model, act.Prompt)
		}
		i.invalidate()
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
		// Always request a redraw after handling a key here: when esc
		// closes the picker, the overlay-close detection in the render
		// pass needs to run so the tall dialog rows get repainted from
		// the chat (otherwise VS Code's retained scrollback leaves a
		// duplicate frame on screen).
		i.invalidate()
		return false
	}
	if i.swarmDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.swarmDialog.Close()
			i.invalidate()
			return false
		}
		_, msg, errMsg := i.swarmDialog.HandleKey(k)
		if msg != "" || errMsg != "" {
			i.mu.Lock()
			i.statusOK = msg
			i.statusErr = errMsg
			i.mu.Unlock()
		}
		i.invalidate()
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
	if i.matrixDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.matrixDialog.Close()
			i.invalidate()
			return false
		}
		mact := i.matrixDialog.HandleKey(k)
		if mact.Select {
			i.doMatrix(mact.Action)
		}
		i.invalidate()
		return false
	}
	if i.settingsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.settingsDialog.Close()
			i.invalidate()
			return false
		}
		act := i.settingsDialog.HandleKey(k)
		if act.ModelShortcutSlot > 0 {
			i.openQuickModelPicker(act.ModelShortcutSlot)
		}
		if act.Toggle {
			i.applySettingChange(act)
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

	if slot := quickModelShortcutSlot(k); slot > 0 {
		i.applyQuickModelShortcut(slot)
		return false
	}

	// Global keys.
	switch k.Kind {
	case tui.KeyCtrlC:
		i.mu.Lock()
		loadingSession := i.sessionLoading
		i.mu.Unlock()
		if loadingSession {
			return true
		}
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
		ag := i.agent
		pending := 0
		if ag != nil {
			pending = ag.QueuedMessageCount()
		}
		hadInput := !i.ed.IsEmpty() || len(i.queued) > 0 || pending > 0
		if hadInput {
			i.ed.Clear()
			i.clipboardImages = nil
			i.suggest.Reset()
			if ag != nil {
				ag.DrainQueuedMessages()
			}
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
		// Only dismiss a parked shell-escape log on esc when nothing is
		// running; if a !command is in flight, esc must fall through to
		// the cancel path below instead of just hiding the (empty) block.
		hadShell := len(i.shellBlock) > 0 && !i.shellRunning
		if hadHelp {
			i.helpBlock = nil
		}
		if hadNotes {
			i.extNotes = nil
		}
		if hadShell {
			i.shellBlock = nil
		}
		i.mu.Unlock()
		if hadHelp || hadNotes || hadShell {
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
	case tui.KeyPasteClipboard:
		i.pasteClipboard()
		return false
	case tui.KeyCtrlO:
		// Toggle expansion of collapsed tool results. Affects every tool
		// call in the transcript — press again to re-collapse.
		// In main-screen scrollback mode this changes already-emitted
		// transcript rows, so do a full clear+replay instead of trying
		// to edit old scrollback in place.
		i.mu.Lock()
		i.view.ExpandAll = !i.view.ExpandAll
		if i.rend != nil {
			i.rend.Clear()
		}
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
		// Alt/Option+Up: pop the most recently queued ("sliding in")
		// message back into the editor so the user can edit and
		// resend it. Repeated presses keep peeling messages off the
		// tail of the queue; each press *replaces* the editor
		// contents (we don't append/push). When the queue is empty
		// the keypress falls through to the normal scroll behavior.
		if k.Alt {
			i.mu.Lock()
			var text string
			if i.agent != nil {
				text, _ = i.agent.PopQueuedMessage()
			}
			if text == "" {
				if n := len(i.queued); n > 0 {
					text = i.queued[n-1]
					i.queued = i.queued[:n-1]
				}
			}
			i.mu.Unlock()
			if text != "" {
				i.ed.SetValue(text)
				i.inputHistoryIndex = -1
				i.invalidate()
				return false
			}
		}
		// In multi-line / wrapped input, Up first moves inside the editor.
		// At the editor's top edge, plain Up can browse input history when
		// history browsing is safe/active; otherwise it falls back to chat
		// scrolling, preserving the old single-line scroll behavior.
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
			if i.ed.MoveVertical(-1) {
				i.invalidate()
				return false
			}
			if !k.Alt && i.handleInputHistoryKey(k) {
				return false
			}
			i.scrollBy(+3)
			return false
		}
	case tui.KeyDown:
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
			if i.ed.MoveVertical(+1) {
				i.invalidate()
				return false
			}
			if !k.Alt && i.handleInputHistoryKey(k) {
				return false
			}
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

	if k.Kind == tui.KeyEnter && (k.Alt || k.Shift) {
		i.ed.HandleKey(k)
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
		case tui.KeyPageUp:
			i.suggest.PageUp()
			return false
		case tui.KeyPageDown:
			i.suggest.PageDown()
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
			// Open selected directory. The filter the user typed picked
			// that directory at the current level; once we descend it no
			// longer applies to the directory's contents, so clear it.
			// Otherwise typing "@eda" then right would re-filter inside
			// eda/ by "eda" and show nothing.
			if i.fileSuggest.Right() {
				i.clearFileSuggestQuery()
			}
			return false
		case tui.KeyLeft:
			// Go back to parent directory. Clear the filter for the same
			// reason as Right: it was scoped to the level we just left.
			if i.fileSuggest.Left() {
				i.clearFileSuggestQuery()
			}
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

	// Tab-complete a path token in the editor when no popup is open.
	// Recognises tokens that look like paths (start with ~, /, ./, ../
	// or contain a slash); shell-style completion expands ~, lists the
	// parent dir, and completes the basename to the longest common
	// prefix. Single match: full replace and trailing / for dirs.
	// No match: no-op. Plain bare words (no slash, no tilde) fall
	// through so Tab keeps its current no-op behaviour outside paths.
	if k.Kind == tui.KeyTab && !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
		if i.tryPathTabComplete() {
			return false
		}
	}

	if i.inputHistoryIndex >= 0 && k.Kind != tui.KeyUp && k.Kind != tui.KeyDown {
		i.inputHistoryIndex = -1
	}

	if k.Kind == tui.KeyEsc {
		i.clipboardImages = nil
	}
	if submit := i.ed.HandleKey(k); submit {
		// SubmitValue() expands any [pasted text #N +L lines]
		// placeholders back into their bodies; the raw Value()
		// is only what the user sees on screen.
		text := strings.TrimRight(i.ed.SubmitValue(), "\n")
		// Expand [file:name] and [dir:name/] chips to full paths.
		text = expandFileChips(text, i.cfg.CWD)
		text, images := preparePromptWithClipboardImages(text, i.clipboardImages)
		if text == "" && len(images) == 0 {
			return false
		}
		i.clipboardImages = nil
		i.ed.Clear()
		i.inputHistoryIndex = -1
		i.suggest.Reset()
		i.fileSuggest.Reset()

		if cmd, ok := shellEscapeCommand(text); ok {
			i.startShellEscape(ctx, cmd)
			return false
		}

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
		if i.matrixBridge != nil && i.matrixBridge.Active() {
			go i.matrixBridge.OnUserTyped(text)
		}
		// If a turn is already in flight, queue this prompt inside the
		// agent loop so it is delivered at the next safe model-call
		// boundary instead of waiting for the whole run to finish.
		i.mu.Lock()
		busy := i.busy
		ag := i.agent
		i.mu.Unlock()
		if busy {
			if len(images) > 0 {
				i.mu.Lock()
				i.statusErr = "can't queue clipboard images while a turn is running; wait for the current turn to finish"
				i.mu.Unlock()
				i.invalidate()
				return false
			}
			if ag != nil {
				ag.QueueMessage(text)
			} else {
				i.mu.Lock()
				i.queued = append(i.queued, text)
				i.mu.Unlock()
			}
			i.invalidate()
			return false
		}
		i.startTurnWithImages(ctx, text, images)
	}
	return false
}

func (i *Interactive) pasteClipboard() {
	_, data, ok, err := tui.ReadClipboardImagePNG()
	if err != nil {
		i.mu.Lock()
		i.statusErr = "clipboard paste failed: " + err.Error()
		i.statusOK = ""
		i.mu.Unlock()
		return
	}
	if ok {
		i.mu.Lock()
		marker := fmt.Sprintf("[clipboard image #%d]", len(i.clipboardImages)+1)
		i.clipboardImages = append(i.clipboardImages, clipboardImageAttachment{
			Marker: marker,
			Image:  provider.ImageBlock{MimeType: "image/png", Data: data},
		})
		i.ed.Insert(marker + " ")
		i.statusOK = ""
		i.statusErr = ""
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	i.statusErr = "clipboard does not contain an image"
	i.statusOK = ""
	i.mu.Unlock()
}

func (i *Interactive) handleInputHistoryKey(k tui.Key) bool {
	if k.Kind != tui.KeyUp && k.Kind != tui.KeyDown {
		return false
	}
	// Do not steal normal vertical cursor movement. History browsing can only
	// start from an empty editor; once active, Up/Down keep walking
	// the ring so repeated presses work even though the editor now
	// contains the selected historical prompt.
	if i.inputHistoryIndex < 0 && !i.ed.IsEmpty() {
		return false
	}
	hist := i.inputHistory()
	if len(hist) == 0 {
		return false
	}

	if i.inputHistoryIndex < 0 {
		// Start just after the newest item so Up lands on the most
		// recent user prompt. A lone Down from an empty editor is not
		// history navigation; let the caller fall through to normal UI
		// behavior instead.
		if k.Kind != tui.KeyUp {
			return false
		}
		i.inputHistoryIndex = len(hist)
	}

	switch k.Kind {
	case tui.KeyUp:
		if i.inputHistoryIndex > 0 {
			i.inputHistoryIndex--
		}
	case tui.KeyDown:
		if i.inputHistoryIndex < len(hist) {
			i.inputHistoryIndex++
		}
	}

	if i.inputHistoryIndex >= len(hist) {
		i.ed.Clear()
	} else {
		i.ed.SetValue(hist[i.inputHistoryIndex])
	}
	return true
}

func (i *Interactive) inputHistory() []string {
	if i.agent == nil {
		return nil
	}
	msgs := i.agent.Messages()
	hist := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != provider.RoleUser || isHiddenTranscriptMessage(m) {
			continue
		}
		text := userMessageText(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		hist = append(hist, text)
	}
	return hist
}

func userMessageText(m provider.Message) string {
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

// ClearNotes removes every note line owned by extName from the
// bottom-sticky ext-notes block. Extensions use this to retract a
// transient status line (e.g. an approval prompt) once it no longer
// applies, instead of leaving it stacked forever. Notes from other
// extensions and internal notes (auto-compact) are left untouched.
func (i *Interactive) ClearNotes(extName string) {
	marker := "[" + extName + "] "
	i.mu.Lock()
	if len(i.extNotes) == 0 {
		i.mu.Unlock()
		return
	}
	kept := i.extNotes[:0:0]
	changed := false
	for _, line := range i.extNotes {
		if strings.Contains(line, marker) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if changed {
		i.extNotes = kept
	}
	i.mu.Unlock()
	if changed {
		i.invalidate()
	}
}

// Submit feeds text through the agent loop as if the user had typed it.
func (i *Interactive) Submit(text string) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	i.startTurn(i.runCtx, text)
}

// ApplyChangedCWD is called by the host after a successful /cd hook.
// The host has already rebuilt the agent and opened a fresh session
// in the new cwd; this method swaps the fresh agent into the running
// TUI, updates the displayed cwd, clears the transcript display
// caches, and points the file picker at the new directory.
//
// The fresh agent's transcript is empty (new session) so the chat
// view starts blank, matching what relaunching `zot --cwd <path>`
// would show. Cost meters reset.
func (i *Interactive) ApplyChangedCWD(ag *core.Agent, provider, model, cwd string) {
	i.mu.Lock()
	i.agent = ag
	i.cfg.CWD = cwd
	// Re-report the working directory to the terminal so "new tab here"
	// tracks the /cd change (OSC 7, see issue #38).
	if i.cfg.Terminal != nil {
		if seq := tui.ReportCWD(cwd); seq != "" {
			_, _ = i.cfg.Terminal.Write([]byte(seq))
		}
	}
	i.cfg.Provider = provider
	i.cfg.Model = model
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.helpBlock = nil
	i.parkedTurn = 0
	i.statusErr = ""
	i.mu.Unlock()
	i.fileSuggest.Reset()
	i.fileSuggest.SetCWD(cwd)
	i.invalidate()
}

// SubmitSlash runs text as a slash command in the TUI as if the user
// had typed it. text must start with '/' — callers that hand it
// plain prose silently get a no-op so a misbehaving extension can't
// run a stray prompt through this path. Read-only commands run in
// place; commands that would mutate the transcript or replace the
// agent cancel the active turn first via the same path the editor
// uses for typed slash commands.
func (i *Interactive) SubmitSlash(text string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return
	}
	head := text
	if idx := strings.IndexAny(text, " \t"); idx >= 0 {
		head = text[:idx]
	}
	if slashCancelsTurn(head) {
		i.cancelAndWaitForIdle()
	}
	i.runSlash(i.runCtx, text)
	i.invalidate()
}

// SubmitOrQueue runs text immediately if the agent is idle, or
// appends it to the pending queue if a turn is already in flight.
// Used by the telegram bridge (and by the editor submit path) so
// both input sources share the same "queue behind an active turn"
// semantics. Images are ignored for now — only the text prompt is
// forwarded — because the queued-prompt path is text-only; a
// follow-up can expand the queue entry to carry images.
func (i *Interactive) SubmitOrQueue(text string, images []provider.ImageBlock) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	i.mu.Lock()
	if i.agent == nil {
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.busy {
		// Queue text only; images are dropped for queued prompts.
		ag := i.agent
		i.mu.Unlock()
		if ag != nil {
			ag.QueueMessage(text)
		} else {
			i.mu.Lock()
			i.queued = append(i.queued, text)
			i.mu.Unlock()
		}
		i.invalidate()
		return
	}
	i.mu.Unlock()
	i.startTurnWithImages(i.runCtx, text, images)
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
		i.confirmDialog.CancelAll("turn cancelled")
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

func effectiveImageProtocol(override *bool) tui.ImageProtocol {
	detected := tui.DetectImageProtocol()
	if override == nil {
		return detected
	}
	if !*override {
		return tui.ImageProtocolNone
	}
	return detected
}

func imageProtocolName(p tui.ImageProtocol) string {
	switch p {
	case tui.ImageProtocolKitty:
		return "kitty/ghostty"
	case tui.ImageProtocolITerm2:
		return "iTerm2"
	default:
		return "none"
	}
}

func onOff(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}

func (i *Interactive) applyInputCursorColor() {
	if i == nil || i.cfg.Terminal == nil {
		return
	}
	_, _ = i.cfg.Terminal.Write([]byte(tui.CursorColor256(15) + tui.CursorShapeBlock()))
}

func (i *Interactive) openSettingsDialog() {
	detected := tui.DetectImageProtocol()
	imgEnabled := detected != tui.ImageProtocolNone
	if i.cfg.InlineImagesEnabled != nil {
		imgEnabled = *i.cfg.InlineImagesEnabled
	}
	imgDisabled := detected == tui.ImageProtocolNone
	imgHint := ""
	if imgDisabled {
		imgEnabled = false
		imgHint = "this terminal does not support inline images"
	} else {
		imgHint = "terminal supports " + imageProtocolName(detected)
	}

	autoSwarm := false
	if i.cfg.AutoSwarmEnabled != nil {
		autoSwarm = *i.cfg.AutoSwarmEnabled
	}
	autoSwarmDisabled := i.cfg.Swarm == nil
	autoSwarmHint := ""
	if autoSwarmDisabled {
		autoSwarm = false
		autoSwarmHint = "swarm supervisor not available in this mode"
	}

	recursiveFiles := i.cfg.RecursiveFileSuggest != nil && *i.cfg.RecursiveFileSuggest
	respectGitignore := i.cfg.RespectGitignore == nil || *i.cfg.RespectGitignore
	compactMode := i.compactModeEnabled()
	inputStyle := tui.NormalizeInputStyle(i.cfg.TUIInputStyle)
	statusPosition := tui.NormalizeStatusPosition(i.cfg.TUIStatusPosition)
	workingPosition := tui.NormalizeWorkingPosition(i.cfg.TUIWorkingPosition)
	quickItems := i.quickModelSettingItems()

	reasoningOptions := []settingsOption{
		{value: "", label: "off", desc: "no reasoning"},
		{value: "minimum", label: "minimum", desc: "very brief (~1k tokens)"},
		{value: "low", label: "low", desc: "light (~2k tokens)"},
		{value: "medium", label: "medium", desc: "moderate (~8k tokens)"},
		{value: "high", label: "high", desc: "deep (~16k tokens)"},
		{value: "maximum", label: "maximum", desc: "highest (~32k tokens)"},
	}
	reasoning := provider.NormalizeReasoning(i.cfg.Reasoning)
	reasoningChoice := 0
	for idx, opt := range reasoningOptions {
		if opt.value == reasoning {
			reasoningChoice = idx
			break
		}
	}
	reasoningHint := ""
	if m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil && !m.Reasoning {
		reasoningHint = "current model does not support thinking"
	}

	themeName := i.cfg.ThemeName
	if themeName == "" {
		themeName = "auto"
	}
	if themeName != "auto" && !tui.ThemeExists(i.cfg.ZotHome, themeName) {
		themeName = "auto"
		i.cfg.ThemeName = ""
		if i.cfg.SettingsStore != nil {
			_ = i.cfg.SettingsStore.SetTheme("auto")
		}
		i.applyThemeNow("auto")
	}
	themeOptions := []settingsOption{}
	themeChoice := 0
	availableThemes := tui.AvailableThemes(i.cfg.ZotHome)
	if i.cfg.ExtensionThemes != nil {
		availableThemes = append(availableThemes, i.cfg.ExtensionThemes()...)
	}
	for idx, opt := range availableThemes {
		themeOptions = append(themeOptions, settingsOption{value: opt.Value, label: opt.Label, desc: opt.Description})
		if opt.Value == themeName {
			themeChoice = idx
		}
	}

	inputStyleOptions := []settingsOption{
		{value: tui.InputStylePlain, label: "plain", desc: "render the input as the normal prompt line"},
		{value: tui.InputStyleLines, label: "lines", desc: "draw separator lines above and below the input"},
		{value: tui.InputStyleBlock, label: "block", desc: "render the input as a user-bubble-style block"},
	}
	inputStyleChoice := 0
	for idx, opt := range inputStyleOptions {
		if opt.value == inputStyle {
			inputStyleChoice = idx
			break
		}
	}
	statusPositionOptions := []settingsOption{
		{value: tui.StatusPositionAboveInput, label: "above input", desc: "show model, usage, and working directory above the input"},
		{value: tui.StatusPositionBelowInput, label: "below input", desc: "show model, usage, and working directory below the input"},
	}
	statusPositionChoice := 0
	for idx, opt := range statusPositionOptions {
		if opt.value == statusPosition {
			statusPositionChoice = idx
			break
		}
	}

	workingPositionOptions := []settingsOption{
		{value: tui.WorkingPositionAboveInput, label: "above input", desc: "show the working spinner above the input"},
		{value: tui.WorkingPositionBelowInput, label: "below input", desc: "show the working spinner below the input"},
	}
	workingPositionChoice := 0
	for idx, opt := range workingPositionOptions {
		if opt.value == workingPosition {
			workingPositionChoice = idx
			break
		}
	}

	items := []settingsItem{
		{
			key:      "inline_images_enabled",
			label:    "render images when supported",
			desc:     "draw screenshots inline instead of showing a text placeholder",
			value:    imgEnabled,
			disabled: imgDisabled,
			hint:     imgHint,
		},
		{
			key:      "auto_swarm_enabled",
			label:    "auto-swarm",
			desc:     "let the agent spawn background sub-agents in parallel via the swarm_spawn tool",
			value:    autoSwarm,
			disabled: autoSwarmDisabled,
			hint:     autoSwarmHint,
		},
		{
			key:   "recursive_file_suggest",
			label: "recursive @-file search",
			desc:  "fuzzy-search the whole project tree when picking files with @ instead of browsing one directory at a time",
			value: recursiveFiles,
		},
		{
			key:   "respect_gitignore",
			label: "hide gitignored files in @-picker",
			desc:  "skip files and directories matched by the project's root .gitignore (and .git) when picking files with @",
			value: respectGitignore,
		},
		{
			key:   "compact_mode",
			label: "compact transcript rendering",
			desc:  "reduce visual chrome by rendering tool calls without boxes and sent messages without padded bubbles",
			value: compactMode,
		},
		{
			key:   "tui_settings",
			label: "tui settings",
			desc:  "choose input chrome and where status information appears",
			children: []settingsItem{
				{
					key:     "tui_input_style",
					label:   "input style",
					desc:    "choose between the plain prompt, lines, and a block input area",
					options: inputStyleOptions,
					choice:  inputStyleChoice,
				},
				{
					key:     "tui_status_position",
					label:   "status position",
					desc:    "place model, usage, and working directory above or below the input",
					options: statusPositionOptions,
					choice:  statusPositionChoice,
				},
				{
					key:     "tui_working_position",
					label:   "working spinner position",
					desc:    "place the working spinner above or below the input",
					options: workingPositionOptions,
					choice:  workingPositionChoice,
				},
			},
		},
		{
			key:     "reasoning",
			label:   "thinking level",
			desc:    "reasoning depth for thinking-capable models",
			options: reasoningOptions,
			choice:  reasoningChoice,
			hint:    reasoningHint,
		},
		{
			key:     "theme",
			label:   "color theme",
			desc:    "choose a theme from $ZOT_HOME/themes or a loaded extension",
			options: themeOptions,
			choice:  themeChoice,
		},
	}
	if len(quickItems) > 0 {
		items = append(items, settingsItem{
			key:      "quick_models",
			label:    "model shortcuts",
			desc:     "configure " + quickModelShortcutPrefix() + "+1 through " + quickModelShortcutPrefix() + "+9 quick model switches",
			children: quickItems,
		})
	}
	i.settingsDialog.Open(items)
}

func (i *Interactive) applySettingChange(act settingsAction) {
	switch {
	case strings.HasPrefix(act.Key, "quick_model_"):
		i.applyQuickModelSetting(act.Key, act.StringValue)
	case act.Key == "reasoning":
		i.applyReasoningSetting(act.StringValue)
	case act.Key == "theme":
		i.applyThemeSetting(act.StringValue)
	case act.Key == "tui_input_style":
		i.applyTUIInputStyleSetting(act.StringValue)
	case act.Key == "tui_status_position":
		i.applyTUIStatusPositionSetting(act.StringValue)
	case act.Key == "tui_working_position":
		i.applyTUIWorkingPositionSetting(act.StringValue)
	default:
		i.applySettingToggle(act.Key, act.Value)
	}
}

func (i *Interactive) quickModelSettingItems() []settingsItem {
	if len(i.cfg.QuickModelShortcuts) < 9 {
		next := make([]QuickModelShortcut, 9)
		copy(next, i.cfg.QuickModelShortcuts)
		i.cfg.QuickModelShortcuts = next
	}
	items := make([]settingsItem, 0, 9)
	for slot := 1; slot <= 9; slot++ {
		items = append(items, i.quickModelSettingItem(slot))
	}
	return items
}

func (i *Interactive) quickModelSettingItem(slot int) settingsItem {
	current := QuickModelShortcut{}
	if slot >= 1 && len(i.cfg.QuickModelShortcuts) >= slot {
		current = i.cfg.QuickModelShortcuts[slot-1]
	}
	hint := "not assigned"
	if current.Provider != "" && current.Model != "" {
		hint = current.Provider + " / " + current.Model
	}
	return settingsItem{
		key:    "quick_model_" + strconv.Itoa(slot),
		label:  "model " + strconv.Itoa(slot),
		desc:   quickModelShortcutLabel(slot) + " switches to this model. Enter opens the /model selector, Backspace clears.",
		picker: true,
		hint:   hint,
	}
}

func (i *Interactive) compactModeEnabled() bool {
	return i.cfg.CompactMode != nil && *i.cfg.CompactMode
}

func quickModelShortcutSlot(k tui.Key) int {
	if k.Kind != tui.KeyRune || k.Rune < '1' || k.Rune > '9' {
		return 0
	}
	if runtime.GOOS == "darwin" {
		if !k.Super && !k.Ctrl {
			return 0
		}
	} else if !k.Ctrl {
		return 0
	}
	return int(k.Rune - '0')
}

func quickModelShortcutPrefix() string {
	return "Ctrl"
}

func quickModelShortcutLabel(slot int) string {
	return quickModelShortcutPrefix() + "+" + strconv.Itoa(slot)
}

func (i *Interactive) openQuickModelPicker(slot int) {
	if slot < 1 || slot > 9 {
		return
	}
	i.quickModelAssign = slot
	current := i.cfg.Model
	if len(i.cfg.QuickModelShortcuts) >= slot && i.cfg.QuickModelShortcuts[slot-1].Model != "" {
		current = i.cfg.QuickModelShortcuts[slot-1].Model
	}
	var loggedIn []string
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	i.modelDialog.Open(current, loggedIn)
}

func (i *Interactive) applyQuickModelSelection(slot int, providerName, model string) {
	i.setQuickModelShortcut(slot, providerName, model)
}

func (i *Interactive) applyQuickModelShortcut(slot int) {
	if slot < 1 || slot > 9 {
		return
	}
	if i.busy {
		i.mu.Lock()
		i.statusErr = "cannot switch model while a turn is running"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if len(i.cfg.QuickModelShortcuts) < slot {
		i.mu.Lock()
		i.statusErr = quickModelShortcutLabel(slot) + " is not assigned"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	shortcut := i.cfg.QuickModelShortcuts[slot-1]
	if shortcut.Provider == "" || shortcut.Model == "" {
		i.mu.Lock()
		i.statusErr = quickModelShortcutLabel(slot) + " is not assigned"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.swapModel(shortcut.Provider, shortcut.Model, i.cfg.BuildAgentFor, false)
	i.invalidate()
}

func (i *Interactive) applyQuickModelSetting(key, value string) {
	slotText := strings.TrimPrefix(key, "quick_model_")
	slot, err := strconv.Atoi(slotText)
	if err != nil || slot < 1 || slot > 9 {
		return
	}
	providerName, model := "", ""
	if value != "" {
		parts := strings.SplitN(value, "\t", 2)
		if len(parts) == 2 {
			providerName, model = parts[0], parts[1]
		}
	}
	i.setQuickModelShortcut(slot, providerName, model)
}

func (i *Interactive) setQuickModelShortcut(slot int, providerName, model string) {
	if len(i.cfg.QuickModelShortcuts) < slot {
		next := make([]QuickModelShortcut, slot)
		copy(next, i.cfg.QuickModelShortcuts)
		i.cfg.QuickModelShortcuts = next
	}
	i.cfg.QuickModelShortcuts[slot-1] = QuickModelShortcut{Provider: providerName, Model: model}
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetQuickModelShortcut(slot, providerName, model); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	if model == "" {
		i.statusOK = quickModelShortcutLabel(slot) + " cleared"
	} else {
		i.statusOK = quickModelShortcutLabel(slot) + " set to " + providerName + " / " + model
	}
	i.statusErr = ""
	i.mu.Unlock()
	i.refreshQuickModelSettingsItem(slot)
	i.invalidate()
}

func (i *Interactive) refreshQuickModelSettingsItem(slot int) {
	if i.settingsDialog == nil || !i.settingsDialog.Active() || len(i.settingsDialog.items) == 0 {
		return
	}
	key := "quick_model_" + strconv.Itoa(slot)
	for idx, it := range i.settingsDialog.items {
		if it.key == key {
			i.settingsDialog.items[idx] = i.quickModelSettingItem(slot)
			return
		}
	}
}

func (i *Interactive) applySettingToggle(key string, value bool) {
	// Every setting toggle forces a full repaint at the end — same
	// effect as the user pressing Ctrl+L — so any per-setting visual
	// change (image rendering, status copy, future toggles) lands
	// immediately instead of waiting for the next diff frame.
	defer func() {
		if i.rend != nil {
			i.rend.Clear()
		}
		i.invalidate()
	}()
	switch key {
	case "inline_images_enabled":
		val := value
		i.cfg.InlineImagesEnabled = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetInlineImages(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.ImageProto = effectiveImageProtocol(i.cfg.InlineImagesEnabled)
		i.view.InvalidateRenderCache()
		i.statusOK = "inline image rendering " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "auto_swarm_enabled":
		val := value
		i.cfg.AutoSwarmEnabled = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetAutoSwarm(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		// Add/remove the swarm_spawn tool on the live agent so the
		// model's tools[] list reflects the toggle on the next turn.
		// Without this the tool stays advertised after a disable and
		// the model keeps trying to call it.
		i.applyAutoSwarmTool(value)
		// Also swap the system-prompt addendum in/out so the model
		// knows to use the tool proactively (or stops referencing it
		// after a disable).
		i.applyAutoSwarmSystemPrompt(value)
		i.mu.Lock()
		i.statusOK = "auto-swarm " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "recursive_file_suggest":
		val := value
		i.cfg.RecursiveFileSuggest = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetRecursiveFileSuggest(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		// Flip the live picker so the next @ reflects the new mode
		// without restarting zot. SetRecursive drops its cache.
		i.fileSuggest.SetRecursive(value)
		i.mu.Lock()
		i.statusOK = "recursive @-file search " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "respect_gitignore":
		val := value
		i.cfg.RespectGitignore = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetRespectGitignore(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.fileSuggest.SetRespectGitignore(value)
		i.mu.Lock()
		i.statusOK = "hide gitignored files in @-picker " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "compact_mode":
		val := value
		i.cfg.CompactMode = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetCompactMode(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.CompactMode = value
		i.view.InvalidateRenderCache()
		i.statusOK = "compact transcript rendering " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	}
}

func (i *Interactive) applyTUIInputStyleSetting(style string) {
	defer func() {
		if i.rend != nil {
			i.rend.Clear()
		}
		i.invalidate()
	}()
	style = tui.NormalizeInputStyle(style)
	i.cfg.TUIInputStyle = style
	i.applyInputCursorColor()
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIInputStyle(style); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	i.statusOK = "input style " + style
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) applyTUIStatusPositionSetting(position string) {
	defer func() {
		if i.rend != nil {
			i.rend.Clear()
		}
		i.invalidate()
	}()
	position = tui.NormalizeStatusPosition(position)
	i.cfg.TUIStatusPosition = position
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIStatusPosition(position); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	label := "above input"
	if position == tui.StatusPositionBelowInput {
		label = "below input"
	}
	i.statusOK = "status position " + label
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) applyTUIWorkingPositionSetting(position string) {
	defer func() {
		if i.rend != nil {
			i.rend.Clear()
		}
		i.invalidate()
	}()
	position = tui.NormalizeWorkingPosition(position)
	i.cfg.TUIWorkingPosition = position
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIWorkingPosition(position); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	label := "above input"
	if position == tui.WorkingPositionBelowInput {
		label = "below input"
	}
	i.statusOK = "working spinner position " + label
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) applyThemeSetting(name string) {
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTheme(name); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.cfg.ThemeName = name
	if name == "auto" {
		i.cfg.ThemeName = ""
	}
	i.applyThemeNow(name)
}

func (i *Interactive) applyThemeNow(name string) {
	if name == "" {
		name = "auto"
	}
	detected := tui.Dark
	if tui.IsLightTheme(i.cfg.Theme) {
		detected = tui.Light
	}
	th, applied, err := tui.LoadThemeFromHome(i.cfg.ZotHome, name, detected)
	if err != nil {
		if i.cfg.SettingsStore != nil {
			_ = i.cfg.SettingsStore.SetTheme("auto")
		}
		i.cfg.ThemeName = ""
		th, applied, _ = tui.LoadThemeFromHome(i.cfg.ZotHome, "auto", detected)
		i.mu.Lock()
		i.statusErr = "theme missing; reset to default"
		i.mu.Unlock()
	} else {
		i.mu.Lock()
		label := applied
		if label == "" {
			label = "auto"
		}
		i.statusOK = "theme " + label
		i.statusErr = ""
		i.mu.Unlock()
	}
	i.cfg.Theme = th
	i.view.Theme = th
	i.view.InvalidateRenderCache()
	i.ed.Prompt = th.AccentBar(th.Accent)
	i.applyInputCursorColor()
	i.spin.Configure(th)
	if i.rend != nil {
		i.rend.SetTheme(th)
		i.rend.Clear()
	}
	i.invalidate()
}

func (i *Interactive) applyReasoningSetting(level string) {
	defer func() {
		if i.rend != nil {
			i.rend.Clear()
		}
		i.invalidate()
	}()
	level = provider.NormalizeReasoning(level)
	i.cfg.Reasoning = level
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetReasoning(level); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	if i.agent != nil {
		i.agent.Reasoning = level
	}
	label := level
	if label == "" {
		label = "off"
	}
	i.statusOK = "thinking level " + label
	i.statusErr = ""
	i.mu.Unlock()
}

// buildStudyPrompt returns the canned prompt the /study command
// submits to the agent.
//
// With no argument, /study targets the current directory — the
// historical behaviour. With an argument, /study targets that path
// instead; either a directory ("read every file in here") or a
// single file ("read this file"). The argument can be:
//
//   - a relative path (resolved against cwd)
//   - an absolute path
//   - an @-picker chip, which has already been expanded to an
//     absolute path by expandFileChips before runSlash sees it
//
// The path is stat'd to pick the right wording ("directory" vs
// "file"). If the path doesn't exist, we still build a sensible
// prompt rather than erroring — the agent will surface the
// missing-file failure itself when it tries to read it, which is
// more useful than a refusal here.
func buildStudyPrompt(arg, cwd string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "Read and understand everything in the current directory."
	}
	abs := arg
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	display := arg
	if rel, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
		display = rel
	}
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		return "Read and understand the file " + display + "."
	}
	return "Read and understand everything in the directory " + display + "."
}

// tryPathTabCompleteEditor looks at ed's current value, finds the
// path-like token immediately before the cursor (the cursor is always
// at the end of the buffer after a keystroke, so "before the cursor"
// is the trailing non-whitespace run), and rewrites it to its shell-
// style completion against the filesystem.
//
// Returns true when it consumed the Tab keystroke (token recognised,
// completion attempted — even if no candidates matched, the keystroke
// is still consumed so it doesn't insert a literal tab character).
// Returns false when the token doesn't look like a path; callers then
// let Tab fall through to its normal no-op.
//
// Recognised path shapes:
//   - ~ or ~/foo                  expanded via os.UserHomeDir()
//   - /abs/path or /abs/path/foo  absolute
//   - ./foo, ../foo, foo/bar      relative to cwd
//
// A bare word like "hello" is not treated as a path so plain text
// keeps Tab as a literal no-op.
//
// Free function (not a method) so the same logic runs against the
// editor instances owned by btwDialog and swarmDialog without each
// dialog needing its own copy.
func tryPathTabCompleteEditor(ed *tui.Editor, cwd string) bool {
	if ed == nil {
		return false
	}
	val := ed.Value()
	// Find the trailing run of non-whitespace.
	start := len(val)
	for start > 0 {
		r := val[start-1]
		if r == ' ' || r == '\t' || r == '\n' {
			break
		}
		start--
	}
	token := val[start:]
	if token == "" {
		return false
	}
	if !looksLikePathToken(token) {
		return false
	}

	// Resolve the absolute parent directory + base prefix to match.
	parentAbs, basePrefix, displayParent, ok := resolvePathTabToken(token, cwd)
	if !ok {
		return true
	}
	entries, err := os.ReadDir(parentAbs)
	if err != nil {
		return true
	}
	var names []string
	var isDir []bool
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, basePrefix) {
			continue
		}
		// Hide dotfiles unless the user explicitly typed a leading dot,
		// mirroring bash's default behaviour.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(basePrefix, ".") {
			continue
		}
		names = append(names, name)
		isDir = append(isDir, e.IsDir())
	}
	if len(names) == 0 {
		return true
	}

	var completed string
	var completedIsDir bool
	if len(names) == 1 {
		completed = names[0]
		completedIsDir = isDir[0]
	} else {
		completed = longestCommonPrefix(names)
		if completed == basePrefix {
			// Already at the deepest unambiguous prefix; nothing to add.
			return true
		}
	}

	// Build the replacement token in the same display form the user
	// typed (preserve ~ vs absolute vs relative).
	newToken := displayParent + completed
	if len(names) == 1 && completedIsDir {
		newToken += "/"
	}

	ed.SetValue(val[:start] + newToken)
	return true
}

// tryPathTabComplete is the Interactive-bound convenience wrapper.
// It calls the free helper against the main editor and invalidates
// the frame on a successful rewrite.
func (i *Interactive) tryPathTabComplete() bool {
	if tryPathTabCompleteEditor(i.ed, i.cfg.CWD) {
		i.invalidate()
		return true
	}
	return false
}

// looksLikePathToken reports whether tok is shaped like a filesystem
// path. Paths must either start with ~, /, ./, ../ or contain a /.
// Plain words are excluded so Tab on "hello" stays a no-op.
func looksLikePathToken(tok string) bool {
	if tok == "" {
		return false
	}
	if tok[0] == '~' || tok[0] == '/' {
		return true
	}
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") {
		return true
	}
	return strings.Contains(tok, "/")
}

// resolvePathTabToken splits tok into (absolute parent dir, basename
// prefix to match, display-form parent the user typed). ok is false
// when the parent dir can't be resolved (e.g. ~ with no $HOME).
func resolvePathTabToken(tok, cwd string) (parentAbs, basePrefix, displayParent string, ok bool) {
	// Detect ~ expansion.
	expanded := tok
	homePrefix := ""
	if tok == "~" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", "", "", false
		}
		// "~" alone: complete in $HOME. parent = home, base = "".
		return home, "", "~/", true
	}
	if strings.HasPrefix(tok, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", "", "", false
		}
		expanded = home + tok[1:]
		homePrefix = "~"
	}

	dir, base := splitDirBase(expanded)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}

	// Reconstruct the display form the user typed for the parent,
	// keeping ~ when they used it. The base is dropped — the caller
	// substitutes the completed name.
	display := tok[:len(tok)-len(base)]
	if homePrefix != "" && !strings.HasPrefix(display, "~") {
		display = homePrefix + display[len(homePrefix):]
	}
	return dir, base, display, true
}

// splitDirBase is like filepath.Split but preserves the trailing
// slash convention: "foo" => (".", "foo"); "foo/" => ("foo", "");
// "a/b" => ("a/", "b"); "/" => ("/", ""). Returned dir always has
// the trailing separator when non-empty so callers can rebuild paths
// by concatenation.
func splitDirBase(p string) (dir, base string) {
	if p == "" {
		return ".", ""
	}
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ".", p
	}
	return p[:i+1], p[i+1:]
}

func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	prefix := ss[0]
	for _, s := range ss[1:] {
		n := 0
		for n < len(prefix) && n < len(s) && prefix[n] == s[n] {
			n++
		}
		prefix = prefix[:n]
		if prefix == "" {
			return ""
		}
	}
	return prefix
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
		i.toolGate = map[string]int{}
		i.statusErr = ""
		i.statusOK = ""
		i.helpBlock = nil
		i.parkedTurn = 0
		i.parkedTotal = 0
		i.scrollOffset = 0
		i.extNotes = nil
		i.shellBlock = nil
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
			var loggedIn []string
			if i.cfg.LoggedInProviders != nil {
				loggedIn = i.cfg.LoggedInProviders()
			}
			i.modelDialog.Open(i.cfg.Model, loggedIn)
		}
	case "/settings":
		i.openSettingsDialog()
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
		// in some target so its later turns have the whole thing
		// in context. With no argument, the target is the current
		// directory. With an argument, the target is whatever the
		// user passed — typed by hand, drag-dropped, or selected
		// via the @ file picker (which is why we accept both files
		// and directories; the @-picker chips for both have already
		// been expanded to absolute paths by expandFileChips above).
		// Dispatched through the normal queue-or-start path so it
		// behaves identically to typing the prompt by hand.
		studyPrompt := buildStudyPrompt(strings.TrimSpace(strings.TrimPrefix(cmd, parts[0])), i.cfg.CWD)
		i.mu.Lock()
		busy := i.busy
		ag := i.agent
		i.mu.Unlock()
		if busy {
			if ag != nil {
				ag.QueueMessage(studyPrompt)
			} else {
				i.mu.Lock()
				i.queued = append(i.queued, studyPrompt)
				i.mu.Unlock()
			}
			i.invalidate()
			break
		}
		i.startTurn(ctx, studyPrompt)
	case "/cd":
		// Hidden command: switch the running session's cwd. Not in
		// slash_suggest, not in /help. Used by the workspaces
		// extension's panel-key Enter handler so picking a row
		// jumps zot into that directory without relaunching.
		//
		// Recovers the raw argument (path) from the original cmd
		// string rather than parts, so paths with spaces survive.
		// The host's ChangeCWD hook handles validation, session
		// close + reopen, agent rebuild, sandbox re-rooting, and
		// re-jail-if-jailed semantics.
		if i.cfg.ChangeCWD == nil {
			i.mu.Lock()
			i.statusErr = "/cd unavailable: host did not wire ChangeCWD"
			i.mu.Unlock()
			break
		}
		path := strings.TrimSpace(strings.TrimPrefix(cmd, parts[0]))
		if path == "" {
			i.mu.Lock()
			i.statusErr = "/cd: missing path"
			i.mu.Unlock()
			break
		}
		if err := i.cfg.ChangeCWD(path); err != nil {
			i.mu.Lock()
			i.statusErr = "/cd: " + err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			break
		}
		// ChangeCWD has already updated i.cfg.CWD and swapped the
		// agent + session. Reset transient TUI state so the new
		// session opens clean.
		i.mu.Lock()
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
		i.helpBlock = nil
		i.parkedTurn = 0
		i.statusOK = "cwd " + i.cfg.CWD
		i.statusErr = ""
		i.mu.Unlock()
		i.fileSuggest.Reset()
		i.fileSuggest.SetCWD(i.cfg.CWD)
		i.invalidate()
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
	case "/telegram", "/tg":
		if len(parts) >= 2 {
			i.doTelegram(parts[1])
			break
		}
		i.openTelegramDialog()
	case "/matrix", "/mx":
		if len(parts) >= 2 {
			i.doMatrix(parts[1])
			break
		}
		i.openMatrixDialog()
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
	case "/swarm":
		i.runSwarm(ctx, parts[1:])
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
	for _, p := range []string{"anthropic", "kimi", "google", "github-copilot"} {
		if creds.Has(p) {
			method := creds.Method(p)
			if method == "oauth" {
				method = "subscription"
			}
			items = append(items, logoutItem{
				label:  providerLabel(p),
				target: p,
				method: method,
			})
		}
	}
	if creds.OpenAI.APIKey != "" {
		items = append(items, logoutItem{label: providerLabel("openai"), target: "openai", method: "api key"})
	}
	if creds.OpenAI.OAuth != nil {
		items = append(items, logoutItem{label: providerLabel("openai-codex"), target: "openai-codex", method: "subscription"})
	}
	for p, c := range creds.AdditionalAPIKeyCreds {
		if c.APIKey != "" {
			items = append(items, logoutItem{label: providerLabel(p), target: p, method: "api key"})
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
// target: "anthropic" | "openai" | "kimi" | "github-copilot" | "all"
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
		providers = append([]string{"anthropic", "openai", "openai-codex", "kimi", "google", "github-copilot"}, auth.APIKeyProviders()...)
	case "anthropic", "openai", "openai-codex", "kimi", "google", "github-copilot":
		providers = []string{target}
	default:
		known := false
		for _, p := range auth.APIKeyProviders() {
			if target == p {
				known = true
				break
			}
		}
		if !known {
			i.mu.Lock()
			i.statusErr = "unknown provider: " + target
			i.mu.Unlock()
			return
		}
		providers = []string{target}
	}

	var errs []string
	clearedCurrent := false
	for _, p := range providers {
		var err error
		switch p {
		case "openai":
			err = store.ClearAPIKey("openai")
		case "openai-codex":
			err = store.ClearOAuth("openai")
		default:
			err = store.Clear(p)
		}
		if err != nil {
			errs = append(errs, p+": "+err.Error())
			continue
		}
		if p == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
			if err := i.cfg.SetKimiCLIFallbackDisabled(true); err != nil {
				errs = append(errs, p+": "+err.Error())
				continue
			}
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

func providerSetupInfo(provider string) (string, []string, bool) {
	const docsURL = "https://raw.githubusercontent.com/patriceckhart/zot/main/docs/providers.md"
	switch provider {
	case "amazon-bedrock":
		return "Amazon Bedrock setup", []string{
			"Amazon Bedrock uses AWS credentials instead of a generic zot API-key entry.",
			"Configure an AWS profile, IAM keys, bearer token, or role-based credentials.",
			"",
			"For Bedrock API keys, set:",
			"  AWS_BEARER_TOKEN_BEDROCK=...",
			"  AWS_REGION=us-east-1",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "google-vertex":
		return "Google Vertex AI setup", []string{
			"Google Vertex AI usually uses Google Cloud credentials and project settings.",
			"Set a Google API key, application-default credentials, or a service account.",
			"",
			"Common environment:",
			"  GOOGLE_CLOUD_API_KEY=...",
			"  GOOGLE_CLOUD_PROJECT=...",
			"  GOOGLE_CLOUD_LOCATION=us-central1",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "cloudflare-workers-ai":
		return "Cloudflare Workers AI setup", []string{
			"Cloudflare Workers AI needs both an API token and an account ID.",
			"",
			"Set:",
			"  CLOUDFLARE_API_KEY=...",
			"  CLOUDFLARE_ACCOUNT_ID=...",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "cloudflare-ai-gateway":
		return "Cloudflare AI Gateway setup", []string{
			"Cloudflare AI Gateway needs an API token, account ID, and gateway ID.",
			"",
			"Set:",
			"  CLOUDFLARE_API_KEY=...",
			"  CLOUDFLARE_ACCOUNT_ID=...",
			"  CLOUDFLARE_GATEWAY_ID=...",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "azure-openai-responses":
		return "Azure OpenAI Responses setup", []string{
			"Azure OpenAI needs an API key plus your Azure endpoint or deployment setup.",
			"",
			"Set:",
			"  AZURE_OPENAI_API_KEY=...",
			"  AZURE_OPENAI_BASE_URL=https://your-resource.openai.azure.com",
			"  AZURE_OPENAI_API_VERSION=2024-02-01",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	default:
		return "", nil, false
	}
}

func (i *Interactive) startAPIKeyFlow(provider string) {
	if title, lines, ok := providerSetupInfo(provider); ok {
		i.dialog.ShowInfo(title, lines)
		return
	}
	if provider == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
		_ = i.cfg.SetKimiCLIFallbackDisabled(false)
	}
	url, err := i.cfg.AuthManager.StartAPIKey(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.ShowWaiting(url)
}

func (i *Interactive) startOAuthFlow(provider string) {
	if provider == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
		_ = i.cfg.SetKimiCLIFallbackDisabled(false)
	}
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
	i.btwDialog.Open(i.cfg.Theme, i.agent, i.agent.System, i.cfg.Model, i.cfg.CWD, seed, i.compactModeEnabled(), tui.NormalizeInputStyle(i.cfg.TUIInputStyle) == tui.InputStyleLines, i.invalidate)
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
// callback and snaps the viewport to the bottom (the latest message)
// so the user lands at the live tail of the resumed conversation.
func (i *Interactive) applySessionSelection(path string) {
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusErr = "session loading is not wired in this build"
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	if i.sessionLoading {
		i.statusErr = "already resuming a session"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.sessionLoading = true
	i.statusOK = "resuming session: " + path
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()

	go func() {
		err := i.cfg.LoadSession(path)
		i.mu.Lock()
		defer i.mu.Unlock()
		i.sessionLoading = false
		if err != nil {
			i.statusErr = err.Error()
			i.statusOK = ""
			i.invalidate()
			return
		}
		i.statusOK = "resumed session: " + path
		i.statusErr = ""
		i.parkedTurn = 0
		i.parkedTotal = 0
		i.scrollOffset = 0
		// Fresh transcript swapped in: drop the auto-follow baseline so
		// the next render's follow guard doesn't see the wholesale
		// length change as a delta and jump the viewport.
		i.prevChatLen = 0
		i.prevChatCols = 0
		i.extNotes = nil
		i.view.InvalidateRenderCache()
		if i.agent != nil {
			i.view.Messages = i.agent.Messages()
			i.cumUsage = i.agent.Cost()
			if last := i.agent.LastTurnUsage(); last.InputTokens > 0 || last.CacheReadTokens > 0 || last.CacheWriteTokens > 0 {
				i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
			} else {
				i.lastCtxInput = 0
			}
			// Snap to the tail again — the swap brought in a fresh
			// transcript whose markdown / chroma cost we don't want
			// blocking the redraw.
			if len(i.view.Messages) > initialResumeTailLimit {
				i.view.TailLimit = initialResumeTailLimit
			} else {
				i.view.TailLimit = 0
			}
		}
		i.invalidate()
	}()
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
	i.swapModel(prov, model, i.cfg.BuildAgentFor, false)
}

// applyRescueModelSelection is like applyModelSelection but routes
// through BuildAgentForRescue so launch-time --api-key / --base-url
// overrides are dropped before the new agent is built. Falls back to
// the regular builder when the host doesn't wire a rescue builder.
func (i *Interactive) applyRescueModelSelection(prov, model string) {
	builder := i.cfg.BuildAgentForRescue
	if builder == nil {
		builder = i.cfg.BuildAgentFor
	}
	i.swapModel(prov, model, builder, true)
}

// swapModel applies a /model selection (or a rescue selection) using
// the supplied builder. rescue=true tags the success message so the
// user can see that launch-time overrides were ignored.
func (i *Interactive) swapModel(prov, model string, builder func(string, string) (*core.Agent, string, string, error), rescue bool) {
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
	// Same provider AND not a rescue retry: just swap the model on
	// the existing agent — no rebuild needed because the underlying
	// client is reusable. Rescue retries always rebuild so a stale
	// auth header / base URL can't carry over.
	if !rescue && i.agent != nil && m.Provider == i.cfg.Provider {
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
	if builder == nil {
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

	ag, p, md, err := builder(m.Provider, m.ID)
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
	if rescue {
		i.statusOK = "rescue retry: switched to " + p + " / " + md + " (ignored --api-key / --base-url)"
	} else {
		i.statusOK = "switched to " + p + " / " + md
	}
	i.statusErr = ""
	// Render cache keys are width+content based, so the new agent's
	// identical messages will reuse the existing entries. Nothing
	// to invalidate.
	i.mu.Unlock()
	// The new agent was built off the base tool registry, so any
	// dynamically-registered tools need to be reattached. The apply
	// helpers are no-ops when their feature is inactive, so the
	// cross-provider path still works on a vanilla setup.
	i.applyAutoSwarmTool(i.autoSwarmEnabled())
	i.applyTelegramTools(i.telegramBridge != nil && i.telegramBridge.Active())
	i.applyMatrixTools(i.matrixBridge != nil && i.matrixBridge.Active())
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
		i.applyAutoSwarmTool(i.autoSwarmEnabled())
		i.applyTelegramTools(i.telegramBridge != nil && i.telegramBridge.Active())
		i.applyMatrixTools(i.matrixBridge != nil && i.matrixBridge.Active())
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
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		case err != nil:
			i.statusErr = "compaction failed: " + err.Error()
			i.statusOK = ""
			i.queued = nil // drop queue on error
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		default:
			i.statusErr = ""
			// Read token count from the compaction message meta.
			tokens := ""
			msgs := i.agent.Messages()
			if len(msgs) > 0 && msgs[0].Meta["compaction"] == "true" {
				tokens = msgs[0].Meta["tokens_before"]
			}
			switch {
			case i.pendingPostCompactNote != "":
				i.statusOK = i.pendingPostCompactNote
			case tokens != "":
				i.statusOK = fmt.Sprintf("compacted from ~%s tokens (ctrl+o to expand)", tokens)
			default:
				i.statusOK = "compacted (ctrl+o to expand)"
			}
			i.pendingPostCompactNote = ""
			i.extNotes = stripAutoCompactNotes(i.extNotes)
			i.lastCtxInput = 0
			i.toolCalls = map[string]*tui.ToolCallView{}
			i.toolOrder = nil
			i.toolGate = map[string]int{}
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

// shellEscapeCommand reports whether text is a "!command" shell
// escape and, if so, returns the command with the leading '!' (and
// surrounding whitespace) stripped. A bare "!" with no command is
// treated as not an escape so it falls through to the normal prompt
// path rather than running an empty shell.
func shellEscapeCommand(text string) (string, bool) {
	trimmed := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(trimmed, "!") {
		return "", false
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(trimmed, "!"))
	if cmd == "" {
		return "", false
	}
	return cmd, true
}

// startShellEscape runs a "!command" in the same shell the bash tool
// uses, in the session working directory, honoring the /jail sandbox.
// It shares the busy/cancel state with the agent: esc cancels it, and
// it refuses to start while a turn or another shell escape is already
// in flight. The terminal-log output is parked in i.shellBlock below
// the transcript until the next prompt or /clear, so it never enters
// the model conversation.
func (i *Interactive) startShellEscape(parent context.Context, cmd string) {
	i.mu.Lock()
	if i.busy || i.shellRunning {
		i.statusErr = "busy — wait for the current turn to finish before running a shell command"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if parent == nil {
		parent = i.runCtx
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	i.busy = true
	i.shellRunning = true
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	i.spin.StartFixed("running shell command")
	// A new shell escape replaces the previous block; clear stale
	// extension notes the same way a new turn would so the screen
	// doesn't accumulate transient state.
	i.shellBlock = nil
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	i.helpBlock = nil
	sandbox := i.cfg.Sandbox
	cwd := i.cfg.CWD
	i.mu.Unlock()
	i.invalidate()

	go func() {
		defer cancel()
		raw, _ := json.Marshal(map[string]any{"command": cmd})
		bash := &tools.BashTool{CWD: cwd, Sandbox: sandbox}
		res, err := bash.Execute(ctx, raw, nil)

		var out string
		if err != nil {
			out = "$ " + cmd + "\n\n" + err.Error() + "\n\n[error]"
		} else {
			for _, c := range res.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					out += tb.Text
				}
			}
		}
		cancelled := ctx.Err() != nil
		failed := err != nil || res.IsError || cancelled
		if cancelled {
			out += "\n\n[cancelled]"
		}

		block := i.renderShellBlock(out, failed)

		i.mu.Lock()
		i.shellRunning = false
		i.busy = false
		i.cancelTurn = nil
		i.shellBlock = block
		if failed {
			if cancelled {
				i.statusErr = "shell command cancelled"
			} else {
				i.statusErr = "shell command failed"
			}
			i.statusOK = ""
		} else {
			i.statusOK = "shell command finished"
			i.statusErr = ""
		}
		i.mu.Unlock()
		i.invalidate()
	}()
}

// renderShellBlock turns merged bash output into a styled terminal-log
// block: each line colored by overall success (tool/green) or failure
// (error/red), with the [exit ...] / [error] footer dimmed via the
// muted color so it reads as metadata.
func (i *Interactive) renderShellBlock(out string, failed bool) []string {
	th := i.cfg.Theme
	base := th.Tool
	if failed {
		base = th.Error
	}
	out = strings.TrimRight(out, "\n")
	lines := strings.Split(out, "\n")
	styled := make([]string, 0, len(lines))
	for _, line := range lines {
		color := base
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[exit ") || strings.HasPrefix(trimmed, "[error]") || strings.HasPrefix(trimmed, "[cancelled]") {
			color = th.Muted
		}
		styled = append(styled, th.FG256(color, line))
	}
	return styled
}

func (i *Interactive) startTurn(parent context.Context, prompt string) {
	i.startTurnWithImages(parent, prompt, nil)
}

func (i *Interactive) startTurnWithImages(parent context.Context, prompt string, images []provider.ImageBlock) {
	if i.agent == nil {
		return
	}
	// Pre-turn safety: if the most recent context measurement is
	// already past the auto-compact threshold, condense before
	// sending so the next outbound request stays under the limit.
	// The condense flow re-fires the user's queued prompt for us, so
	// we just hand it off and exit.
	i.mu.Lock()
	needsPreCompact := !i.autoCompacting && i.shouldAutoCompactLocked()
	if needsPreCompact {
		if prompt != "" {
			i.queued = append([]string{prompt}, i.queued...)
		}
		i.statusErr = ""
		i.extNotes = append(i.extNotes, autoCompactNoteLine(i.cfg.Theme, "context near limit — condensing history before sending..."))
		i.pendingPostCompactNote = "context auto-compacted; sending your last message"
		i.mu.Unlock()
		i.invalidate()
		i.runCompact(parent, true)
		return
	}
	i.mu.Unlock()

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
	i.toolGate = map[string]int{}
	i.shellBlock = nil // sending a prompt clears any parked shell-escape log
	i.extNotes = nil   // ext notes are one-shot; a new prompt clears them
	i.scrollOffset = 0 // jump back to the bottom on new turn
	// Lift the resume tail cap once the user starts interacting. The
	// cap is purely a first-paint optimization (don't markdown the
	// whole history before showing anything). Keeping it active during
	// a turn makes the rendered chat a sliding window: appended
	// messages push older ones off the TOP of the buffer, which the
	// renderer must treat as a change above the viewport and repaint
	// fully, snapping the terminal's native scrollback to the bottom on
	// every streamed chunk. A fresh session has no cap (append-only),
	// which is why the jump only shows up in resumed sessions. Dropping
	// the cap here makes resumed turns append-only too.
	i.view.TailLimit = 0
	// Reset the auto-follow baseline so the very next render at
	// interactive.go:1053 doesn't see a synthetic shrink between
	// "last frame had the previous turn's tool overlay" and
	// "this frame had it cleared above". Without this, the guard
	// reads delta = -(rows in cleared overlay) and decrements
	// scrollOffset, which on terminals that mirror zot's pane
	// scroll into the host scrollbar visibly yanks the viewport.
	// See autofollow_shrink_test.go for the exact arithmetic.
	i.prevChatLen = 0
	i.prevChatCols = 0
	i.parkedTurn = 0 // starting a turn clears the /jump parked state
	i.parkedTotal = 0
	i.helpBlock = nil // hide the help block once the user asks something
	i.mu.Unlock()
	i.invalidate()

	sink := func(ev core.AgentEvent) {
		i.handleEvent(ev)
		i.invalidate()
	}

	go func() {
		err := i.agent.Prompt(ctx, prompt, images, sink)
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
		// Decide whether to offer a model rescue picker for recoverable
		// provider failures (auth/rate/temporary). The picker opens after
		// the mutex is released so it can take its own locks freely.
		var (
			offer       bool
			rescueWhy   string
			rescueImgs  []provider.ImageBlock
			rescueModel string
			rescueProv  string
			rescueFprov string
		)
		if err != nil && ctx.Err() == nil {
			if ok, reason := classifyRescueError(err); ok {
				offer = true
				rescueWhy = reason
				rescueImgs = images
				rescueModel = i.cfg.Model
				rescueProv = i.cfg.Provider
				rescueFprov = extractFailedProvider(err)
				if rescueFprov == "" {
					rescueFprov = i.cfg.Provider
				}
				// Suppress the red banner — the rescue dialog already
				// surfaces the failure.
				i.statusErr = ""
			}
		}
		// Detect HTTP 413 "payload too large" responses. The provider
		// rejected the request because the request body exceeded its
		// per-request limit. Token-based auto-compact can miss this
		// because the limit is on raw bytes, not tokens. Re-queue the
		// prompt so it survives the condense pass and trigger one.
		payloadTooLarge := err != nil && ctx.Err() == nil && isPayloadTooLargeError(err)
		if payloadTooLarge {
			i.statusErr = ""
			i.queued = append([]string{prompt}, i.queued...)
			i.extNotes = append(i.extNotes, autoCompactNoteLine(i.cfg.Theme, "request was too large. condensing history before retrying ..."))
			i.pendingPostCompactNote = "context auto-compacted; retrying your last message"
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
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		}
		// Decide whether the next thing to do is an auto-compaction.
		// Only fires when the turn completed cleanly AND no host-side
		// or agent-side queued messages are waiting (otherwise a queued
		// message would race the condense).
		agentQueued := 0
		if i.agent != nil {
			agentQueued = i.agent.QueuedMessageCount()
		}
		shouldAutoCompact := !hasNext && agentQueued == 0 && err == nil && ctx.Err() == nil && i.shouldAutoCompactLocked()
		i.mu.Unlock()
		i.invalidate()
		parent := i.runCtx
		if parent == nil {
			parent = context.Background()
		}
		switch {
		case hasNext:
			i.startTurn(parent, next)
		case offer:
			i.openRescueDialog(rescueProv, rescueFprov, rescueModel, rescueWhy, prompt, rescueImgs)
		case payloadTooLarge:
			i.runCompact(parent, true)
		case shouldAutoCompact:
			i.runCompact(parent, true)
		}
	}()
}

// openRescueDialog surfaces the rescue model picker after a recoverable
// provider failure. The pending prompt + images are stashed on the
// Interactive so a later applyRescueSelection can re-run the same turn
// against the freshly-picked model. activeProvider/failedProvider are
// usually the same, but some clients embed different prefixes in their
// errors than the configured provider id, so we accept both.
func (i *Interactive) openRescueDialog(activeProvider, failedProvider, failedModel, reason, prompt string, images []provider.ImageBlock) {
	if i.rescueDialog == nil {
		return
	}
	loggedIn := []string{}
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	fprov := failedProvider
	if fprov == "" {
		fprov = activeProvider
	}
	i.mu.Lock()
	i.pendingRescuePrompt = prompt
	i.pendingRescueImages = images
	i.mu.Unlock()
	i.rescueDialog.Open(failedModel, loggedIn, fprov, failedModel, reason, prompt)
	i.invalidate()
}

// applyRescueSelection switches model (cross-provider if needed) and
// re-runs the same prompt+images that just failed. Mirrors
// applyModelSelection's transcript-carry logic so the user keeps full
// session continuity across the swap.
func (i *Interactive) applyRescueSelection(prov, model, prompt string) {
	if model == "" {
		return
	}
	i.applyRescueModelSelection(prov, model)
	i.mu.Lock()
	images := i.pendingRescueImages
	if prompt == "" {
		prompt = i.pendingRescuePrompt
	}
	i.pendingRescuePrompt = ""
	i.pendingRescueImages = nil
	i.mu.Unlock()
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	i.startTurnWithImages(parent, prompt, images)
}

func stripAutoCompactNotes(notes []string) []string {
	if len(notes) == 0 {
		return notes
	}
	out := notes[:0]
	for _, n := range notes {
		if strings.Contains(n, "condensing history") {
			continue
		}
		out = append(out, n)
	}
	return out
}

// autoCompactNoteLine returns a styled chat-area note for the
// inline auto-compact heads-up. Lives in extNotes so it survives
// the busy-spinner overwrite of the status row.
func autoCompactNoteLine(th tui.Theme, msg string) string {
	return "  " + th.FG256(th.Warning, "⚠ "+msg)
}

// isPayloadTooLargeError matches HTTP 413 responses surfaced by the
// provider clients. The error formatting differs slightly between
// providers (anthropic and openai both prepend the status code), so
// we look for the canonical 413 marker as well as the conventional
// 'payload too large' phrase.
func isPayloadTooLargeError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 413") || strings.Contains(msg, " 413") || strings.HasPrefix(msg, "413 ") || strings.Contains(msg, "payload too large") || strings.Contains(msg, "request entity too large")
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
		i.toolGate = map[string]int{}
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
			i.gateToolLocked(e.ID)
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
			if tc.RawJSONBuf == "" {
				tc.RawJSONBuf = string(e.Args)
			}
			tc.Streaming = false
		} else {
			i.toolCalls[e.ID] = &tui.ToolCallView{
				ID:         e.ID,
				Name:       e.Name,
				Args:       tui.ShortArgs(e.Name, e.Args),
				RawJSONBuf: string(e.Args),
			}
			i.toolOrder = append(i.toolOrder, e.ID)
			i.gateToolLocked(e.ID)
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
			i.resetStreamingStateLocked()
			i.statusErr = ""
			i.statusOK = "cancelled"
			return
		}
		if e.Stop == provider.StopLength {
			// The model hit its output-token cap mid-response, so the
			// reply (often a long write/edit) is truncated. Surface it
			// explicitly, otherwise the turn just ends and reads like
			// the UI gave up. The agent already requests the model's
			// full MaxOutput budget, so this means the response genuinely
			// exceeded that ceiling; ask the user to continue.
			i.statusErr = "response hit the model's output-token limit and was cut off, ask it to continue"
			i.statusOK = ""
			return
		}
		// Don't surface mid-loop stream errors as a red banner here.
		// EvTurnEnd fires after every step in a multi-step tool loop,
		// so a transient 503 / network blip would briefly paint a red
		// banner over the still-streaming chat before the agent loop
		// either retries or exits. The final error (if any) is set by
		// startTurnWithImages once Prompt() returns, and recoverable
		// failures are routed to the rescue picker instead — which
		// keeps the chat clean while the agent is working.
		_ = e.Err
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
	i.statusOK = "reloading extensions..."
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
	i.applyTelegramTools(true)
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
	i.applyTelegramTools(false)
	i.mu.Lock()
	i.statusOK = "telegram disconnected"
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// telegramSenderAdapter wraps the bridge so the tools package can
// drive it without importing telegram directly. The Active() check
// is forwarded to the bridge so the tool can fail clearly with a
// model-readable error when the user disconnected mid-turn.
type telegramSenderAdapter struct {
	bridge *telegram.Bridge
}

func (a telegramSenderAdapter) SendImage(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("telegram bridge is not connected")
	}
	return a.bridge.SendImage(ctx, path, caption)
}

func (a telegramSenderAdapter) SendDocument(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("telegram bridge is not connected")
	}
	return a.bridge.SendDocument(ctx, path, caption)
}

func (a telegramSenderAdapter) Active() bool {
	return a.bridge != nil && a.bridge.Active()
}

// swarmWatchEntry is one tracked auto-swarm sub-agent. Filled in at
// spawn time and finalised in trackSwarmAgent's waiter goroutine.
type swarmWatchEntry struct {
	agent *swarm.Agent
	task  string
	done  bool
	err   string
}

// TrackSwarmAgent is the exported entry point used by the cli to
// hand a freshly-spawned auto-swarm agent off to the watcher.
func (i *Interactive) TrackSwarmAgent(a *swarm.Agent, task string) {
	i.trackSwarmAgent(a, task)
}

// trackSwarmAgent records a freshly-spawned auto-swarm agent and
// subscribes to its prompt-level task completion events. Sub-agents
// are long-lived daemons that keep running on the inbox after the
// initial task, so we can't wait on agent.Wait() — it never returns
// until the whole daemon dies. Instead we mark each entry done when
// the swarm daemon reports the initial prompt has finished, and when
// every tracked entry has reported in, flush a single summary into
// the main chat.
//
// Wired in from cli.go via SwarmSpawnTool.OnSpawned only when auto-
// swarm is enabled, so this is a no-op when the feature is off.
func (i *Interactive) trackSwarmAgent(a *swarm.Agent, task string) {
	if i == nil || a == nil {
		return
	}
	entry := &swarmWatchEntry{agent: a, task: task}
	i.swarmWatchMu.Lock()
	i.swarmWatch = append(i.swarmWatch, entry)
	i.swarmWatchMu.Unlock()

	a.SetOnTurnEnd(func(step int, errMsg string) {
		i.swarmWatchMu.Lock()
		if entry.done {
			i.swarmWatchMu.Unlock()
			return
		}
		entry.done = true
		entry.err = errMsg
		allDone := true
		for _, e := range i.swarmWatch {
			if !e.done {
				allDone = false
				break
			}
		}
		var batch []*swarmWatchEntry
		if allDone {
			batch = i.swarmWatch
			i.swarmWatch = nil
		}
		i.swarmWatchMu.Unlock()
		if len(batch) == 0 {
			return
		}
		i.flushSwarmSummary(batch)
	})
}

// flushSwarmSummary composes a synthetic user turn describing every
// sub-agent's outcome and injects it via SubmitOrQueue so the main
// agent picks it up at the next safe boundary. The summary is
// phrased as a system update ("Auto-swarm finished: ...") so the
// model treats it as observed state, not as a fresh user request.
func (i *Interactive) flushSwarmSummary(batch []*swarmWatchEntry) {
	if len(batch) == 0 {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[auto-swarm update] %d sub-agent(s) finished:\n\n", len(batch))
	for idx, e := range batch {
		snap := e.agent.Snapshot()
		status := string(snap.Status)
		task := snap.Task
		if task == "" {
			task = e.task
		}
		fmt.Fprintf(&sb, "%d. agent %s \u2014 status: %s\n", idx+1, snap.ID, status)
		fmt.Fprintf(&sb, "   task: %s\n", truncateForSummary(task, 240))
		if snap.Err != "" {
			fmt.Fprintf(&sb, "   error: %s\n", truncateForSummary(snap.Err, 240))
		} else if e.err != "" {
			fmt.Fprintf(&sb, "   turn error: %s\n", truncateForSummary(e.err, 240))
		}
		if tail := strings.TrimSpace(snap.Tail); tail != "" {
			fmt.Fprintf(&sb, "   tail: %s\n", truncateForSummary(tail, 600))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Briefly summarise the collective outcome for the user. Reference the agents by id. If any failed, suggest a follow-up; otherwise confirm completion. Do not spawn new sub-agents unless the user asks.")
	i.SubmitOrQueue(sb.String(), nil)
}

func truncateForSummary(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// applyAutoSwarmSystemPrompt appends (active=true) or strips
// (active=false) the auto-swarm system-prompt block on the running
// agent so the model proactively considers swarm_spawn when the user
// flips the toggle. The block lives at the tail of agent.System so
// stripping is a plain suffix-trim; idempotent in both directions.
func (i *Interactive) applyAutoSwarmSystemPrompt(active bool) {
	if i.agent == nil {
		return
	}
	addendum := i.cfg.AutoSwarmSystemAddendum
	if addendum == "" {
		return
	}
	sys := i.agent.System
	has := strings.Contains(sys, addendum)
	switch {
	case active && !has:
		if sys != "" && !strings.HasSuffix(sys, "\n\n") {
			sys += "\n\n"
		}
		i.agent.System = sys + addendum
	case !active && has:
		i.agent.System = strings.TrimRight(strings.ReplaceAll(sys, addendum, ""), "\n") + "\n"
	}
}

// applyAutoSwarmTool registers (active=true) or removes (active=false)
// the swarm_spawn tool on the running agent so the model only sees it
// when /settings -> auto-swarm is enabled. Mirrors applyTelegramTools'
// snapshot+mutate pattern so extension tools and /reload-ext additions
// survive a toggle.
func (i *Interactive) autoSwarmEnabled() bool {
	return i.cfg.AutoSwarmEnabled != nil && *i.cfg.AutoSwarmEnabled
}

func (i *Interactive) applyAutoSwarmTool(active bool) {
	if i.agent == nil {
		return
	}
	current := i.agent.Tools
	next := core.Registry{}
	for name, t := range current {
		if name == "swarm_spawn" {
			continue
		}
		next[name] = t
	}
	if active && i.cfg.Swarm != nil {
		next["swarm_spawn"] = &tools.SwarmSpawnTool{
			Swarm:           i.cfg.Swarm,
			Enabled:         func() bool { return true },
			DefaultModel:    func() string { return i.cfg.Model },
			DefaultProvider: func() string { return i.cfg.Provider },
			OnSpawned:       i.trackSwarmAgent,
		}
	}
	i.agent.SetTools(next)
}

// applyTelegramTools registers (active=true) or removes (active=false)
// the telegram_send_image and telegram_send_file tools on the running
// agent so the model only sees them while the bridge is connected.
// Snapshots and mutates the live tool registry so any extension or
// /reload-ext additions made while Telegram is connected survive a
// later /telegram disconnect (we only add or strip the two telegram
// entries, never the rest).
func (i *Interactive) applyTelegramTools(active bool) {
	if i.agent == nil {
		return
	}
	current := i.agent.Tools
	next := core.Registry{}
	for name, t := range current {
		if name == "telegram_send_image" || name == "telegram_send_file" {
			continue
		}
		next[name] = t
	}
	if active {
		sender := telegramSenderAdapter{bridge: i.telegramBridge}
		next["telegram_send_image"] = &tools.TelegramSendImageTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
		next["telegram_send_file"] = &tools.TelegramSendFileTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
	}
	i.agent.SetTools(next)
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

func (h *telegramHost) Status() string {
	h.iv.mu.Lock()
	providerName := h.iv.cfg.Provider
	model := h.iv.cfg.Model
	cwd := h.iv.cfg.CWD
	usage := h.iv.cumUsage
	subscription := h.iv.cfg.AuthMethod == "oauth"
	ctxUsed := h.iv.lastCtxInput
	busy := h.iv.busy
	queued := len(h.iv.queued)
	h.iv.mu.Unlock()

	ctxMax := 0
	if m, err := provider.FindModel(providerName, model); err == nil {
		ctxMax = m.ContextWindow
	}
	return telegram.FormatStatus(telegram.StatusSnapshot{
		Provider:     providerName,
		Model:        model,
		CWD:          cwd,
		Usage:        usage,
		Subscription: subscription,
		ContextUsed:  ctxUsed,
		ContextMax:   ctxMax,
		Busy:         busy,
		Queued:       queued,
	})
}

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

// openMatrixDialog shows the picker for `/matrix` with no arg.
func (i *Interactive) openMatrixDialog() {
	items := i.matrixMenuItems()
	if len(items) == 0 {
		i.mu.Lock()
		i.statusErr = "matrix not configured. run `zot matrix-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.matrixDialog.Open(items)
	i.invalidate()
}

// matrixMenuItems builds the dialog entries for the current bridge
// state. Returns empty when no matrix.json exists so the caller can
// show a helpful status line instead of an empty menu.
func (i *Interactive) matrixMenuItems() []matrixItem {
	cfg, err := matrix.LoadConfig(i.cfg.ZotHome)
	if err != nil || cfg.AccessToken == "" {
		return nil
	}
	var items []matrixItem
	if i.matrixBridge != nil && i.matrixBridge.Active() {
		items = append(items, matrixItem{label: "disconnect", action: "disconnect", hint: "stop mirroring"})
		st := i.matrixBridge.State()
		hint := "active"
		if st.UserID != "" {
			hint += " as " + st.UserID
		}
		if st.E2EE {
			hint += " (e2ee)"
		}
		items = append(items, matrixItem{label: "status", action: "status", hint: hint})
	} else {
		label := "connect"
		hint := "start mirroring dms into this session"
		if cfg.AllowedUserID == "" {
			hint = "awaiting pairing (DM the bot once connected)"
		}
		items = append(items, matrixItem{label: label, action: "connect", hint: hint})
		items = append(items, matrixItem{label: "status", action: "status", hint: "disconnected"})
	}
	return items
}

// doMatrix dispatches one of the three explicit actions. Called
// from /matrix <action> or after the picker selects a row.
func (i *Interactive) doMatrix(action string) {
	switch action {
	case "connect":
		i.matrixConnect()
	case "disconnect":
		i.matrixDisconnect()
	case "status":
		i.matrixStatus()
	default:
		i.mu.Lock()
		i.statusErr = "unknown matrix action: " + action + " (use connect, disconnect, or status)"
		i.mu.Unlock()
		i.invalidate()
	}
}

// matrixConnect starts the bridge. Refuses if it's already running
// or if the on-disk matrix.json is missing an access token.
func (i *Interactive) matrixConnect() {
	if i.matrixBridge != nil && i.matrixBridge.Active() {
		i.mu.Lock()
		i.statusOK = "matrix already connected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	cfg, err := matrix.LoadConfig(i.cfg.ZotHome)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "matrix: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if cfg.AccessToken == "" {
		i.mu.Lock()
		i.statusErr = "matrix: not configured. run `zot matrix-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Refuse to start when a background daemon is already syncing
	// the same account. Two concurrent sync consumers race on the
	// sync cursor and events get dropped.
	if pid, alive, _ := matrix.IsRunning(i.cfg.ZotHome); alive && pid > 0 {
		i.mu.Lock()
		i.statusErr = fmt.Sprintf("matrix: bot daemon already running (pid %d). stop it with `zot matrix-bot stop` first.", pid)
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.matrixBridge = &matrix.Bridge{
		Config:  cfg,
		Save:    func(next matrix.Config) error { return matrix.SaveConfig(i.cfg.ZotHome, next) },
		Host:    &matrixHost{iv: i},
		ZotHome: i.cfg.ZotHome,
	}
	if err := i.matrixBridge.Start(i.runCtx); err != nil {
		i.matrixBridge = nil
		i.mu.Lock()
		i.statusErr = "matrix connect failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.applyMatrixTools(true)
	state := i.matrixBridge.State()
	label := "matrix connected as " + state.UserID
	if state.E2EE {
		label += " (e2ee)"
	}
	if state.PairedID == "" {
		label += " — DM the bot from your client to claim it"
	}
	i.mu.Lock()
	i.statusOK = label
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// matrixDisconnect stops the bridge. No-op when already stopped.
func (i *Interactive) matrixDisconnect() {
	if i.matrixBridge == nil || !i.matrixBridge.Active() {
		i.mu.Lock()
		i.statusOK = "matrix already disconnected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.matrixBridge.Stop()
	i.applyMatrixTools(false)
	i.mu.Lock()
	i.statusOK = "matrix disconnected"
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// matrixSenderAdapter wraps the bridge so the tools package can
// drive it without importing matrix directly.
type matrixSenderAdapter struct {
	bridge *matrix.Bridge
}

func (a matrixSenderAdapter) SendImage(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("matrix bridge is not connected")
	}
	return a.bridge.SendImage(ctx, path, caption)
}

func (a matrixSenderAdapter) SendDocument(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("matrix bridge is not connected")
	}
	return a.bridge.SendDocument(ctx, path, caption)
}

func (a matrixSenderAdapter) Active() bool {
	return a.bridge != nil && a.bridge.Active()
}

// applyMatrixTools registers (active=true) or removes (active=false)
// the matrix_send_image and matrix_send_file tools on the running
// agent so the model only sees them while the bridge is connected.
// Mirrors applyTelegramTools' snapshot+mutate pattern.
func (i *Interactive) applyMatrixTools(active bool) {
	if i.agent == nil {
		return
	}
	current := i.agent.Tools
	next := core.Registry{}
	for name, t := range current {
		if name == "matrix_send_image" || name == "matrix_send_file" {
			continue
		}
		next[name] = t
	}
	if active {
		sender := matrixSenderAdapter{bridge: i.matrixBridge}
		next["matrix_send_image"] = &tools.MatrixSendImageTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
		next["matrix_send_file"] = &tools.MatrixSendFileTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
	}
	i.agent.SetTools(next)
}

// matrixStatus writes a one-liner describing the bridge state.
func (i *Interactive) matrixStatus() {
	var msg string
	if i.matrixBridge != nil && i.matrixBridge.Active() {
		s := i.matrixBridge.State()
		msg = "matrix: connected (tui bridge)"
		if s.UserID != "" {
			msg += " as " + s.UserID
		}
		if s.E2EE {
			msg += " (e2ee)"
		}
		if s.PairedID != "" {
			msg += " - paired with " + s.PairedID
		} else {
			msg += " - awaiting pairing"
		}
	} else if pid, alive, _ := matrix.IsRunning(i.cfg.ZotHome); alive && pid > 0 {
		msg = fmt.Sprintf("matrix: background daemon running (pid %d) - /matrix connect won't work until you stop it", pid)
	} else {
		cfg, _ := matrix.LoadConfig(i.cfg.ZotHome)
		if cfg.AccessToken == "" {
			msg = "matrix: not configured. run `zot matrix-bot setup` first."
		} else {
			msg = "matrix: disconnected"
			if cfg.UserID != "" {
				msg += " (" + cfg.UserID + " ready to connect)"
			}
		}
	}
	i.mu.Lock()
	i.statusOK = msg
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// matrixHost adapts *Interactive to matrix.Host so the bridge can
// call back into the TUI without importing modes directly.
type matrixHost struct{ iv *Interactive }

func (h *matrixHost) SubmitOrQueue(prompt string, images []provider.ImageBlock) {
	h.iv.SubmitOrQueue(prompt, images)
}

func (h *matrixHost) CancelTurn() { h.iv.CancelTurn() }

func (h *matrixHost) Status() string {
	h.iv.mu.Lock()
	providerName := h.iv.cfg.Provider
	model := h.iv.cfg.Model
	cwd := h.iv.cfg.CWD
	usage := h.iv.cumUsage
	subscription := h.iv.cfg.AuthMethod == "oauth"
	ctxUsed := h.iv.lastCtxInput
	busy := h.iv.busy
	queued := len(h.iv.queued)
	h.iv.mu.Unlock()

	ctxMax := 0
	if m, err := provider.FindModel(providerName, model); err == nil {
		ctxMax = m.ContextWindow
	}
	return bot.FormatStatus(bot.StatusSnapshot{
		Provider:     providerName,
		Model:        model,
		CWD:          cwd,
		Usage:        usage,
		Subscription: subscription,
		ContextUsed:  ctxUsed,
		ContextMax:   ctxMax,
		Busy:         busy,
		Queued:       queued,
	})
}

func (h *matrixHost) Notify(level, message string) {
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
	i.openAllToolGatesLocked()
}

// openAllToolGatesLocked drops every pending tool gate so that any
// tool registered during this turn renders unconditionally from now
// on. Called when streaming finalizes (the paced text has fully
// drained and `streaming` is about to reset to length 0): without
// this, the gate comparison against a freshly-reset streaming buffer
// would wrongly re-hide tools that had already cleared their gate.
// Must be called with i.mu held.
func (i *Interactive) openAllToolGatesLocked() {
	for id := range i.toolGate {
		i.toolGate[id] = 0
	}
}

// gateToolLocked records the stream position at which a tool call may
// become visible. The gate is the total length the streaming buffer
// will reach once the pacer has drained everything currently queued
// (already painted + still pending). Holding the tool block back
// until the pacer crosses that mark guarantees the prose emitted
// before the tool call finishes typing out above it, instead of the
// tool block snapping in while the paragraph is still filling in.
//
// We only gate while text is actively streaming. If no stream is in
// flight (gate 0), the tool shows immediately, which is the correct
// behaviour for tool-only turns and replayed sessions. First
// registration wins so a later EvToolCall can't move an existing
// gate. Must be called with i.mu held.
func (i *Interactive) gateToolLocked(id string) {
	if _, ok := i.toolGate[id]; ok {
		return
	}
	if !i.streamOn {
		i.toolGate[id] = 0
		return
	}
	i.toolGate[id] = i.streaming.Len() + len(i.streamPending)
}

// toolGateOpenLocked reports whether the gated tool block may render
// yet, i.e. the pacer has drained enough text to reach the position
// recorded when the tool call arrived. Must be called with i.mu held.
func (i *Interactive) toolGateOpenLocked(id string) bool {
	gate, ok := i.toolGate[id]
	if !ok || gate == 0 {
		return true
	}
	return i.streaming.Len() >= gate
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
	text := assistantVisibleText(m)
	if strings.TrimSpace(text) == "" {
		return
	}
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		go i.telegramBridge.OnAssistantText(text)
	}
	if i.matrixBridge != nil && i.matrixBridge.Active() {
		go i.matrixBridge.OnAssistantText(text)
	}
}

// assistantVisibleText concatenates the text blocks of an assistant
// message into a single string, used by both the telegram and matrix
// bridge mirrors.
func assistantVisibleText(m provider.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
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
					i.openAllToolGatesLocked()
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
