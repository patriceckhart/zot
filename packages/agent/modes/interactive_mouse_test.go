package modes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

func TestClickToPositionSettingsAndInput(t *testing.T) {
	for _, style := range []string{"plain", "lines", "block"} {
		for _, position := range []string{"above_input", "below_input"} {
			t.Run(style+"/"+position, func(t *testing.T) {
				term := &cleanupTestTerminal{}
				i := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark,
					TUIInputStyle: style, TUIStatusPosition: position})
				i.rend.Resize(80, 24)
				i.ed.SetValue("hello")
				i.applySettingToggle("tui_click_to_position", true)
				if !strings.Contains(term.String(), tui.SeqMouseOn) {
					t.Fatal("mouse reporting not enabled")
				}
				i.redraw()
				promptWidth := 0
				if style == "plain" {
					promptWidth = 2
				}
				click := tui.Key{Kind: tui.KeyMouseLeft,
					MouseX: i.inputMouseLeft + promptWidth + 3, MouseY: i.inputMouseTop + 1}
				i.handleKey(context.Background(), click)
				release := click
				release.Kind = tui.KeyMouseRelease
				i.handleKey(context.Background(), release)
				if i.ed.CursorC != 2 {
					t.Fatalf("cursor = %d, want 2", i.ed.CursorC)
				}
				outside := click
				outside.MouseY--
				i.handleMouseKey(outside)
				outside.Kind = tui.KeyMouseRelease
				i.handleMouseKey(outside)
				if i.ed.CursorC != 2 {
					t.Fatal("outside click changed cursor")
				}
				i.openSettingsDialog()
				i.handleMouseKey(click)
				if i.ed.CursorC != 2 {
					t.Fatal("dialog click changed cursor")
				}
				found := false
				for _, item := range i.settingsDialog.items {
					for _, child := range item.children {
						if child.key == "tui_click_to_position" {
							found = item.key == "tui_settings" && child.value
						}
					}
				}
				if !found {
					t.Fatal("enabled toggle missing from TUI settings")
				}
				i.applySettingToggle("tui_click_to_position", false)
				if !strings.HasSuffix(term.String(), tui.SeqMouseOff) {
					t.Fatal("mouse reporting not disabled")
				}
			})
		}
	}
}

type failingClickSettingsStore struct {
	SettingsStore
}

func (failingClickSettingsStore) SetTUIClickToPosition(bool) error {
	return errors.New("synthetic save failure")
}

func TestClickToPositionDisabledAndSaveFailure(t *testing.T) {
	term := &cleanupTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark,
		SettingsStore: failingClickSettingsStore{}})
	i.ed.SetValue("hello")
	i.redraw()
	click := tui.Key{Kind: tui.KeyMouseLeft, MouseX: 3, MouseY: i.inputMouseTop + 1}
	i.handleMouseKey(click)
	if i.ed.CursorC != 5 {
		t.Fatal("disabled click moved the cursor")
	}
	term.Reset()
	i.applyClickToPosition(true)
	if i.cfg.TUIClickToPosition || term.Len() != 0 || !strings.Contains(i.statusErr, "synthetic save failure") {
		t.Fatal("failed persistence enabled mouse reporting or did not surface the error")
	}
}

func TestMouseDragSelectsWithoutMovingInputCursor(t *testing.T) {
	// Force the terminal clipboard path without touching the real clipboard.
	t.Setenv("SSH_CONNECTION", "synthetic")
	term := &cleanupTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark,
		TUIClickToPosition: true, TUIInputStyle: "lines"})
	i.rend.Resize(80, 24)
	i.ed.SetValue("hello world")
	i.redraw()
	press := tui.Key{Kind: tui.KeyMouseLeft, MouseX: 1, MouseY: i.inputMouseTop + 1}
	i.handleMouseKey(press)
	term.Reset()
	i.redraw()
	if term.Len() != 0 {
		t.Fatal("redraw changed the screen during selection")
	}
	drag := press
	drag.Kind = tui.KeyMouseDrag
	drag.MouseX = 6
	i.handleMouseKey(drag)
	if !strings.Contains(term.String(), tui.Dark.SelectionStyle()) {
		t.Fatal("drag did not highlight text")
	}
	drag.Kind = tui.KeyMouseRelease
	i.handleMouseKey(drag)
	if i.ed.CursorC != len("hello world") {
		t.Fatal("drag moved the input cursor")
	}
	if strings.Contains(term.String(), "\x1b]52;") || i.statusOK == "selection copied" {
		t.Fatal("release automatically copied the selection")
	}
	if !i.rend.MouseSelectionActive() || i.rend.MouseSelectionDragging() {
		t.Fatal("release did not preserve the highlight")
	}
	term.Reset()
	i.redraw()
	if term.Len() != 0 {
		t.Fatal("redraw changed the persistent highlight")
	}
	for _, key := range []tui.Key{
		{Kind: tui.KeyCtrlC},
		{Kind: tui.KeyCtrlC, Ctrl: true, Shift: true},
		{Kind: tui.KeyRune, Rune: 'c', Super: true},
	} {
		term.Reset()
		if i.handleKey(context.Background(), key) {
			t.Fatal("copy key exited the session")
		}
		if !strings.Contains(term.String(), tui.ClipboardCopySequence("hello")) {
			t.Fatal("explicit copy did not send the selected text")
		}
		if !i.rend.MouseSelectionActive() || i.ed.Value() != "hello world" {
			t.Fatal("copy cleared the selection or edited the input")
		}
	}
	// Duplicate release/motion events must not move a finished selection.
	i.handleMouseKey(tui.Key{Kind: tui.KeyMouseRelease, MouseX: 2, MouseY: press.MouseY})
	i.handleMouseKey(tui.Key{Kind: tui.KeyMouseDrag, MouseX: 2, MouseY: press.MouseY})
	if text, _ := i.rend.MouseSelectionText(); text != "hello" || i.ed.CursorC != len("hello world") {
		t.Fatal("stray mouse events changed the persistent selection")
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	if i.rend.MouseSelectionActive() || i.ed.Value() != "hello world" {
		t.Fatal("escape did not dismiss only the selection")
	}
}

func TestMouseSelectionCopyIsSilentForLocalSuccessAndFallback(t *testing.T) {
	term := &cleanupTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term})
	i.statusOK = "existing status"
	i.statusErr = "existing error"
	i.copyMouseSelection("synthetic", func(ctx context.Context, text string) error {
		if text != "synthetic" {
			t.Fatal("copy payload changed")
		}
		return nil
	})
	if i.statusOK != "existing status" || i.statusErr != "existing error" || strings.Contains(term.String(), "\x1b]52;") {
		t.Fatal("local copy changed status or unnecessarily sent a terminal request")
	}
	i.copyMouseSelection("synthetic", func(context.Context, string) error {
		return errors.New("synthetic unavailable")
	})
	if !strings.Contains(term.String(), tui.ClipboardCopySequence("synthetic")) ||
		i.statusOK != "existing status" || i.statusErr != "existing error" {
		t.Fatal("fallback missing or changed status")
	}
}

func TestMouseSelectionTypingCancelsDrag(t *testing.T) {
	term := &cleanupTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark, TUIClickToPosition: true})
	i.rend.Resize(80, 24)
	i.ed.SetValue("hello")
	i.redraw()
	i.handleMouseKey(tui.Key{Kind: tui.KeyMouseLeft, MouseX: 3, MouseY: i.inputMouseTop + 1})
	i.handleMouseKey(tui.Key{Kind: tui.KeyMouseDrag, MouseX: 5, MouseY: i.inputMouseTop + 1})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: '!'})
	if i.rend.MouseSelectionActive() || i.ed.Value() != "hello!" {
		t.Fatal("typing did not cancel selection and edit normally")
	}
}

func TestClickToPositionRunRestoresMouse(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		term := &cleanupTestTerminal{}
		i := NewInteractive(InteractiveConfig{Terminal: term, TUIClickToPosition: enabled})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = i.Run(ctx)
		if strings.Contains(term.String(), tui.SeqMouseOn) != enabled {
			t.Fatal("startup mouse mode does not match setting")
		}
		if !strings.Contains(term.String(), tui.SeqMouseOff) {
			t.Fatal("exit did not disable mouse mode")
		}
	}
}
