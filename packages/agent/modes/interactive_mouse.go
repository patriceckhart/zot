package modes

import (
	"context"

	"github.com/patriceckhart/zot/packages/tui"
)

// Optional to keep existing SettingsStore implementations compatible.
type clickToPositionSettingsStore interface {
	SetTUIClickToPosition(bool) error
}

func (i *Interactive) applyClickToPosition(enabled bool) {
	if store, ok := i.cfg.SettingsStore.(clickToPositionSettingsStore); ok {
		if err := store.SetTUIClickToPosition(enabled); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			i.invalidate()
			return
		}
	}
	i.clearMouseSelection()
	i.cfg.TUIClickToPosition = enabled
	seq := tui.SeqMouseOff
	if enabled {
		seq = tui.SeqMouseOn
	}
	if i.cfg.Terminal != nil {
		_, _ = i.cfg.Terminal.Write([]byte(seq))
	}
	i.invalidate()
}

func (i *Interactive) clearMouseSelection() {
	i.mu.Lock()
	if i.rend != nil {
		i.rend.ClearMouseSelection()
	}
	i.mu.Unlock()
}

// A stationary left click positions the input cursor. A drag anywhere in the
// visible chat/input selects text instead, without moving the editor cursor.
func (i *Interactive) handleMouseKey(k tui.Key) bool {
	copyKey := k.Kind == tui.KeyCtrlC ||
		(k.Kind == tui.KeyRune && (k.Rune == 'c' || k.Rune == 'C') && (k.Ctrl || k.Super) && !k.Alt)
	if copyKey {
		i.mu.Lock()
		text, selected := "", false
		if i.rend != nil {
			text, selected = i.rend.MouseSelectionText()
		}
		i.mu.Unlock()
		if selected {
			if text != "" {
				i.copyMouseSelection(text, tui.WriteClipboardText)
			}
			return true
		}
	}
	if k.Kind == tui.KeyEsc {
		i.mu.Lock()
		selected := i.rend != nil && i.rend.MouseSelectionActive()
		if selected {
			i.rend.ClearMouseSelection()
		}
		i.mu.Unlock()
		if selected {
			return true
		}
	}
	switch k.Kind {
	case tui.KeyMouseLeft, tui.KeyMouseDrag, tui.KeyMouseRelease,
		tui.KeyMouseWheelUp, tui.KeyMouseWheelDown:
	default:
		i.clearMouseSelection()
		return false
	}
	if !i.cfg.TUIClickToPosition || i.dialogOwnsInput() {
		i.clearMouseSelection()
		return true
	}
	if k.Kind == tui.KeyMouseWheelUp || k.Kind == tui.KeyMouseWheelDown {
		i.clearMouseSelection()
		delta := 3
		if k.Kind == tui.KeyMouseWheelDown {
			delta = -delta
		}
		i.scrollBy(delta)
		return true
	}
	i.mu.Lock()
	if i.rend == nil {
		i.mu.Unlock()
		return true
	}
	row, col := k.MouseY-1, k.MouseX-1
	switch k.Kind {
	case tui.KeyMouseLeft:
		i.rend.BeginMouseSelection(row, col)
	case tui.KeyMouseDrag:
		i.rend.DragMouseSelection(row, col)
	case tui.KeyMouseRelease:
		active := i.rend.MouseSelectionDragging()
		_, dragged := i.rend.EndMouseSelection(row, col)
		if active && !dragged {
			inputRow := row - i.inputMouseTop
			inputCol := col - i.inputMouseLeft
			cols, rows := i.cfg.Terminal.Size()
			if col >= 0 && col < cols && row >= 0 && row < rows &&
				inputRow >= 0 && inputRow < i.inputMouseRows && inputCol >= 0 {
				i.ed.MoveCursorToVisual(inputRow, inputCol)
			}
		}
		i.mu.Unlock()
		return true
	}
	i.mu.Unlock()
	return true
}

func (i *Interactive) copyMouseSelection(text string, write func(context.Context, string) error) {
	ctx := i.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	err := write(ctx, text)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	// OSC 52 reaches the user's terminal over SSH and requires no local
	// clipboard utility. It has no acknowledgement, so don't claim success.
	if _, err := i.cfg.Terminal.Write([]byte(tui.ClipboardCopySequence(text))); err != nil {
		i.statusErr = "selection copy failed"
		i.statusOK = ""
		return
	}
}
