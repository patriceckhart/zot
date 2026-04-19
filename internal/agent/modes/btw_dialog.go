package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/tui"
)

// btwTurn is one user/assistant pair within a side chat. Kept
// separate from the main transcript so closing the dialog leaves
// the main session untouched.
type btwTurn struct {
	User      string
	Assistant string
	Err       string
}

// btwDialog is the side-chat overlay opened by /btw. It shows the
// user's question, runs a one-off model call against the live
// snapshot of the main session plus any prior side-chat turns,
// renders the assistant reply through the markdown pipeline, and
// keeps the main transcript completely untouched.
//
// Cancellation: esc cancels an in-flight call when one is running,
// otherwise closes the dialog.
type btwDialog struct {
	mu      sync.Mutex
	active  bool
	turns   []btwTurn
	editor  *tui.Editor
	loading bool
	cancel  context.CancelFunc

	// spin drives the same braille animation + rotating funny-line
	// shown in the main status bar. Owned by the dialog so its clock
	// is independent of the main spinner (so re-opening the dialog
	// always starts fresh and the message doesn't carry over from a
	// completed main turn).
	spin *spinner

	// Frozen at Open() time so the side-chat keeps a stable view of
	// the main thread even if a turn happens to land on the main
	// agent while the dialog is open (rare but possible).
	frozenSystem string
	frozenMsgs   []provider.Message

	// Provider details captured at open time; used by send() to
	// build the request without going back through the agent.
	client provider.Client
	model  string

	// Theme cached so render() doesn't need it threaded through.
	theme tui.Theme
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

// Open enters the side chat. agent supplies the live transcript and
// system prompt, plus the underlying provider client to use for the
// one-off completion. seed is an optional first question that gets
// auto-submitted (so /btw <text> behaves like the reference).
func (d *btwDialog) Open(th tui.Theme, agent *core.Agent, system, model, seed string) {
	d.mu.Lock()
	d.active = true
	d.theme = th
	d.turns = nil
	d.loading = false
	d.cancel = nil
	d.editor = tui.NewEditor(th.FG256(th.Accent, "▌ "))
	d.frozenSystem = system
	d.frozenMsgs = agent.Messages()
	d.client = agent.Client
	d.model = model
	d.mu.Unlock()

	if seed = strings.TrimSpace(seed); seed != "" {
		d.editor.SetValue(seed)
		d.submit()
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
	d.frozenMsgs = nil
	d.client = nil
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
	d.mu.Unlock()
	if editor == nil {
		return false
	}
	// Don't accept new submissions while a call is in flight; arrow
	// keys / scrolling still flow through to the editor for caret
	// movement and history.
	submitted := editor.HandleKey(k)
	invalidate()
	if submitted && !loading {
		d.submit()
	}
	return false
}

// submit fires the LLM call for the current input and, on success,
// appends a new turn to d.turns.
func (d *btwDialog) submit() {
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
		d.spin = newSpinner()
	}
	d.spin.Start()
	d.turns = append(d.turns, btwTurn{User: question})
	turnIdx := len(d.turns) - 1

	// Build the request: system + frozen main transcript + every
	// prior side-chat turn (user + assistant) + this question.
	msgs := append([]provider.Message(nil), d.frozenMsgs...)
	for i, t := range d.turns {
		msgs = append(msgs, provider.Message{
			Role: provider.RoleUser,
			Content: []provider.Content{
				provider.TextBlock{Text: t.User},
			},
			Time: time.Now(),
		})
		// Only completed turns contribute an assistant reply; the
		// in-flight one (turnIdx) hasn't got one yet.
		if i < turnIdx && t.Assistant != "" {
			msgs = append(msgs, provider.Message{
				Role: provider.RoleAssistant,
				Content: []provider.Content{
					provider.TextBlock{Text: t.Assistant},
				},
				Time: time.Now(),
			})
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	client := d.client
	model := d.model
	system := d.frozenSystem
	d.mu.Unlock()

	go func() {
		req := provider.Request{
			Model:    model,
			System:   system,
			Messages: msgs,
			// No tools: side chat is conversational, not agentic.
		}
		stream, err := client.Stream(ctx, req)
		if err != nil {
			d.completeTurn(turnIdx, "", err.Error())
			return
		}

		var reply strings.Builder
		var finalErr error
		for ev := range stream {
			switch e := ev.(type) {
			case provider.EventTextDelta:
				reply.WriteString(e.Delta)
			case provider.EventDone:
				if e.Err != nil {
					finalErr = e.Err
				}
			}
		}

		errMsg := ""
		if finalErr != nil && ctx.Err() == nil {
			errMsg = finalErr.Error()
		}
		d.completeTurn(turnIdx, reply.String(), errMsg)
	}()
}

// completeTurn fills in the assistant text or error for the turn at
// idx and clears the loading state.
func (d *btwDialog) completeTurn(idx int, assistant, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if idx < 0 || idx >= len(d.turns) {
		return
	}
	d.turns[idx].Assistant = assistant
	d.turns[idx].Err = errMsg
	d.loading = false
	d.cancel = nil
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
	out = append(out, frameHeaderColor(th, "btw — side chat (esc closes; nothing is added to the main thread)", width, th.Accent))

	if len(d.turns) == 0 && !d.loading {
		out = append(out, "  "+th.FG256(th.Muted, "ask anything; replies stay private to this side chat."))
	}

	for _, t := range d.turns {
		out = append(out, "")
		out = append(out, "  "+th.FG256(th.User, "▌ you"))
		for _, line := range strings.Split(t.User, "\n") {
			out = append(out, "    "+th.FG256(th.Muted, line))
		}
		if t.Assistant != "" {
			out = append(out, "")
			out = append(out, "  "+th.FG256(th.Assistant, "▌ zot"))
			md := tui.RenderMarkdown(t.Assistant, th, width-4)
			for _, line := range strings.Split(md, "\n") {
				out = append(out, "    "+line)
			}
		}
		if t.Err != "" {
			out = append(out, "    "+th.FG256(th.Error, "✖ "+t.Err))
		}
	}

	if d.loading && d.spin != nil {
		out = append(out, "")
		// Match the main chat busy prefix shape: spinner glyph,
		// rotating funny-line, elapsed seconds, then a muted hint
		// that esc cancels.
		prefix := fmt.Sprintf("%s %s · %s",
			th.FG256(th.Spinner, d.spin.Frame()),
			th.FG256(th.Spinner, d.spin.Message()),
			th.FG256(th.Muted, d.spin.Elapsed().String()),
		)
		out = append(out, "  "+prefix+"  "+th.FG256(th.Muted, "(esc cancels)"))
	}

	out = append(out, "")
	if d.editor != nil {
		edLines, _, _ := d.editor.Render(width)
		for _, l := range edLines {
			// Indent the editor body so it lines up with the side-chat
			// content column. Editor's prompt already includes a left
			// marker, so just two cells of pad.
			out = append(out, "  "+l)
		}
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
	// Reproduce render's structure to find where the editor sits:
	// header (1) + (turns_section_lines) + (loading?) + blank (1) + editor
	editorOffset := 1 // header
	if len(d.turns) == 0 && !d.loading {
		editorOffset++ // muted "ask anything..." line
	}
	for _, t := range d.turns {
		editorOffset++ // blank
		editorOffset++ // "you" header
		editorOffset += len(strings.Split(t.User, "\n"))
		if t.Assistant != "" {
			editorOffset++ // blank
			editorOffset++ // "zot" header
			editorOffset += len(strings.Split(tui.RenderMarkdown(t.Assistant, d.theme, width-4), "\n"))
		}
		if t.Err != "" {
			editorOffset++
		}
	}
	if d.loading {
		editorOffset++ // blank
		editorOffset++ // spinner line
	}
	editorOffset++ // pre-editor blank
	_, eRow, eCol := d.editor.Render(width - 2)
	return editorOffset + eRow, eCol + 2 /* matches render indent */
}

// errMessage is a tiny helper for the future when we want to surface
// retryable failures in a styled way.
func errMessage(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("error: %s", err.Error())
}
