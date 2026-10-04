package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

// btwTurn is one user/assistant pair within a side chat. Kept
// separate from the main transcript so closing the dialog leaves
// the main session untouched.
type btwTurn struct {
	User      string
	Assistant string
	Tools     []tui.ToolCallView
	Err       string
}

// btwDialog is the side-chat overlay opened by /btw. It shows the
// user's question, runs an isolated agent turn against the live
// snapshot of the main session plus any prior side-chat turns,
// renders the assistant reply through the markdown pipeline, and
// keeps the main transcript completely untouched.
//
// Cancellation: esc cancels an in-flight call when one is running,
// otherwise closes the dialog.
type btwDialog struct {
	mu          sync.Mutex
	active      bool
	turns       []btwTurn
	editor      *tui.Editor
	loading     bool
	compactMode bool
	lineInput   bool
	cancel      context.CancelFunc

	// spin drives the same braille animation + rotating funny-line
	// shown in the main status bar. Owned by the dialog so its clock
	// is independent of the main spinner (so re-opening the dialog
	// always starts fresh and the message doesn't carry over from a
	// completed main turn).
	spin *spinner

	// sideAgent owns an isolated transcript seeded from the main
	// conversation. It uses the same tools and runtime hooks as the
	// main agent, but has no persistence callbacks.
	sideAgent *core.Agent

	// Theme and toolView are cached so render() uses the same tool-call
	// presentation and collapse behavior as the main transcript.
	theme    tui.Theme
	toolView *tui.View

	// cwd is the working directory used to resolve relative paths
	// when the user presses Tab on a path-like token in the side-
	// chat editor. Set by Open() from the host's cwd so the same
	// path-completion that works in the main editor also works
	// here.
	cwd string
}

func newBtwDialog() *btwDialog {
	return &btwDialog{}
}

// Active reports whether the dialog is visible and consuming keys.
func (d *btwDialog) Active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}

// Loading reports whether the dialog is currently awaiting a
// model response (and therefore rendering an animated spinner).
// Used by the host to decide whether a periodic redraw is worth
// triggering; when false and the user is just typing, we can
// skip the tick and let the terminal drive the cursor blink.
func (d *btwDialog) Loading() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active && d.loading
}

// SetToolPreview attaches the side-effect-free confirmation preview to the
// matching side-chat tool box before execution begins.
func (d *btwDialog) SetToolPreview(id, summary, content string) bool {
	if d == nil || id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for turnIdx := range d.turns {
		if tool := findBtwTool(&d.turns[turnIdx], id); tool != nil {
			tool.Args = summary
			tool.Preview = content
			return true
		}
	}
	return false
}

// ToggleToolExpansion expands or collapses long tool results in the side chat.
func (d *btwDialog) ToggleToolExpansion() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.toolView != nil {
		d.toolView.ExpandAll = !d.toolView.ExpandAll
	}
}

// Open enters the side chat. agent supplies the live transcript and
// system prompt, plus the underlying provider client to use for the
// one-off completion. seed is an optional first question that gets
// auto-submitted (so /btw <text> starts a conversation right away).
// invalidate, if non-nil, is called after each state change so the
// host redraw loop can pick up the update without polling.
func (d *btwDialog) Open(th tui.Theme, agent *core.Agent, system, model, cwd, seed string, compactMode, flatTools bool, lineInput bool, invalidate func()) {
	d.mu.Lock()
	d.active = true
	d.theme = th
	d.compactMode = compactMode
	d.lineInput = lineInput
	d.toolView = &tui.View{Theme: th, CompactMode: compactMode, FlatTools: flatTools}
	d.turns = nil
	d.loading = false
	d.cancel = nil
	prompt := th.AccentBar(th.Accent)
	if lineInput {
		prompt = ""
	}
	d.editor = tui.NewEditor(prompt)
	d.sideAgent = newBtwAgent(agent, system, model)
	d.cwd = cwd
	d.mu.Unlock()

	if seed = strings.TrimSpace(seed); seed != "" {
		d.editor.SetValue(seed)
		d.submit(invalidate)
	}
}

// Close hides the dialog. Cancels any in-flight request.
func (d *btwDialog) Close() {
	d.mu.Lock()
	d.active = false
	d.turns = nil
	d.editor = nil
	d.loading = false
	cancel := d.cancel
	d.cancel = nil
	d.sideAgent = nil
	d.toolView = nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// HandleKey routes a keypress to the dialog. Returns true if the
// dialog wants the event consumed (always true while active, except
// for the special closing case where the caller may want to signal
// the parent).
func (d *btwDialog) HandleKey(k tui.Key, invalidate func()) (closed bool) {
	if !d.Active() {
		return false
	}
	switch k.Kind {
	case tui.KeyEsc:
		// First esc: cancel an in-flight call. Subsequent esc closes.
		d.mu.Lock()
		busy := d.loading
		cancel := d.cancel
		d.mu.Unlock()
		if busy && cancel != nil {
			cancel()
			return false
		}
		d.Close()
		invalidate()
		return true
	}

	d.mu.Lock()
	editor := d.editor
	loading := d.loading
	cwd := d.cwd
	d.mu.Unlock()
	if editor == nil {
		return false
	}
	// Tab-complete a path-like token before the editor sees the key,
	// matching the main editor's behaviour.
	if k.Kind == tui.KeyTab {
		if tryPathTabCompleteEditor(editor, cwd) {
			invalidate()
			return false
		}
	}
	// Don't accept new submissions while a call is in flight; arrow
	// keys / scrolling still flow through to the editor for caret
	// movement and history.
	submitted := editor.HandleKey(k)
	invalidate()
	if submitted && !loading {
		d.submit(invalidate)
	}
	return false
}

// submit fires the LLM call for the current input and, on success,
// appends a new turn to d.turns. invalidate is called every time
// the turn's visible state changes (text delta, error, complete)
// so the host redraw loop picks up the update without relying on
// a periodic tick.
func (d *btwDialog) submit(invalidate func()) {
	d.mu.Lock()
	if d.editor == nil || d.loading {
		d.mu.Unlock()
		return
	}
	question := strings.TrimSpace(d.editor.Value())
	if question == "" {
		d.mu.Unlock()
		return
	}
	d.editor.Clear()
	d.loading = true
	if d.spin == nil {
		d.spin = newSpinner(d.theme)
	} else {
		d.spin.Configure(d.theme)
	}
	d.spin.Start()
	d.turns = append(d.turns, btwTurn{User: question})
	turnIdx := len(d.turns) - 1

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	agent := d.sideAgent
	d.mu.Unlock()

	go func() {
		err := agent.Prompt(ctx, question, nil, func(ev core.AgentEvent) {
			d.handleAgentEvent(turnIdx, ev)
			if invalidate != nil {
				invalidate()
			}
		})
		errMsg := ""
		if err != nil && ctx.Err() == nil {
			errMsg = err.Error()
		}
		d.completeTurn(turnIdx, errMsg)
		if invalidate != nil {
			invalidate()
		}
	}()
}

func newBtwAgent(main *core.Agent, system, model string) *core.Agent {
	agent := core.NewAgent(main.Client, model, system, main.Tools)
	agent.MaxSteps = main.MaxSteps
	agent.Reasoning = main.Reasoning
	agent.Temperature = main.Temperature
	agent.MaxTokens = main.MaxTokens
	agent.BeforeToolExecute = main.BeforeToolExecute
	agent.BeforeToolExecuteContext = main.BeforeToolExecuteContext
	agent.BeforeTurn = main.BeforeTurn
	agent.BeforeAssistantMessage = main.BeforeAssistantMessage
	agent.MaxRetries = main.MaxRetries
	agent.RetryBaseDelay = main.RetryBaseDelay
	agent.OnEvent = main.OnEvent
	agent.SetMessages(main.Messages())
	return agent
}

func (d *btwDialog) handleAgentEvent(turnIdx int, ev core.AgentEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if turnIdx < 0 || turnIdx >= len(d.turns) {
		return
	}
	turn := &d.turns[turnIdx]
	switch e := ev.(type) {
	case core.EvTextDelta:
		turn.Assistant += e.Delta
	case core.EvToolUseStart:
		turn.Tools = append(turn.Tools, tui.ToolCallView{ID: e.ID, Name: e.Name, Streaming: true})
	case core.EvToolUseArgs:
		if tool := findBtwTool(turn, e.ID); tool != nil {
			tool.RawJSONBuf += e.Delta
			if path, ok, _ := tui.ExtractPartialStringField(tool.RawJSONBuf, "path"); ok {
				tool.LivePath = path
			} else if path, ok, _ := tui.ExtractPartialStringField(tool.RawJSONBuf, "file_path"); ok {
				tool.LivePath = path
			}
		}
	case core.EvToolUseEnd:
		if tool := findBtwTool(turn, e.ID); tool != nil {
			tool.Streaming = false
		}
	case core.EvToolCall:
		tool := findBtwTool(turn, e.ID)
		if tool == nil {
			turn.Tools = append(turn.Tools, tui.ToolCallView{ID: e.ID, Name: e.Name})
			tool = &turn.Tools[len(turn.Tools)-1]
		}
		tool.Name = e.Name
		tool.Args = tui.ShortArgs(e.Name, e.Args)
		if tool.RawJSONBuf == "" {
			tool.RawJSONBuf = string(e.Args)
		}
		tool.Streaming = false
	case core.EvToolResult:
		if tool := findBtwTool(turn, e.ID); tool != nil {
			tool.Done = true
			tool.Error = e.Result.IsError
			tool.ResultContent = nil
			var text strings.Builder
			for _, content := range e.Result.Content {
				if block, ok := content.(provider.TextBlock); ok {
					tool.ResultContent = append(tool.ResultContent, block)
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(block.Text)
				}
			}
			tool.Result = text.String()
		}
	}
}

func findBtwTool(turn *btwTurn, id string) *tui.ToolCallView {
	for j := range turn.Tools {
		if turn.Tools[j].ID == id {
			return &turn.Tools[j]
		}
	}
	return nil
}

// completeTurn records an error, if any, and clears the loading state.
func (d *btwDialog) completeTurn(idx int, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if idx < 0 || idx >= len(d.turns) {
		return
	}
	d.turns[idx].Err = errMsg
	d.loading = false
	d.cancel = nil
}

func renderBtwConfirmation(th tui.Theme, width int, btw *btwDialog, confirm *confirmDialog) []string {
	rows := padDialogFrame(btw.Render(th, width))
	rows = append(rows, "")
	return append(rows, padDialogFrame(confirm.Render(th, width))...)
}

// Render returns the side-chat panel lines. Called every frame
// while active.
func (d *btwDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active {
		return nil
	}

	var out []string
	out = append(out, frameHeaderColor(th, "btw - side chat (esc closes; nothing is added to the main thread)", width, th.Accent))

	if len(d.turns) == 0 && !d.loading {
		out = append(out, "  "+th.FG256(th.Muted, "ask anything; replies stay private to this side chat."))
	}

	for _, t := range d.turns {
		out = append(out, "")
		out = append(out, btwUserBubbleRows(th, t.User, width-2, d.compactMode)...)
		if len(t.Tools) > 0 {
			// Match the main transcript's inter-message separator between
			// the user bubble and the first tool box.
			out = append(out, "")
		}
		for _, tool := range t.Tools {
			out = append(out, d.toolView.RenderToolCall(tool, width)...)
		}
		if t.Assistant != "" {
			out = append(out, "")
			out = append(out, renderDialogMarkdownRows(t.Assistant, th, width)...)
		}
		if t.Err != "" {
			out = append(out, wrapDialogTextRows(th.FG256(th.Error, "✖ "+t.Err), width)...)
		}
	}

	if d.loading && d.spin != nil {
		out = append(out, "")
		// Match the main chat busy prefix shape: spinner glyph,
		// rotating funny-line, elapsed seconds, then a muted hint
		// that esc cancels.
		prefix := fmt.Sprintf("%s %s - %s",
			th.FG256(th.Assistant, d.spin.Frame()),
			th.FG256(th.Assistant, d.spin.Message()),
			th.FG256(th.Muted, d.spin.Elapsed().String()),
		)
		out = append(out, "  "+prefix+"  "+th.FG256(th.Muted, "(esc cancels)"))
	}

	out = append(out, "")
	if d.editor != nil {
		// Render at width-2 to match the two-cell left indent applied
		// below. CursorPos uses the same width so the reported cursor
		// column matches the wrapped layout shown on screen.
		d.editor.Prompt = th.AccentBar(th.Accent)
		if d.lineInput {
			d.editor.Prompt = ""
		}
		edLines, _, _ := d.editor.Render(width - 2)
		for _, l := range edLines {
			// Indent the editor body so it lines up with the side-chat
			// content column. Editor's prompt already includes a left
			// marker, so just two cells of pad.
			out = append(out, "  "+l)
		}
		out = append(out, "") // breathing room between editor and frame rule
	}
	out = append(out, frameRuleColor(th, width, th.Accent))
	return out
}

// CursorRow / CursorCol report where the dialog wants the terminal
// cursor placed within its render output, so the parent can position
// the actual terminal cursor on the editor input. Returns (-1, -1)
// when the dialog isn't active or has no editor.
func (d *btwDialog) CursorPos(width int) (row, col int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active || d.editor == nil {
		return -1, -1
	}
	// Reproduce render's structure to find where the editor sits.
	// Note: the parent (interactive.go) wraps every dialog with
	// padDialogFrame. It only injects a blank row after the frame
	// header when Render did not already put one there. With existing
	// turns or a loading spinner, Render's first body row is already
	// blank, so counting an extra pad row would place the cursor one
	// row too low.
	editorOffset := 1 // header
	if len(d.turns) == 0 && !d.loading {
		editorOffset++ // padDialogFrame's post-header blank
		editorOffset++ // muted "ask anything..." line
	}
	for _, t := range d.turns {
		editorOffset++ // blank
		editorOffset += len(btwUserBubbleRows(d.theme, t.User, width-2, d.compactMode))
		if len(t.Tools) > 0 {
			editorOffset++ // main transcript's user-to-tool separator
		}
		for _, tool := range t.Tools {
			editorOffset += len(d.toolView.RenderToolCall(tool, width))
		}
		if t.Assistant != "" {
			editorOffset++ // blank
			editorOffset += len(renderDialogMarkdownRows(t.Assistant, d.theme, width))
		}
		if t.Err != "" {
			editorOffset += len(wrapDialogTextRows(d.theme.FG256(d.theme.Error, "✖ "+t.Err), width))
		}
	}
	if d.loading {
		editorOffset++ // blank
		editorOffset++ // spinner line
	}
	editorOffset++ // pre-editor blank
	d.editor.Prompt = d.theme.AccentBar(d.theme.Accent)
	if d.lineInput {
		d.editor.Prompt = ""
	}
	_, eRow, eCol := d.editor.Render(width - 2)
	return editorOffset + eRow, eCol + 2 /* matches render indent */
}

// btwUserBubbleRows renders a user message inside the /btw dialog
// using the same bubble layout the main chat uses (full-width tinted
// panel, left-edge ▌ bar, padding rows above and below). The frame
// padding is the caller's job; bubbleWidth is the available row
// width inside the frame.
func btwUserBubbleRows(th tui.Theme, text string, bubbleWidth int, compactMode bool) []string {
	const leftGutter = 0
	const rightGutter = 2
	innerWidth := bubbleWidth - 2 - leftGutter - rightGutter // 2 = bar's two cells
	if innerWidth < 1 {
		innerWidth = 1
	}
	bar := th.BG(th.UserBubbleBG, th.FG256(th.Accent, "▌ "))
	row := func(content string) string {
		inner := strings.Repeat(" ", leftGutter) + content
		return "  " + bar + th.UserBubble(inner, bubbleWidth-2)
	}
	if compactMode {
		innerWidth = bubbleWidth - 2
		if innerWidth < 1 {
			innerWidth = 1
		}
		row = func(content string) string {
			return "  " + th.FG256(th.Accent, "▌ ") + th.FG256(th.Muted, content)
		}
	}
	var bubble []string
	for _, l := range strings.Split(text, "\n") {
		for _, w := range tui.WrapANSILine(l, innerWidth) {
			bubble = append(bubble, row(w))
		}
	}
	if len(bubble) == 0 {
		return nil
	}
	if compactMode {
		return bubble
	}
	out := make([]string, 0, len(bubble)+2)
	out = append(out, row(""))
	out = append(out, bubble...)
	out = append(out, row(""))
	return out
}

// errMessage is a tiny helper for the future when we want to surface
// retryable failures in a styled way.
func errMessage(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("error: %s", err.Error())
}
