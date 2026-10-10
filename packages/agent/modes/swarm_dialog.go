package modes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

// swarmDialog is the dashboard shown by /swarm (no argument) and
// /swarm list. It's a read-only window onto a *swarm.Swarm; the
// caller passes a snapshotter so the dialog never holds the swarm
// pointer or its lock.
//
// Keys (list view):
//
//	n          spawn a new agent (inline task prompt)
//	p          send a follow-up prompt to the selected agent
//	R          resume (restart) a detached / finished agent
//	↑/↓        move cursor
//	enter      show transcript tail for the selected agent
//	k          kill (Stop) the selected running agent
//	r          remove a terminated agent (clears its state)
//	esc / q    close
//
// Keys (transcript view):
//
//	p          send a follow-up prompt to this agent (inline editor)
//	↑/↓/PgUp/PgDn/Home/End  scroll
//	esc / q    return to the list
type swarmDialog struct {
	active      bool
	compactMode bool
	lineInput   bool
	snapshot    func() []swarm.AgentSnapshot
	stop        func(id string) error
	remove      func(id string) error
	// spawn accepts an optional model + provider override (empty
	// strings mean "let the child resolve its own default"). The cli
	// adapter forwards these to swarm.Swarm.SpawnReq.
	spawn func(task, model, provider string) error
	// send delivers a follow-up user turn to a running agent's inbox.
	// Wired by Open(), when nil the inline 'p' shortcut is disabled.
	send func(id, text string) error
	// Optional image-aware adapters preserve compatibility with text-only
	// dialog embedders. Unsupported image input is refused without clearing it.
	spawnWithImages func(task, model, provider string, images []provider.ImageBlock) error
	sendWithImages  func(id, text string, images []provider.ImageBlock) error
	// resume restarts a detached or terminated agent on its existing
	// session. Wired by Open(); when nil the inline 'R' shortcut is
	// disabled.
	resume func(id string) error

	rows    []swarm.AgentSnapshot
	cursor  int
	viewing bool // when true, show full transcript of the selected row

	// transcriptSpin animates the busy hint shown above the inline
	// editor while the agent is mid-turn (status running, activity
	// anything other than idle). Same shape as the /btw spinner and
	// the main chat's busy line, so the user gets a consistent
	// "something's happening" signal. Lazily created on first
	// busy frame; reset by Open / Close.
	transcriptSpin *spinner

	// spawning toggles the inline "enter task" editor. While true the
	// dialog captures keys into a real tui.Editor so swarm-new has the
	// same editing, paste, drag-drop file chips, and visual style as
	// the default input and /btw input.
	spawning      bool
	newTaskEd     *tui.Editor
	fileSuggest   *fileSuggester
	cwd           string
	fileContext   context.Context
	sandbox       *tools.Sandbox
	transferFiles bool

	// pendingModel / pendingProvider are the model + provider every
	// freshly-spawned agent will be pinned to. They're seeded from
	// the host's currently-active model (via SetCurrentModel) at the
	// moment the user opens the dashboard, so swarm agents inherit
	// whatever the user last picked via /model. Empty means "don't
	// pin a model; let the child resolve its own default".
	pendingModel    string
	pendingProvider string

	// pickingModel + modelPicker drive the in-spawn-editor /model
	// command: while typing a task into the spawn editor, the user
	// can submit just "/model" to pop the same picker /model uses
	// globally, choose a different model for this spawn, and resume
	// the editor with the new pending model captured. Empty when
	// the picker isn't on screen.
	pickingModel        bool
	modelPicker         *modelDialog
	modelPickerLoggedIn []string

	// spawnDraft remembers the editor buffer when we suspend it to
	// show the model picker, so the user doesn't lose half-typed
	// task text just because they wanted to switch models. Restored
	// when the picker closes (whether by select or Esc).
	spawnDraft string

	// prompting toggles the inline "send prompt to selected agent"
	// editor. Distinct from `spawning` so we don't accidentally
	// route a follow-up into Swarm.Spawn. The editor and its
	// file-suggest live in their own fields so opening the prompt
	// editor while a spawn editor is half-typed doesn't clobber it
	// (the dialog enforces one-at-a-time, but the separate state
	// keeps the lifecycle obvious).
	prompting     bool
	promptEd      *tui.Editor
	promptSuggest *fileSuggester
	// promptTargetID is the agent id captured the moment the user
	// pressed 'p'. We pin it here so the prompt routes to the same
	// agent even if the user's cursor moves (or rows reorder) while
	// they're typing.
	promptTargetID string
}

func newSwarmDialog() *swarmDialog { return &swarmDialog{} }

// SetCurrentModel pins the model + provider every fresh spawn will
// inherit. The host wires this to the same Model / Provider the rest
// of zot is currently using, so agents started from the dashboard
// run on whatever the user last picked via /model. Pass empty
// strings to clear the override (the child then resolves its own
// default the same way a bare `zot` invocation does).
func (d *swarmDialog) SetCompactMode(enabled bool) {
	d.compactMode = enabled
}

func (d *swarmDialog) SetLineInput(enabled bool) {
	d.lineInput = enabled
}

func (d *swarmDialog) editorPrompt(th tui.Theme) string {
	if d.lineInput {
		return ""
	}
	return th.AccentBar(th.Accent)
}

func (d *swarmDialog) SetCurrentModel(model, providerID string) {
	d.pendingModel = model
	d.pendingProvider = providerID
}

// SetLoggedInProviders supplies the list of providers the user has
// active credentials for. Forwarded to the embedded modelDialog when
// the user invokes /model inside the spawn editor so the picker can
// filter out unauthenticated providers, matching the global /model
// behaviour.
func (d *swarmDialog) SetLoggedInProviders(provs []string) {
	d.modelPickerLoggedIn = provs
}

// Open binds the dialog to a swarm via the provided callbacks. The
// dialog refreshes the snapshot every Render so the dashboard reads
// live state without needing a goroutine. spawn / send are optional;
// when nil the corresponding inline shortcut ('n' / 'p') is disabled.
func (d *swarmDialog) Open(
	snapshot func() []swarm.AgentSnapshot,
	stop func(id string) error,
	remove func(id string) error,
	spawn func(task, model, provider string) error,
	send func(id, text string) error,
	resume func(id string) error,
	cwd string,
) {
	d.snapshot = snapshot
	d.stop = stop
	d.remove = remove
	d.spawn = spawn
	d.send = send
	d.resume = resume
	d.active = true
	d.cursor = 0
	d.viewing = false
	d.spawning = false
	d.newTaskEd = nil
	d.fileSuggest = nil
	d.prompting = false
	d.promptEd = nil
	d.promptSuggest = nil
	d.promptTargetID = ""
	// pendingModel / pendingProvider are NOT cleared here: the host
	// sets them via SetCurrentModel just before opening the dialog
	// so the spawn flow inherits whatever /model selected. Resetting
	// here would undo that.
	d.pickingModel = false
	d.modelPicker = nil
	d.spawnDraft = ""
	d.transcriptSpin = nil
	d.cwd = cwd
	d.refresh()
}

// CursorPos returns the row/col for the terminal cursor while an
// inline editor (spawn or prompt) is active so the caret blinks in
// the right spot. Returns -1, -1 when no input is being captured.
func (d *swarmDialog) CursorPos(width int) (row, col int) {
	if !d.Active() {
		return -1, -1
	}
	// Spawn editor (list view): rendered directly under the frame
	// header, optionally preceded by the model banner row (when a
	// model was picked) and the @ file-suggest popup, with one
	// breathing-room blank row inserted just above the editor. The
	// constants below must mirror the spawn-editor render path.
	if d.spawning && d.newTaskEd != nil {
		modelBannerRows := 0
		if d.pendingModel != "" {
			modelBannerRows = 1
		}
		popupRows := 0
		if d.fileSuggest != nil && d.fileSuggest.Active(d.newTaskEd.Value()) {
			popupRows = len(d.fileSuggest.Render(d.newTaskEd.Value(), tui.Theme{}, width))
		}
		d.newTaskEd.Prompt = d.editorPrompt(tui.Theme{})
		_, eRow, eCol := d.newTaskEd.Render(width - 2)
		const headerRows = 1
		const editorBlankRow = 1
		padAfterHeaderRows := 0
		if modelBannerRows > 0 {
			// interactive.Render wraps dialog output with padDialogFrame,
			// which inserts a blank row after the frame header when the
			// first body row is non-empty. The model banner is that first
			// body row in the spawn editor, so mirror the extra row here.
			padAfterHeaderRows = 1
		}
		return headerRows + padAfterHeaderRows + modelBannerRows + popupRows + editorBlankRow + eRow, eCol + 2
	}
	// Prompt editor cursor placement. Two flavours:
	//   1. List-view modal (`p` from the dashboard list): renderPromptEditor
	//      draws frame header + "send to <id>:" hint + popup + editor.
	//   2. Transcript view (always-on editor, /btw-style): the editor
	//      lives at the bottom of renderTranscript's output, after the
	//      metadata block, the "── transcript ──" rule, the transcript
	//      body itself, optional "↑/↓ N more" rows and the @-popup.
	//
	// Both flavours funnel through here. The math must mirror the
	// corresponding render path exactly.
	if d.prompting && d.promptEd != nil {
		popupRows := 0
		if d.promptSuggest != nil && d.promptSuggest.Active(d.promptEd.Value()) {
			popupRows = len(d.promptSuggest.Render(d.promptEd.Value(), tui.Theme{}, width))
		}
		d.promptEd.Prompt = d.editorPrompt(tui.Theme{})
		_, eRow, eCol := d.promptEd.Render(width - 2)

		if d.viewing {
			return d.transcriptEditorCursorRow(width, popupRows, eRow), eCol + 2
		}

		// List-view modal layout after interactive.Render applies
		// padDialogFrame: frame header + inserted pad row + "send to
		// <id>:" hint + popup + blank + editor.
		const headerRows = 1
		const padAfterHeaderRows = 1
		const sendToHintRow = 1
		const editorBlankRow = 1
		return headerRows + padAfterHeaderRows + sendToHintRow + popupRows + editorBlankRow + eRow, eCol + 2
	}
	return -1, -1
}

// transcriptEditorCursorRow computes the visual row the caret
// belongs on when the always-on editor sits at the bottom of the
// transcript view. Mirrors renderTranscript's row layout so the
// terminal cursor stays glued to the editor as the body grows.
func (d *swarmDialog) transcriptEditorCursorRow(width, popupRows, editorRowOffset int) int {
	a := d.selected()
	if a == nil {
		return -1
	}
	row := 1 // frame header
	row++    // padDialogFrame blank row after header (next row is task metadata)
	row += 3 // task / dir / status (mirrors renderTranscript's fixed header rows)
	if a.Model != "" {
		row++
	}
	if a.Err != "" {
		row++
	}
	row += 3 // blank + "── transcript ──" + trailing blank

	// Replay transcript body sizing (no-transcript-yet path vs.
	// real body path) so the editor row count matches what render
	// actually emitted.
	raw := a.Lines
	if len(raw) == 0 && a.Tail != "" {
		raw = strings.Split(a.Tail, "\n")
	}
	if len(raw) == 0 {
		row++ // "(no transcript yet)" line
	} else {
		// Full body, no scroll window — the host's outer scrollback
		// owns overflow now. Cursor row math just counts every
		// rendered line.
		body := renderSwarmTranscriptBlocks(raw, tui.Theme{}, width, d.compactMode)
		row += len(body)
	}

	row += popupRows // @-file-suggest popup, if active
	if agentIsBusy(a) {
		row += 2 // blank + spinner row inserted above the editor
	}
	row++ // blank above editor (appendTranscriptEditor)
	return row + editorRowOffset
}

func (d *swarmDialog) Active() bool { return d != nil && d.active }
func (d *swarmDialog) Close()       { d.active = false }

// NeedsTickRefresh reports whether the host's animation tick should
// repaint the screen while this dialog is open. True when the
// dashboard's list/transcript view is on (rows / activity / age and
// the transcript itself are background-driven and need a periodic
// redraw — essential for streaming agent replies to actually appear
// on screen). False only while a modal editor (spawn, list-view
// prompt, model picker) has the dialog to itself; those need the
// terminal's cursor blink, which a full-frame redraw cancels.
//
// Note: the transcript view's always-on editor does NOT suppress
// tick refresh. The cursor blink still works because the tick rate
// is well below the terminal's blink rate, and we need the redraws
// for streaming output. /btw makes the same trade-off.
func (d *swarmDialog) NeedsTickRefresh() bool {
	if d == nil || !d.active {
		return false
	}
	if d.spawning || d.pickingModel {
		return false
	}
	// The list-view `p` modal (prompting without viewing) suppresses
	// tick like the other modal editors. The transcript-view
	// always-on editor (viewing && prompting) does not — streaming
	// agent output needs periodic redraws to render.
	if d.prompting && !d.viewing {
		return false
	}
	return true
}

// OpenViewing opens the dashboard and jumps straight to the
// transcript view for the agent with id agentID. Used by /swarm logs
// <id>. If no such agent is found the dashboard opens normally so
// the user can see why (and pick from the list).
func (d *swarmDialog) OpenViewing(
	agentID string,
	snapshot func() []swarm.AgentSnapshot,
	stop func(id string) error,
	remove func(id string) error,
	spawn func(task, model, provider string) error,
	send func(id, text string) error,
	resume func(id string) error,
	cwd string,
) bool {
	d.Open(snapshot, stop, remove, spawn, send, resume, cwd)
	rows := d.snapshot()
	for i, r := range rows {
		if r.ID == agentID || strings.HasPrefix(r.ID, agentID) {
			d.cursor = i
			d.viewing = true
			// Auto-open the inline editor for running agents so
			// /swarm logs <id> drops the user straight into a
			// /btw-style send-ready prompt.
			if r.Status == swarm.StatusRunning && d.send != nil {
				d.openPromptEditor(tui.Theme{}, r.ID)
			}
			return true
		}
	}
	return false
}

// OpenForResume opens the dashboard and parks the cursor on the first
// resumable agent (any non-running status), so the user can press R
// to resume without typing an id. Returns the count of resumable
// agents found — callers surface that as "3 resumable, press R" or,
// when zero, "no agents available to resume".
//
// The dialog still shows the full list (live + detached + terminated)
// so the user has context: pressing ↑/↓ they can still walk through
// running agents, and the R key is a no-op there (Resume rejects
// non-terminal agents with a clear error).
func (d *swarmDialog) OpenForResume(
	snapshot func() []swarm.AgentSnapshot,
	stop func(id string) error,
	remove func(id string) error,
	spawn func(task, model, provider string) error,
	send func(id, text string) error,
	resume func(id string) error,
	cwd string,
) (resumable int) {
	d.Open(snapshot, stop, remove, spawn, send, resume, cwd)
	// d.rows was populated by Open's refresh(); walk it for the
	// first resumable row and count the total in one pass.
	for i, r := range d.rows {
		if isResumable(r.Status) {
			if resumable == 0 {
				d.cursor = i
			}
			resumable++
		}
	}
	return resumable
}

// isResumable reports whether a Status is eligible for Resume. Mirror
// of the precondition inside Swarm.Resume; centralised so the dialog
// and the slash handler agree on what "resumable" means.
func isResumable(s swarm.Status) bool {
	switch s {
	case swarm.StatusDetached,
		swarm.StatusDone,
		swarm.StatusFailed,
		swarm.StatusKilled:
		return true
	}
	return false
}

// promptDisabledHint returns the status-bar message shown when the
// user presses 'p' on an agent that can't receive input. We branch on
// status so the hint points at the right next step: resume for the
// states Swarm.Resume accepts; nothing useful otherwise.
func promptDisabledHint(s swarm.Status) string {
	if isResumable(s) {
		return "prompt: agent is " + string(s) + "; press R to resume first"
	}
	return "prompt: agent is " + string(s) + " and can't receive input"
}

// killDisabledHint mirrors promptDisabledHint for the 'k' shortcut.
// Kill only makes sense on running / pending agents; on detached and
// terminal ones it's a no-op and the user usually wants 'r' (remove)
// to clear out the agent's state instead.
func killDisabledHint(s swarm.Status) string {
	return "kill: agent is " + string(s) + "; nothing to stop (press r to remove)"
}

// friendlySendErr turns a raw send error into a status-bar message
// the user can act on. The most common failure mode — dialling an
// agent whose previous subprocess exited — surfaces as
// swarm.ErrNotReady; we rewrite it to mention resume instead of
// leaking the unix-socket path. Other errors fall through verbatim
// because they're rare and almost always indicate a real bug.
func friendlySendErr(id string, err error) string {
	if errors.Is(err, swarm.ErrNotReady) {
		return "send: agent " + id + " isn't accepting input (press R to resume)"
	}
	return "send: " + err.Error()
}

func (d *swarmDialog) refresh() {
	if d.snapshot == nil {
		d.rows = nil
		return
	}
	d.rows = d.snapshot()
	if d.cursor >= len(d.rows) {
		d.cursor = len(d.rows) - 1
	}
	if d.cursor < 0 {
		d.cursor = 0
	}
}

// HandleKey advances state. Returns (closed, statusMsg, statusErr).
// statusMsg / statusErr surface in the host's status bar so the user
// gets feedback on kill/remove without writing to the chat.
func (d *swarmDialog) HandleKey(k tui.Key) (closed bool, msg, errMsg string) {
	if !d.Active() {
		return false, "", ""
	}
	d.refresh()

	// Inline editors own key handling. They run before the other
	// modes so the user's typed letters aren't interpreted as 'k'
	// (kill), 'p' (prompt), etc.
	//
	// Transcript view is a special case: the editor is always on,
	// but we still need transcript-level keys (PageUp/PageDown/
	// Home/End scroll, Esc close) to take precedence over the
	// editor. handleViewingKey owns that routing and delegates
	// editor input to handlePromptKey itself.
	if d.viewing {
		return d.handleViewingKey(k)
	}
	if d.prompting {
		return d.handlePromptKey(k)
	}
	if d.pickingModel {
		return d.handleModelPickerKey(k)
	}
	if d.spawning {
		return d.handleSpawnKey(k)
	}

	switch k.Kind {
	case tui.KeyEsc:
		d.Close()
		return true, "", ""
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.rows)-1 {
			d.cursor++
		}
	case tui.KeyEnter:
		if len(d.rows) > 0 {
			d.viewing = true
			// Auto-open the inline editor (à la /btw) so the user
			// can start typing immediately. Skipped for non-running
			// agents; their inbox isn't listening, so an editor
			// would just frustrate them with bounced sends.
			if a := d.selected(); a != nil && a.Status == swarm.StatusRunning && d.send != nil {
				d.openPromptEditor(tui.Theme{}, a.ID)
			}
		}
	case tui.KeyRune:
		switch k.Rune {
		case 'q':
			d.Close()
			return true, "", ""
		case 'n':
			if d.spawn != nil {
				// Open the task editor directly. The model the new
				// agent will run against was already captured via
				// SetCurrentModel from the host's /model selection;
				// the user changes models the normal way (/model)
				// before opening /swarm.
				d.openSpawnEditor(tui.Theme{})
			}
		case 'p':
			if a := d.selected(); a != nil && d.send != nil {
				// Only running agents have a live inbox listener;
				// dialling the unix socket on a detached / done /
				// failed / killed agent fails with a long ECONNREFUSED
				// or ENOENT trail that's not actionable. Tell the user
				// what to do instead.
				if a.Status != swarm.StatusRunning {
					return false, "", promptDisabledHint(a.Status)
				}
				d.openPromptEditor(tui.Theme{}, a.ID)
			}
		case 'R':
			// Capital R for resume; lowercase r is already taken by
			// remove. Using a different keystroke avoids a confirm
			// modal for an action that's already destructive (resume
			// is reversible, so we don't need the shift-as-guard
			// dance lowercase r has historically warranted).
			if a := d.selected(); a != nil && d.resume != nil {
				if err := d.resume(a.ID); err != nil {
					return false, "", "resume: " + err.Error()
				}
				return false, "resumed " + a.ID, ""
			}
		case 'k':
			if a := d.selected(); a != nil && d.stop != nil {
				// Only running / pending agents have something to
				// cancel. Detached and already-terminal agents would
				// be silent no-ops at the Swarm layer; surface a hint
				// so the user knows kill did nothing and what to do
				// instead (remove via 'r').
				if a.Status != swarm.StatusRunning && a.Status != swarm.StatusPending {
					return false, "", killDisabledHint(a.Status)
				}
				if err := d.stop(a.ID); err != nil {
					return false, "", "stop: " + err.Error()
				}
				return false, "stopped " + a.ID, ""
			}
		case 'r':
			if a := d.selected(); a != nil && d.remove != nil {
				if err := d.remove(a.ID); err != nil {
					return false, "", "remove: " + err.Error()
				}
				return false, "removed " + a.ID, ""
			}
		}
	}
	return false, "", ""
}

// handleViewingKey routes one keystroke while the transcript view
// is on screen. The transcript view runs /btw-style: an inline
// editor is always pinned at the bottom, the transcript flows in
// full above it, and the host's outer scrollback handles overflow.
// There is no internal scroll, so this handler is small:
//
//   - Esc closes the transcript view (back to the dashboard list).
//     If the user was in the middle of an @-file-pick we let the
//     editor handler eat the Esc first so it just cancels the popup.
//
// Every other key is forwarded to handlePromptKey, which owns the
// editor itself (typed runes, Enter to submit, Backspace, file
// suggest, arrow keys for caret movement, etc.). Submits stay in
// the view and clear the editor for the next message instead of
// closing it — see handlePromptKey for the always-on behaviour.
func (d *swarmDialog) handleViewingKey(k tui.Key) (closed bool, msg, errMsg string) {
	a := d.selected()

	// Lazy-open the editor on first key into the transcript view, so
	// re-entering an agent after switching away picks up cleanly.
	// Skipped for non-running agents (their inbox isn't listening;
	// sending would just bounce).
	if a != nil && a.Status == swarm.StatusRunning && d.send != nil && !d.prompting {
		d.openPromptEditor(tui.Theme{}, a.ID)
	}

	// Esc closes the transcript view, regardless of whether the
	// editor is open. Handled here (not in the editor handler) so
	// the user gets a consistent "esc backs out one level" feel.
	// The file-suggest popup, if active, gets to eat the Esc first
	// inside handlePromptKey — we route there and check whether the
	// editor consumed it.
	if k.Kind == tui.KeyEsc {
		if d.prompting && d.promptSuggest != nil && d.promptEd != nil && d.promptSuggest.Active(d.promptEd.Value()) {
			// Let the editor's @-suggest popup handle the Esc to
			// dismiss itself instead of collapsing the whole view.
			return d.handlePromptKey(k)
		}
		d.viewing = false
		d.closePromptEditor()
		return false, "", ""
	}

	// Everything else — typed runes, Enter, Backspace, arrow keys
	// for caret movement, @-file picker — is editor input. If the
	// agent isn't running there's no live editor; surface a hint.
	if a == nil || a.Status != swarm.StatusRunning || d.send == nil {
		if k.Kind == tui.KeyRune && a != nil {
			return false, "", promptDisabledHint(a.Status)
		}
		return false, "", ""
	}
	return d.handlePromptKey(k)
}

// openSpawnEditor enters the inline new-agent prompt. It deliberately
// uses tui.Editor (not a one-line string) so swarm-new supports the same
// editing model as the normal input: multiline paste collapse, drag-drop
// file/dir chips, cursor movement, and the same accent-bar prompt style.
func (d *swarmDialog) openSpawnEditor(th tui.Theme) {
	d.spawning = true
	d.newTaskEd = tui.NewEditor(d.editorPrompt(th))
	d.fileSuggest = newFileSuggester()
	d.fileSuggest.SetCWD(d.cwd)
	if d.spawnDraft != "" {
		d.newTaskEd.SetValue(d.spawnDraft)
	}
}

// openModelPicker suspends the spawn editor and shows the same
// model dialog the global /model uses. Triggered when the user
// submits "/model" from inside the spawn editor; on select we
// capture the new pendingModel/Provider and reopen the spawn
// editor with the previously-typed task buffer restored.
func (d *swarmDialog) openModelPicker() {
	d.spawning = false
	d.newTaskEd = nil
	d.fileSuggest = nil
	d.pickingModel = true
	d.modelPicker = newModelDialog()
	d.modelPicker.Open(d.pendingModel, d.modelPickerLoggedIn)
}

// handleModelPickerKey forwards one keystroke to the embedded model
// picker and, on select or close, reopens the spawn editor so the
// user can continue composing the task. A selection captures the new
// pendingModel + pendingProvider; Esc leaves them unchanged (the
// user gets to keep whatever was inherited before they invoked
// /model).
func (d *swarmDialog) handleModelPickerKey(k tui.Key) (closed bool, msg, errMsg string) {
	if d.modelPicker == nil {
		d.openModelPicker()
	}
	act := d.modelPicker.HandleKey(k)
	switch {
	case act.Select:
		d.pendingModel = act.Model
		d.pendingProvider = act.Provider
		d.pickingModel = false
		d.modelPicker = nil
		d.openSpawnEditor(tui.Theme{})
		return false, "model: " + act.Model, ""
	case act.Close:
		d.pickingModel = false
		d.modelPicker = nil
		d.openSpawnEditor(tui.Theme{})
		return false, "", ""
	}
	return false, "", ""
}

// containsSlashModelLine reports whether any line in buf has trimmed
// content of "/model" (with optional trailing args). Used to decide
// whether to treat a spawn-editor submit as a model-picker command
// vs. as a task description.
func containsSlashModelLine(buf string) bool {
	for _, ln := range strings.Split(buf, "\n") {
		if isSlashModelLine(ln) {
			return true
		}
	}
	return false
}

// stripSlashModelLine removes the line containing "/model" (with
// optional trailing args) from the spawn-editor buffer so the
// remainder can be restored after the picker closes. The /model line
// is always exactly one full line in practice — the user typed
// /model<Enter> — but we tolerate inline placement at the start of
// a line just in case.
func isSlashModelLine(line string) bool {
	fields := strings.Fields(strings.TrimSpace(line))
	return len(fields) > 0 && strings.EqualFold(fields[0], "/model")
}

func stripSlashModelLine(buf string) string {
	lines := strings.Split(buf, "\n")
	out := lines[:0]
	for _, ln := range lines {
		if isSlashModelLine(ln) {
			continue
		}
		out = append(out, ln)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// openPromptEditor enters the inline "send a follow-up prompt" editor
// for the agent with id targetID. Mirrors openSpawnEditor so the
// editing experience (paste, @-picker, drop chips) is identical.
func (d *swarmDialog) openPromptEditor(th tui.Theme, targetID string) {
	d.prompting = true
	d.promptEd = tui.NewEditor(d.editorPrompt(th))
	d.promptSuggest = newFileSuggester()
	d.promptSuggest.SetCWD(d.cwd)
	d.promptTargetID = targetID
}

// handlePromptKey owns the keystrokes while the inline prompt editor
// is open. It mirrors handleSpawnKey but, on submit, invokes d.send
// with the captured target id instead of d.spawn. Esc cancels and
// returns the user to whichever view (list or transcript) was active
// when 'p' was pressed.
func (d *swarmDialog) handlePromptKey(k tui.Key) (closed bool, msg, errMsg string) {
	if d.promptEd == nil {
		d.openPromptEditor(tui.Theme{}, d.promptTargetID)
	}
	ed := d.promptEd
	fs := d.promptSuggest
	if fs != nil {
		fs.SetCWD(d.cwd)
	}

	if handled, errMsg := handleFilePickerKey(ed, fs, k); handled {
		return false, "", errMsg
	}

	// Esc cancels the prompt editor when the file picker isn't active.
	if k.Kind == tui.KeyEsc {
		d.closePromptEditor()
		return false, "", ""
	}

	// Tab-complete a path-like token before the editor sees the key,
	// matching the main editor's behaviour. Skipped above by the
	// @-picker branch when its popup is active.
	if k.Kind == tui.KeyTab {
		if tryPathTabCompleteEditor(ed, d.cwd) {
			return false, "", ""
		}
	}

	if submitted := ed.HandleKey(k); submitted {
		text, images, err := d.prepareFilePrompt(ed, strings.TrimRight(ed.SubmitValue(), "\n"))
		if err != nil {
			return false, "", "attachment: " + err.Error()
		}
		text = strings.TrimSpace(text)
		targetID := d.promptTargetID

		// Empty submit: nothing to send. If we're in transcript
		// view (always-on editor), just clear and stay; otherwise
		// (list-view-p flow) close the modal.
		if text == "" && len(images) == 0 {
			if d.viewing {
				ed.Clear()
				return false, "", ""
			}
			d.closePromptEditor()
			return false, "", ""
		}
		if len(images) == 0 && d.send == nil {
			return false, "", "send not wired"
		}
		if targetID == "" {
			if !d.viewing {
				d.closePromptEditor()
			}
			return false, "", "send: no target agent"
		}
		if len(images) > 0 {
			if d.sendWithImages == nil {
				return false, "", "image prompts are not supported by this Swarm adapter"
			}
			err = d.sendWithImages(targetID, text, images)
		} else {
			err = d.send(targetID, text)
		}
		if err != nil {
			return false, "", friendlySendErr(targetID, err)
		}

		// In transcript view (always-on editor) keep the editor
		// mounted and clear the buffer for the next message; in the
		// list-view modal flow, close it so the user returns to the
		// list. Either way, snap the transcript to the tail so the
		// user sees their just-sent message and the agent's reply.
		if d.viewing {
			ed.Clear()
		} else {
			d.closePromptEditor()
		}
		return false, "sent to " + targetID, ""
	}
	return false, "", ""
}

// closePromptEditor resets all prompt-editor state. Called on Esc and
// after submit so a subsequent 'p' starts from a clean slate.
func (d *swarmDialog) closePromptEditor() {
	d.prompting = false
	d.promptEd = nil
	d.promptSuggest = nil
	d.promptTargetID = ""
}

func (d *swarmDialog) prepareFilePrompt(ed *tui.Editor, raw string) (string, []provider.ImageBlock, error) {
	ctx := d.fileContext
	if ctx == nil {
		ctx = context.Background()
	}
	return prepareFilePrompt(ctx, d.cwd, raw, nil, selectedFilePaths(ed, raw, d.transferFiles), d.sandbox)
}

// handleSpawnKey owns the keystrokes while the inline new-agent editor
// is open. It mirrors the normal input's @ file-picker path and submit
// expansion, then asks the host to spawn.
func (d *swarmDialog) handleSpawnKey(k tui.Key) (closed bool, msg, errMsg string) {
	if d.newTaskEd == nil {
		d.openSpawnEditor(tui.Theme{})
	}
	ed := d.newTaskEd
	fs := d.fileSuggest
	if fs != nil {
		fs.SetCWD(d.cwd)
	}

	if handled, errMsg := handleFilePickerKey(ed, fs, k); handled {
		return false, "", errMsg
	}

	// Esc cancels the spawn prompt when the file picker isn't active.
	if k.Kind == tui.KeyEsc {
		d.spawning = false
		d.newTaskEd = nil
		d.fileSuggest = nil
		return false, "", ""
	}

	// Tab-complete a path-like token before the editor sees the key,
	// matching the main editor's behaviour. Skipped above by the
	// @-picker branch when its popup is active.
	if k.Kind == tui.KeyTab {
		if tryPathTabCompleteEditor(ed, d.cwd) {
			return false, "", ""
		}
	}

	if submitted := ed.HandleKey(k); submitted {
		raw := strings.TrimRight(ed.SubmitValue(), "\n")

		// In-editor /model command: when the buffer contains a line
		// whose trimmed content is "/model" (anywhere — typically on
		// its own line, but a trailing /model after a task works
		// too), pop the same picker the global /model uses, drop the
		// /model line, and preserve the rest of the buffer so the
		// user can keep composing once they've picked.
		if containsSlashModelLine(raw) {
			d.spawnDraft = stripSlashModelLine(raw)
			d.openModelPicker()
			return false, "", ""
		}

		task, images, err := d.prepareFilePrompt(ed, raw)
		if err != nil {
			return false, "", "attachment: " + err.Error()
		}
		task = strings.TrimSpace(task)
		model := d.pendingModel
		provider := d.pendingProvider
		if task != "" || len(images) > 0 {
			if len(images) > 0 {
				if d.spawnWithImages == nil {
					return false, "", "image prompts are not supported by this Swarm adapter"
				}
				err = d.spawnWithImages(task, model, provider, images)
			} else {
				if d.spawn == nil {
					return false, "", "spawn not wired"
				}
				err = d.spawn(task, model, provider)
			}
			if err != nil {
				return false, "", "spawn: " + err.Error()
			}
		}
		d.spawning = false
		d.newTaskEd = nil
		d.fileSuggest = nil
		d.spawnDraft = ""
		d.pendingModel = ""
		d.pendingProvider = ""
		if task == "" && len(images) == 0 {
			return false, "", ""
		}
		if model != "" {
			return false, "spawned (model " + model + ")", ""
		}
		return false, "spawned", ""
	}
	return false, "", ""
}

func (d *swarmDialog) selected() *swarm.AgentSnapshot {
	if d.cursor < 0 || d.cursor >= len(d.rows) {
		return nil
	}
	r := d.rows[d.cursor]
	return &r
}

// Render returns the dialog lines. Re-reads the snapshot each call so
// the dashboard updates as agents make progress.
func (d *swarmDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	d.refresh()

	if d.viewing {
		return d.renderTranscript(th, width)
	}

	out := []string{frameHeader(th, "swarm (n new, p prompt, R resume, ↑/↓ move, enter view, k kill, r remove, esc close)", width)}
	if d.prompting {
		return d.renderPromptEditor(th, width, out)
	}
	if d.pickingModel {
		// modelDialog renders its own frame; replace ours so the
		// picker takes the dashboard cleanly. Trailing hint reminds
		// the user Esc returns to the spawn editor.
		if d.modelPicker == nil {
			d.openModelPicker()
		}
		out = d.modelPicker.Render(th, width)
		out = append(out, "  "+th.FG256(th.Muted, "select model for next spawn, esc to cancel"))
		return out
	}
	if d.spawning {
		if d.newTaskEd == nil {
			d.openSpawnEditor(th)
		}
		if d.newTaskEd != nil {
			d.newTaskEd.Prompt = d.editorPrompt(th)
		}
		// One-line banner reminding the user which model the
		// agent-to-be will run against. Skipped when no model was
		// picked so the layout matches the historical look.
		if d.pendingModel != "" {
			label := "model: " + d.pendingModel
			if d.pendingProvider != "" {
				label += " (" + d.pendingProvider + ")"
			}
			out = append(out, "  "+th.FG256(th.Accent, label))
		}
		if d.fileSuggest != nil {
			d.fileSuggest.SetCWD(d.cwd)
			if popup := d.fileSuggest.Render(d.newTaskEd.Value(), th, width); len(popup) > 0 {
				out = append(out, popup...)
			}
		}
		// Breathing-room blank row above the editor so the input
		// doesn't sit flush against the frame header.
		out = append(out, "")
		if d.newTaskEd != nil {
			edLines, _, _ := d.newTaskEd.Render(width - 2)
			for _, l := range edLines {
				out = append(out, "  "+l)
			}
		}
		// Matching blank row below before the hint, mirroring the
		// main input's editor breathing room.
		out = append(out, "")
		out = append(out, "  "+th.FG256(th.Muted, "enter spawn, /model pick model, @ file/dir picker, !@ content, esc cancel"))
		out = append(out, frameRule(th, width))
		return out
	}
	if len(d.rows) == 0 {
		out = append(out, "  "+th.FG256(th.Muted, "no agents — press n to spawn one (or /swarm new <task>)"))
		out = append(out, frameRule(th, width))
		return out
	}

	// Column header for readability.
	header := fmt.Sprintf("  %-9s  %-26s  %-8s  %s", "STATUS", "ID", "AGE", "ACTIVITY")
	out = append(out, th.FG256(th.Muted, header))

	for i, r := range d.rows {
		row := formatSwarmRow(r, width-2)
		if i == d.cursor {
			out = append(out, th.PadHighlight("  "+row, width))
		} else {
			out = append(out, "  "+th.FG256(th.FG, row))
		}
	}
	out = append(out, frameRule(th, width))
	return out
}

func (d *swarmDialog) renderTranscript(th tui.Theme, width int) []string {
	a := d.selected()
	if a == nil {
		d.viewing = false
		return d.Render(th, width)
	}
	header := []string{
		frameHeader(th, "swarm: "+a.ID+"  (type to send, esc back)", width),
		"  " + th.FG256(th.Muted, "task:   "+a.Task),
		"  " + th.FG256(th.Muted, "dir:    "+a.Dir),
		"  " + th.FG256(th.Muted, fmt.Sprintf("status: %s, %s", a.Status, a.Activity)),
	}
	if a.Model != "" {
		modelLine := "model:  " + a.Model
		if a.Provider != "" {
			modelLine += " (" + a.Provider + ")"
		}
		header = append(header, "  "+th.FG256(th.Muted, modelLine))
	}
	if a.ConversationID != "" {
		header = append(header, "  "+th.FG256(th.Muted, "host:   conversation "+a.ConversationID))
	}
	if a.Err != "" {
		header = append(header, "  "+th.FG256(th.Muted, "error:  "+a.Err))
	}
	header = append(header,
		"",
		"  "+th.FG256(th.Muted, "── transcript ──"),
		"",
	)

	// Body: prefer the full Lines slice when present, fall back to
	// the Tail string for snapshots that only filled the summary.
	raw := a.Lines
	if len(raw) == 0 && a.Tail != "" {
		raw = strings.Split(a.Tail, "\n")
	}
	if len(raw) == 0 {
		header = append(header, "  "+th.FG256(th.Muted, "(no transcript yet)"))
		header = d.appendTranscriptEditor(header, th, width, a)
		header = append(header, frameRule(th, width))
		return header
	}

	// Re-render the flat log into proper chat blocks: user-bubble
	// rows for echoes, markdown-rendered prose for assistant turns,
	// tinted prefixes for stderr / errors. The result is a single
	// pre-styled, pre-indented []string of display rows ready to
	// hand to the host.
	//
	// We deliberately do NOT clip into a fixed-height viewport with
	// internal scroll — the transcript flows in full, /btw-style,
	// and the host's outer scrollback handles overflow naturally.
	// That gives the user the chat UX they expect (transcript grows
	// with the window, arrow keys belong to the editor, no "↑ N
	// more above" cliff).
	body := renderSwarmTranscriptBlocks(raw, th, width, d.compactMode)

	out := header
	out = append(out, body...)

	// /btw-style inline editor pinned to the bottom of the transcript.
	// Rendered after the body so the editor sits flush above the
	// frame rule. Only shown for running agents (others have no live
	// inbox listener — we surface a hint instead).
	out = d.appendTranscriptEditor(out, th, width, a)

	out = append(out, frameRule(th, width))
	return out
}

// appendTranscriptEditor adds the always-on inline composer to the
// transcript view, mimicking the /btw side-chat layout: a breathing
// row, the editor itself, another breathing row, and a single muted
// hint. For non-running agents we render a short "can't send while
// X" note instead so the user knows why typing is disabled.
func (d *swarmDialog) appendTranscriptEditor(out []string, th tui.Theme, width int, a *swarm.AgentSnapshot) []string {
	if a == nil {
		return out
	}
	if a.Status != swarm.StatusRunning || d.send == nil {
		out = append(out, "")
		out = append(out, "  "+th.FG256(th.Muted, promptDisabledHint(a.Status)))
		return out
	}
	// Lazy-init the editor here too so the very first render of the
	// transcript view (before the user has pressed any key) already
	// shows the composer. Mirrors openPromptEditor's setup but uses
	// the live theme so the accent bar matches.
	if d.promptEd == nil {
		d.openPromptEditor(th, a.ID)
	}
	if d.promptEd != nil {
		d.promptEd.Prompt = d.editorPrompt(th)
	}
	if d.promptSuggest != nil {
		d.promptSuggest.SetCWD(d.cwd)
		if popup := d.promptSuggest.Render(d.promptEd.Value(), th, width); len(popup) > 0 {
			out = append(out, popup...)
		}
	}

	// Busy-spinner row, /btw-style. Shown whenever the agent's
	// current activity isn't "idle" — so "thinking", "tool: foo",
	// "starting", etc. all animate. The spinner is stopped (and the
	// row hidden) once the agent reports idle, mirroring the main
	// chat's busy line.
	if agentIsBusy(a) {
		if d.transcriptSpin == nil {
			d.transcriptSpin = newSpinner(th)
			d.transcriptSpin.Start()
		} else {
			d.transcriptSpin.Configure(th)
		}
		out = append(out, "")
		prefix := fmt.Sprintf("%s %s, %s",
			th.FG256(th.Assistant, d.transcriptSpin.Frame()),
			th.FG256(th.Assistant, a.Activity),
			th.FG256(th.Muted, d.transcriptSpin.Elapsed().String()),
		)
		out = append(out, "  "+prefix)
	} else {
		// Reset so the next busy phase starts fresh (rotating
		// funny-line cycles back to the start, elapsed restarts).
		d.transcriptSpin = nil
	}

	out = append(out, "")
	edLines, _, _ := d.promptEd.Render(width - 2)
	for _, l := range edLines {
		out = append(out, "  "+l)
	}
	out = append(out, "")
	out = append(out, "  "+th.FG256(th.Muted, "enter send, @ file/dir picker, !@ content, esc back"))
	return out
}

// agentIsBusy reports whether the agent is currently mid-turn and
// should render a spinner. Conservative: only known idle markers
// suppress the spinner so transient / unfamiliar activity strings
// still animate. Detached / done / failed / killed never animate —
// they aren't actively producing output.
func agentIsBusy(a *swarm.AgentSnapshot) bool {
	if a == nil {
		return false
	}
	if a.Status != swarm.StatusRunning && a.Status != swarm.StatusPending {
		return false
	}
	switch strings.TrimSpace(a.Activity) {
	case "", "idle":
		return false
	}
	return true
}

// renderSwarmTranscriptBlocks converts the agent's flat transcript
// log into a slice of display rows styled like the main chat: user
// echoes in bubble rows, assistant prose through tui.RenderMarkdown,
// stderr / error chatter tinted. Each output row is fully indented
// and ready for the caller to slice into a scroll window.
//
// The flat log mixes role markers and raw text, so we group adjacent
// lines of the same role into one logical "block" and render the
// whole block together — essential for markdown, which needs the
// full message body in one piece to detect fenced code spans,
// bullet lists, and so on.
func renderSwarmTranscriptBlocks(lines []string, th tui.Theme, width int, compactMode bool) []string {
	type blockKind int
	const (
		kindAssistant blockKind = iota
		kindUser
		kindStderr
		kindError
	)
	type block struct {
		kind blockKind
		body []string // raw lines, role prefix already stripped
	}

	// classify maps one raw line to its role and the stripped body.
	classify := func(s string) (blockKind, string) {
		switch {
		case strings.HasPrefix(s, "user: "):
			return kindUser, strings.TrimPrefix(s, "user: ")
		case strings.HasPrefix(s, "stderr: "):
			return kindStderr, strings.TrimPrefix(s, "stderr: ")
		case strings.HasPrefix(s, "error: "):
			return kindError, strings.TrimPrefix(s, "error: ")
		default:
			return kindAssistant, s
		}
	}

	// Coalesce consecutive same-kind lines into blocks. Empty lines
	// keep the kind of the previous block so blank lines inside an
	// assistant message stay attached for markdown rendering (an
	// empty line between paragraphs is significant in markdown).
	var blocks []block
	for _, ln := range lines {
		if ln == "" {
			// Empty lines stay attached to the previous block so
			// markdown sees paragraph breaks inside an assistant
			// turn. Standalone empties before any block are dropped
			// (nothing meaningful to attach them to).
			if len(blocks) > 0 {
				blocks[len(blocks)-1].body = append(blocks[len(blocks)-1].body, "")
			}
			continue
		}
		k, stripped := classify(ln)
		if len(blocks) == 0 || blocks[len(blocks)-1].kind != k {
			blocks = append(blocks, block{kind: k})
		}
		blocks[len(blocks)-1].body = append(blocks[len(blocks)-1].body, stripped)
	}

	// Render each block. Width budget mirrors the btw dialog: the
	// transcript text sits inside two leading frame cells, so the
	// inner width is width-4 for markdown and width-2 for the
	// user-bubble (the bubble already pads itself).
	var out []string
	bubbleWidth := width - 2
	if bubbleWidth < 10 {
		bubbleWidth = 10
	}

	for i, b := range blocks {
		if i > 0 {
			out = append(out, "") // breathing room between turns
		}
		switch b.kind {
		case kindUser:
			text := strings.Join(b.body, "\n")
			out = append(out, btwUserBubbleRows(th, text, bubbleWidth, compactMode)...)
		case kindAssistant:
			text := strings.Join(b.body, "\n")
			out = append(out, renderDialogMarkdownRows(strings.TrimLeft(text, "\n"), th, width)...)
		case kindStderr:
			for _, line := range b.body {
				out = append(out, wrapDialogTextRows(th.FG256(th.Muted, "stderr  "+line), width)...)
			}
		case kindError:
			for _, line := range b.body {
				out = append(out, wrapDialogTextRows(th.FG256(th.Error, "✖ "+line), width)...)
			}
		}
	}
	return out
}

// renderPromptEditor draws the inline "send a follow-up prompt"
// composer. Reuses the editor + file-suggest popup styling of the
// spawn editor so the two flows are visually consistent. `out` is
// the pre-built header (frame header for list view, or the agent
// metadata block for transcript view); we append onto it and close
// with the standard frame rule.
func (d *swarmDialog) renderPromptEditor(th tui.Theme, width int, out []string) []string {
	if d.promptEd == nil {
		d.openPromptEditor(th, d.promptTargetID)
	}
	if d.promptEd != nil {
		d.promptEd.Prompt = d.editorPrompt(th)
	}
	target := d.promptTargetID
	if target == "" {
		target = "<unknown>"
	}
	out = append(out, "  "+th.FG256(th.Muted, "send to "+target+":"))
	if d.promptSuggest != nil {
		d.promptSuggest.SetCWD(d.cwd)
		if popup := d.promptSuggest.Render(d.promptEd.Value(), th, width); len(popup) > 0 {
			out = append(out, popup...)
		}
	}
	// Breathing-room blank row above the editor.
	out = append(out, "")
	if d.promptEd != nil {
		edLines, _, _ := d.promptEd.Render(width - 2)
		for _, l := range edLines {
			out = append(out, "  "+l)
		}
	}
	// Matching blank row below before the hint.
	out = append(out, "")
	out = append(out, "  "+th.FG256(th.Muted, "enter send, @ file/dir picker, !@ content, esc cancel"))
	out = append(out, frameRule(th, width))
	return out
}

// formatSwarmRow is the one-line summary shown per agent.
//
// Layout (fixed-width columns, then free-form activity):
//
//	STATUS    ID                          AGE       ACTIVITY
//	● run     fix-login-12345             3m        editing main.go
//	✓ done    write-tests-67890           1h        done
func formatSwarmRow(r swarm.AgentSnapshot, maxWidth int) string {
	status := statusLabel(r.Status)
	age := formatAge(r.Started)
	left := fmt.Sprintf("%-9s  %-26s  %-8s  ", status, truncateLineSafe(r.ID, 26), age)
	room := maxWidth - len([]rune(left))
	if room < 10 {
		room = 10
	}
	act := strings.ReplaceAll(r.Activity, "\n", " ")
	if act == "" {
		act = r.Task
	}
	if len([]rune(act)) > room {
		act = string([]rune(act)[:room-3]) + "..."
	}
	row := left + act
	rowRunes := []rune(row)
	if len(rowRunes) > maxWidth {
		if maxWidth <= 3 {
			row = strings.Repeat(".", maxWidth)
		} else {
			row = string(rowRunes[:maxWidth-3]) + "..."
		}
	}
	return row
}

// statusLabel returns a short, padded status badge. Kept stable across
// runs (no colours embedded) so tests can match it deterministically.
func statusLabel(s swarm.Status) string {
	switch s {
	case swarm.StatusPending:
		return "● pend"
	case swarm.StatusRunning:
		return "● run"
	case swarm.StatusDone:
		return "✓ done"
	case swarm.StatusFailed:
		return "✗ fail"
	case swarm.StatusKilled:
		return "■ kill"
	case swarm.StatusDetached:
		return "○ detach"
	}
	return string(s)
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
