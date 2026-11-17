package modes

import (
	"context"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

func TestInteractiveSlashControlNavigation(t *testing.T) {
	i := newNavigationInteractive()
	i.ed.SetValue("/session ")
	i.suggest.Render(i.ed.Value(), tui.Dark, 80)

	for _, step := range []struct {
		key  rune
		want string
	}{
		{'p', "/session timeline"},
		{'n', "/session export"},
		{'p', "/session timeline"},
		{'n', "/session export"},
		{'n', "/session import"},
		{'n', "/session fork"},
		{'n', "/session tree"},
		{'n', "/session copy-id"},
		{'n', "/session copy-id"},
	} {
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: step.key, Ctrl: true})
		if got := i.suggest.Selection(i.ed.Value()); got != step.want {
			t.Fatalf("ctrl+%c selection = %q, want %q", step.key, got, step.want)
		}
		if got := i.ed.Value(); got != "/session " {
			t.Fatalf("editor = %q, want unchanged input", got)
		}
	}
}

func TestInteractiveSlashControlNavigationInactive(t *testing.T) {
	for _, input := range []string{"draft", "/unknown-command"} {
		i := newNavigationInteractive()
		i.ed.SetValue(input)
		for _, r := range "np" {
			i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: r, Ctrl: true})
		}
		if got := i.ed.Value(); got != input {
			t.Fatalf("editor = %q, want %q", got, input)
		}
	}
}

func TestInteractiveSlashControlNavigationKeymapWins(t *testing.T) {
	for _, chord := range []string{"ctrl+n", "ctrl+p"} {
		i := newKeymapInteractive(map[string]string{chord: "/help"})
		i.ed.SetValue("/")
		i.suggest.Render(i.ed.Value(), tui.Dark, 80)
		r := rune(chord[len(chord)-1])
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: r, Ctrl: true})
		if len(i.helpBlock) == 0 {
			t.Fatalf("%s did not run the configured command", chord)
		}
	}
}

func TestInteractiveSlashControlNavigationRequiresPlainCtrl(t *testing.T) {
	for _, key := range []tui.Key{
		{Kind: tui.KeyRune, Rune: 'n', Ctrl: true, Alt: true},
		{Kind: tui.KeyRune, Rune: 'n', Ctrl: true, Shift: true},
		{Kind: tui.KeyRune, Rune: 'p', Ctrl: true, Super: true},
	} {
		i := newNavigationInteractive()
		i.ed.SetValue("/session ")
		i.suggest.Render(i.ed.Value(), tui.Dark, 80)
		i.handleKey(context.Background(), key)
		if got := i.suggest.Selection(i.ed.Value()); got != "/session timeline" {
			t.Fatalf("key %+v moved selection to %q", key, got)
		}
	}
}
