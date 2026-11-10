package modes

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider/auth"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestAPIKeyLoginCompletesThroughAuthEvents(t *testing.T) {
	auth.SetExtraAPIKeyProviders([]string{"test-terminal-login"})
	t.Cleanup(func() { auth.SetExtraAPIKeyProviders(nil) })
	store := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	manager := auth.NewManager(store)
	t.Cleanup(manager.Close)
	built := false
	i := NewInteractive(InteractiveConfig{
		AuthManager: manager,
		BuildAgent: func() (*core.Agent, string, string, error) {
			built = true
			return nil, "test-terminal-login", "test-model", nil
		},
	})
	i.dialog.Open(t.TempDir())
	i.dialog.method = "apikey"
	i.dialog.provider = "test-terminal-login"
	i.startAPIKeyFlow("test-terminal-login")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyPaste, Paste: "synthetic-secret-key"})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	select {
	case event := <-manager.Events():
		if event.Kind != "success" || event.Method != "apikey" {
			t.Fatalf("event = %+v", event)
		}
		i.handleAuthEvent(event)
	case <-time.After(5 * time.Second):
		t.Fatal("API-key login did not complete")
	}
	if !built || i.dialog.step != loginStepDone || !i.dialog.success {
		t.Fatal("login did not rebuild the agent and finish the dialog")
	}
	creds, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.AdditionalAPIKeyCreds["test-terminal-login"].APIKey != "synthetic-secret-key" {
		t.Fatal("key was not stored")
	}
}

func TestAPIKeyLoginUsesMaskedTerminalInput(t *testing.T) {
	manager := auth.NewManager(auth.NewStore(filepath.Join(t.TempDir(), "auth.json")))
	t.Cleanup(manager.Close)
	i := NewInteractive(InteractiveConfig{AuthManager: manager})
	i.dialog.Open(t.TempDir())
	i.dialog.method = "apikey"
	i.dialog.provider = "anthropic"
	i.startAPIKeyFlow("anthropic")
	d := i.dialog
	if d.step != loginStepAPIKey || d.keyEd == nil || !d.keyEd.Mask || d.url != "" {
		t.Fatal("API-key login did not open a masked terminal input")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "synthetic-secret-key"})
	text := stripANSIBytes(strings.Join(d.Render(tui.Theme{}, 80), "\n"))
	if strings.Contains(text, "synthetic-secret-key") || strings.Contains(text, "authorization code") || strings.Contains(text, "127.0.0.1") {
		t.Fatal("API-key dialog leaked a key or displayed OAuth/browser instructions")
	}
	if !strings.Contains(text, "API key (hidden)") {
		t.Fatal("missing API-key prompt")
	}
	row, col := d.CursorPos(80)
	lines := padDialogFrame(d.Render(tui.Theme{}, 80))
	if row < 0 || row >= len(lines) || col < 0 || !strings.Contains(stripANSIBytes(lines[row]), "▌") {
		t.Fatal("API-key cursor is not on the masked input row")
	}
	action := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if action.APIKey != "synthetic-secret-key" || action.Provider != "anthropic" || action.SubmitCode != "" || d.step != loginStepAPIKeySaving {
		t.Fatal("key submission was not routed to the API-key flow")
	}
	if d.keyEd.Value() != "" {
		t.Fatal("submitted key retained in editor")
	}
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.APIKey != "" {
		t.Fatal("duplicate submit while validation is in flight")
	}
	select {
	case ev := <-manager.Events():
		t.Fatalf("input alone started authentication: %+v", ev)
	default:
	}
}

func TestAPIKeyLoginEmptyAndCancel(t *testing.T) {
	d := newLoginDialog()
	d.Open(t.TempDir())
	d.provider = "anthropic"
	d.method = "apikey"
	d.ShowAPIKeyEntry()
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.APIKey != "" || d.step != loginStepAPIKey || d.message == "" {
		t.Fatal("empty key advanced the dialog")
	}
	d.keyEd.SetValue("synthetic-secret-key")
	ctx, cancel := context.WithCancel(context.Background())
	d.cancelKey = cancel
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEsc}); !action.Close || d.Active() {
		t.Fatal("escape did not close the dialog")
	}
	if d.keyEd.Value() != "" || ctx.Err() != context.Canceled {
		t.Fatal("closing did not clear the key and cancel validation")
	}
}

func TestAPIKeyLoginReopenCancelsValidation(t *testing.T) {
	d := newLoginDialog()
	d.Open(t.TempDir())
	d.ShowAPIKeyEntry()
	d.keyEd.SetValue("synthetic-secret-key")
	ctx, cancel := context.WithCancel(context.Background())
	d.cancelKey = cancel
	d.Open(t.TempDir())
	if ctx.Err() != context.Canceled || d.keyEd != nil {
		t.Fatal("reopening retained the old login")
	}
}

func TestOAuthRemoteHints(t *testing.T) {
	for _, tc := range []struct {
		provider, port string
	}{
		{"anthropic", "53692"},
		{"openai-codex", "1455"},
	} {
		d := newLoginDialog()
		d.Open(t.TempDir())
		d.provider = tc.provider
		d.method = "oauth"
		d.ShowWaiting("https://example.com/authorize")
		text := stripANSIBytes(strings.Join(d.Render(tui.Theme{}, 160), "\n"))
		if !strings.Contains(text, "full redirect URL here") || !strings.Contains(text, "ssh -L "+tc.port+":localhost:"+tc.port) {
			t.Fatalf("missing SSH guidance for %s", tc.provider)
		}
	}
	if hints := oauthRemoteHints("github-copilot"); len(hints) != 0 {
		t.Fatal("device flow should not suggest a callback tunnel")
	}
}
