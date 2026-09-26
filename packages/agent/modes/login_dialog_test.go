package modes

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider/auth"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestLoginDialogProviderPickerFiltersLikeModelPicker(t *testing.T) {
	d := newLoginDialog()
	d.step = loginStepProvider
	d.method = "apikey"
	d.status = map[string]string{}
	for _, r := range "llamacpp" {
		d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r})
	}
	options := d.providerOptions()
	if len(options) != 1 || options[0] != "llama.cpp" {
		t.Fatalf("options = %v", options)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if d.step != loginStepLlamaURL {
		t.Fatalf("step = %v", d.step)
	}
}

func TestLoginDialogProviderPickerShowsNoMatches(t *testing.T) {
	d := newLoginDialog()
	d.step = loginStepProvider
	d.method = "apikey"
	d.status = map[string]string{}
	d.providerQuery = "not-a-provider"
	if options := d.providerOptions(); len(options) != 0 {
		t.Fatalf("options = %v", options)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyPageDown})
	if d.cursor != 0 {
		t.Fatalf("cursor = %d", d.cursor)
	}
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action != (loginDialogAction{}) {
		t.Fatalf("action = %+v", action)
	}
}

func TestLoginDialogLlamaCPPValidatesURLAndAcceptsOptionalKey(t *testing.T) {
	d := newLoginDialog()
	d.step = loginStepLlamaURL
	d.provider = "llama.cpp"
	d.method = "apikey"
	d.llamaEd = tui.NewEditor("")

	d.llamaEd.SetValue("file:///tmp/router")
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.SaveLlama || d.step != loginStepLlamaURL {
		t.Fatalf("invalid URL advanced dialog: action=%+v step=%v", action, d.step)
	}
	if !strings.Contains(d.message, "http or https") {
		t.Fatalf("validation message = %q", d.message)
	}

	d.llamaEd.SetValue("http://127.0.0.1:8080/v1/")
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if d.step != loginStepLlamaKey || d.llamaURL != "http://127.0.0.1:8080" {
		t.Fatalf("step=%v URL=%q", d.step, d.llamaURL)
	}
	action := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !action.SaveLlama || action.LlamaURL != "http://127.0.0.1:8080" || action.LlamaAPIKey != "" {
		t.Fatalf("action = %+v", action)
	}
}

func TestLoginDialogOpenAIOAuthAcceptsCodeFromSameTransaction(t *testing.T) {
	d := newLoginDialog()
	d.Open(t.TempDir())
	d.method = "oauth"
	d.provider = "openai-codex"
	d.ShowWaiting("http://localhost:1455/auth/callback")

	text := stripANSIBytes(strings.Join(d.Render(tui.Theme{}, 80), "\n"))
	if !strings.Contains(text, "paste the authorization code") {
		t.Fatal("OpenAI OAuth dialog does not offer pasted code entry")
	}
	d.codeEd.SetValue("code#state")
	if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.SubmitCode != "code#state" {
		t.Fatalf("code submission = %+v", action)
	}
}

func TestLoginDialogAnthropicOffersSeparateOAuthFlows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice int
		manual bool
		step   loginStep
	}{
		{name: "browser", choice: 0, step: loginStepWaiting},
		{name: "copy code", choice: 1, manual: true, step: loginStepPasteCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newLoginDialog()
			d.Open(t.TempDir())
			d.method = "oauth"
			d.step = loginStepProvider
			for idx, p := range d.providerOptions() {
				if p == "anthropic" {
					d.cursor = idx
					break
				}
			}
			if action := d.HandleKey(tui.Key{Kind: tui.KeyEnter}); action.StartOAuth || action.StartManual || d.step != loginStepOAuthMethod {
				t.Fatalf("provider selection started OAuth before choosing a flow: action=%+v step=%v", action, d.step)
			}
			for i := 0; i < tc.choice; i++ {
				d.HandleKey(tui.Key{Kind: tui.KeyDown})
			}
			action := d.HandleKey(tui.Key{Kind: tui.KeyEnter})
			if action.StartManual != tc.manual || action.StartOAuth == tc.manual || d.step != tc.step {
				t.Fatalf("flow action=%+v step=%v", action, d.step)
			}
			if tc.manual {
				d.ShowPasteCode("https://example.com/oauth/authorize")
			} else {
				d.ShowWaiting("https://example.com/oauth/authorize")
			}
			text := stripANSIBytes(strings.Join(d.Render(tui.Theme{}, 80), "\n"))
			if !strings.Contains(text, "paste the authorization code") {
				t.Fatalf("missing code input for %s flow: %s", tc.name, text)
			}
		})
	}
}

func TestAnthropicManualLoginUsesCopyCodeTransaction(t *testing.T) {
	manager := auth.NewManager(auth.NewStore(filepath.Join(t.TempDir(), "auth.json")))
	t.Cleanup(manager.Close)
	i := NewInteractive(InteractiveConfig{AuthManager: manager})
	i.dialog.Open(t.TempDir())
	i.dialog.method = "oauth"
	i.dialog.provider = "anthropic"
	i.dialog.step = loginStepOAuthMethod
	i.dialog.HandleKey(tui.Key{Kind: tui.KeyDown})
	act := i.dialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !act.StartManual || act.Provider != "anthropic" {
		t.Fatalf("expected manual Anthropic transaction, got %+v", act)
	}
	i.startManualOAuthFlow(act.Provider)
	i.handleAuthEvent(<-manager.Events())
	if i.dialog.step != loginStepPasteCode {
		t.Fatalf("step = %v, want paste code", i.dialog.step)
	}
	u, err := url.Parse(i.dialog.url)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("redirect_uri"); got != auth.AnthropicManualOAuth.RedirectURI() {
		t.Fatalf("redirect URI = %q, want copy-code redirect", got)
	}
	if err := manager.CompleteManualOAuth(context.Background(), ""); err == nil || err.Error() != "empty code" {
		t.Fatalf("manual transaction not ready for code entry: %v", err)
	}
}

func TestLoginDialogCursorPosMatchesPaddedInputRow(t *testing.T) {
	d := newLoginDialog()
	d.Open(t.TempDir())
	d.method = "oauth"
	d.provider = "anthropic"
	d.ShowPasteCode("https://example.com/oauth/authorize?code_challenge=abc&state=xyz")

	lines := padDialogFrame(d.Render(tui.Theme{}, 80))
	row, _ := d.CursorPos(80)
	if row < 0 || row >= len(lines) {
		t.Fatalf("CursorPos row = %d outside rendered lines %d", row, len(lines))
	}
	if got := stripANSIBytes(lines[row]); !strings.Contains(got, "▌") {
		t.Fatalf("CursorPos row %d = %q; want editor input row", row, got)
	}
}
