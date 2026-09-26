package auth

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeAPIKeyAcceptsExtraProviderWithoutProbe(t *testing.T) {
	SetExtraAPIKeyProviders([]string{"my-company"})
	t.Cleanup(func() { SetExtraAPIKeyProviders(nil) })

	if err := ProbeAPIKey(context.Background(), "my-company", "test-key"); err != nil {
		t.Fatalf("ProbeAPIKey custom provider: %v", err)
	}
}

func TestProbeGondolaAPIKeyUsesAuthenticatedBalanceEndpoint(t *testing.T) {
	client := &http.Client{Transport: authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.gondola-ai.com/v1/balance" {
			t.Fatalf("probe URL = %q", req.URL.String())
		}
		if got := req.Header.Get("authorization"); got != "Bearer gnd_test" {
			t.Fatalf("authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid key"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}

	err := probeAPIKey(context.Background(), "gondola", "gnd_test", client)
	if err == nil || !strings.Contains(err.Error(), "rejected the key") {
		t.Fatalf("probeAPIKey error = %v", err)
	}
}

type authRoundTripFunc func(*http.Request) (*http.Response, error)

func (f authRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "auth.json"))

	if err := s.SetAPIKey("anthropic", "sk-ant-xyz"); err != nil {
		t.Fatal(err)
	}
	c, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Anthropic.APIKey != "sk-ant-xyz" {
		t.Fatalf("got %+v", c)
	}
	if !c.Has("anthropic") || c.Method("anthropic") != "apikey" {
		t.Fatalf("method mismatch: %q", c.Method("anthropic"))
	}

	tok := OAuthToken{AccessToken: "tok", RefreshToken: "ref", Expiry: time.Now().Add(1 * time.Hour)}
	if err := s.SetOAuth("openai", tok); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Load()
	if c.OpenAI.OAuth == nil || c.OpenAI.OAuth.AccessToken != "tok" {
		t.Fatalf("got %+v", c.OpenAI)
	}
	if c.Method("openai") != "oauth" {
		t.Fatalf("method=%q", c.Method("openai"))
	}

	if err := s.Clear("anthropic"); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Load()
	if c.Has("anthropic") {
		t.Fatalf("expected cleared, got %+v", c.Anthropic)
	}
}

func TestPKCE(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Verifier) < 40 || len(p.Challenge) < 40 {
		t.Fatalf("short pkce: %+v", p)
	}
	if p.Verifier == p.Challenge {
		t.Fatal("verifier == challenge")
	}
}

func TestStartManualOAuthRejectsOpenAIWithoutCallback(t *testing.T) {
	m := NewManager(NewStore(filepath.Join(t.TempDir(), "auth.json")))
	if _, err := m.StartManualOAuth("openai-codex"); err == nil {
		t.Fatal("expected manual OpenAI OAuth to be rejected")
	} else if !strings.Contains(err.Error(), "manual code login is not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBrowserOAuthSharesTransactionWithPastedCode(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai-codex"} {
		t.Run(provider, func(t *testing.T) {
			m := NewManager(NewStore(filepath.Join(t.TempDir(), "auth.json")))
			m.openBrowser = false
			t.Cleanup(m.Close)
			loginURL, err := m.StartOAuth(provider)
			if err != nil {
				if strings.Contains(err.Error(), "bind ") {
					t.Skipf("callback port unavailable: %v", err)
				}
				t.Fatal(err)
			}
			u, err := url.Parse(loginURL)
			if err != nil {
				t.Fatal(err)
			}
			state := u.Query().Get("state")
			if m.manualOp == nil || m.manualState != state || m.manualPKCE.Challenge != u.Query().Get("code_challenge") {
				t.Fatal("pasted-code path does not share the browser transaction")
			}
			if err := m.CompleteManualOAuth(context.Background(), "code#wrong-state"); err == nil || !strings.Contains(err.Error(), "state mismatch") {
				t.Fatalf("wrong state accepted: %v", err)
			}
			if err := m.CompleteManualOAuth(context.Background(), ""); err == nil || err.Error() != "empty code" {
				t.Fatalf("shared transaction unavailable after bad code: %v", err)
			}
			m.CancelOAuth()
			if err := m.CompleteManualOAuth(context.Background(), "code#"+state); err == nil || !strings.Contains(err.Error(), "no manual oauth flow") {
				t.Fatalf("canceled flow still accepts code: %v", err)
			}
		})
	}
}

func TestPastedOAuthCodeUsesBrowserPKCE(t *testing.T) {
	requests := make(chan url.Values, 2)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requests <- r.PostForm
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"test-token","refresh_token":"test-refresh","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	old := OpenAIOAuth.TokenURL
	OpenAIOAuth.TokenURL = tokenServer.URL
	t.Cleanup(func() { OpenAIOAuth.TokenURL = old })

	for _, method := range []string{"paste", "callback"} {
		t.Run(method, func(t *testing.T) {
			store := NewStore(filepath.Join(t.TempDir(), "auth.json"))
			m := NewManager(store)
			m.openBrowser = false
			t.Cleanup(m.Close)
			loginURL, err := m.StartOAuth("openai-codex")
			if err != nil {
				if strings.Contains(err.Error(), "bind ") {
					t.Skipf("callback port unavailable: %v", err)
				}
				t.Fatal(err)
			}
			u, err := url.Parse(loginURL)
			if err != nil {
				t.Fatal(err)
			}
			verifier := m.manualPKCE.Verifier
			redirect := u.Query().Get("redirect_uri")
			codeURL := redirect + "?code=test-code&state=" + url.QueryEscape(u.Query().Get("state"))
			if method == "paste" {
				if err := m.CompleteManualOAuth(context.Background(), codeURL); err != nil {
					t.Fatal(err)
				}
			} else {
				resp, err := http.Get(codeURL)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("callback status = %d", resp.StatusCode)
				}
			}
			select {
			case ev := <-m.Events():
				if ev.Kind != "started" {
					t.Fatalf("event = %+v", ev)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no started event")
			}
			select {
			case ev := <-m.Events():
				if ev.Kind != "success" || ev.Provider != "openai-codex" {
					t.Fatalf("event = %+v", ev)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no success event")
			}
			select {
			case form := <-requests:
				if form.Get("code_verifier") != verifier || form.Get("redirect_uri") != redirect || form.Get("code") != "test-code" {
					t.Fatalf("exchange did not use browser transaction: %v", form)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no token exchange")
			}
			creds, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if creds.OpenAI.OAuth == nil || creds.OpenAI.OAuth.AccessToken != "test-token" {
				t.Fatalf("stored credential: %v", creds.OpenAI.OAuth)
			}
		})
	}
}

func TestAuthorizeURL(t *testing.T) {
	p, _ := NewPKCE()
	u, state, err := AnthropicOAuth.AuthorizeURL(p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("client_id") != AnthropicOAuth.ClientID {
		t.Fatal("missing client_id")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatal("wrong challenge method")
	}
	// anthropic uses state == verifier
	if q.Get("state") != p.Verifier {
		t.Fatalf("state mismatch: %q vs verifier %q", q.Get("state"), p.Verifier)
	}
	if state != p.Verifier {
		t.Fatalf("returned state != verifier")
	}
	if q.Get("redirect_uri") != AnthropicOAuth.RedirectURI() {
		t.Fatalf("redirect_uri=%q", q.Get("redirect_uri"))
	}
	if q.Get("code") != "true" {
		t.Fatalf("missing code=true extra arg")
	}
}

func TestOpenAIAuthorizeURL(t *testing.T) {
	p, _ := NewPKCE()
	u, state, err := OpenAIOAuth.AuthorizeURL(p)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if q.Get("redirect_uri") != "http://localhost:1455/auth/callback" {
		t.Fatalf("redirect_uri=%q", q.Get("redirect_uri"))
	}
	if state == p.Verifier {
		t.Fatal("openai state should be random, not verifier")
	}
	if q.Get("originator") != "zot" {
		t.Fatalf("originator=%q", q.Get("originator"))
	}
}

func TestServerIndexAndAPIKey(t *testing.T) {
	s, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())

	// Swap the probe out for a fake that always succeeds.
	s.probeFn = func(ctx context.Context, provider, key string) error { return nil }

	resp, err := http.Get(s.URL() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("index status %d", resp.StatusCode)
	}

	resp, err = http.Get(s.URL() + "/apikey?provider=anthropic")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("apikey form status %d", resp.StatusCode)
	}

	// POST a fake key and expect a success result on the channel.
	_, err = http.PostForm(s.URL()+"/apikey", url.Values{
		"provider": {"anthropic"},
		"api_key":  {"sk-ant-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-s.Result():
		if r.Err != nil {
			t.Fatalf("unexpected err: %v", r.Err)
		}
		if r.Provider != "anthropic" || r.APIKey != "sk-ant-test" || r.Method != "apikey" {
			t.Fatalf("bad result: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result")
	}
}

func TestServerCallback(t *testing.T) {
	s, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())

	u := s.URL() + "/callback?code=abc123&state=anthropic:xyz"
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case r := <-s.Result():
		if r.Provider != "anthropic" || r.Code != "abc123" || r.State != "anthropic:xyz" || r.Method != "oauth" {
			t.Fatalf("bad result: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result")
	}
}

func TestCallbackServerRoundTrip(t *testing.T) {
	// Use a provider config with an ephemeral port so tests stay hermetic.
	op := OAuthProvider{
		Name:         "anthropic",
		RedirectHost: "127.0.0.1",
		RedirectPort: 0, // let kernel pick
		RedirectPath: "/callback",
	}
	// We need a real fixed port for CallbackServer.NewCallbackServer; find one.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	op.RedirectPort = port

	cs, err := NewCallbackServer(op, "thestate")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Shutdown()

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = http.Get(cs.URL() + "?code=abc&state=thestate")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := cs.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Code != "abc" || r.State != "thestate" {
		t.Fatalf("bad result: %+v", r)
	}
}

func TestCallbackServerStateMismatch(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	op := OAuthProvider{Name: "anthropic", RedirectHost: "127.0.0.1", RedirectPort: port, RedirectPath: "/callback"}
	cs, err := NewCallbackServer(op, "expected")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Shutdown()

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = http.Get(cs.URL() + "?code=abc&state=wrong")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := cs.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Err == nil || !strings.Contains(r.Err.Error(), "state") {
		t.Fatalf("want state error, got %+v", r)
	}
}

func TestServerCallbackError(t *testing.T) {
	s, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())

	_, _ = http.Get(s.URL() + "/callback?error=access_denied&state=openai:xyz")
	select {
	case r := <-s.Result():
		if r.Err == nil || r.Provider != "openai" {
			t.Fatalf("bad result: %+v", r)
		}
		if !strings.Contains(r.Err.Error(), "access_denied") {
			t.Fatalf("missing reason: %v", r.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no result")
	}
}
