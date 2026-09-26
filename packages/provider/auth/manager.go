package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Event is delivered on Manager.Events().
type Event struct {
	Kind     string // "started" | "browser_open" | "success" | "error" | "canceled"
	Provider string
	Method   string
	URL      string // login URL (for "started"/"browser_open")
	Message  string // on error
}

// Manager drives a login flow end-to-end. It owns a local web server
// (for api-key form) plus provider-specific OAuth callback servers.
type Manager struct {
	store       *Store
	keyServer   *Server         // random-port web form server (api-key flow)
	oauthServer *CallbackServer // fixed-port callback server (oauth flow, only one at a time)
	mu          sync.Mutex
	events      chan Event
	openBrowser bool

	oauthCtx    context.Context
	oauthCancel context.CancelFunc

	manualOp            *OAuthProvider
	manualStoreProvider string
	manualEventProvider string
	manualPKCE          PKCE
	manualState         string
}

// NewManager returns a Manager bound to store.
func NewManager(store *Store) *Manager {
	return &Manager{
		store:       store,
		events:      make(chan Event, 16),
		openBrowser: true,
	}
}

// Store returns the underlying credential store.
func (m *Manager) Store() *Store { return m.store }

// Events returns the read-only event channel.
func (m *Manager) Events() <-chan Event { return m.events }

// Close shuts down any running servers and cancels pending flows.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = m.keyServer.Shutdown(ctx)
		cancel()
		m.keyServer = nil
	}
	if m.oauthServer != nil {
		m.oauthServer.Shutdown()
		m.oauthServer = nil
	}
	if m.oauthCancel != nil {
		m.oauthCancel()
		m.oauthCancel = nil
	}
	m.clearManualOAuthLocked()
}

// ---- API key flow ----

// StartAPIKey launches the API-key login flow.
func (m *Manager) StartAPIKey(provider string) (string, error) {
	if !isKnownAPIKeyProvider(provider) {
		return "", errors.New(apiKeyProviderMessage())
	}
	if err := m.ensureKeyServer(); err != nil {
		return "", err
	}
	u := m.keyServer.URL() + "/apikey?provider=" + provider
	go m.maybeOpen(u)
	m.emit(Event{Kind: "started", Provider: provider, Method: "apikey", URL: u})
	return u, nil
}

func (m *Manager) ensureKeyServer() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keyServer != nil {
		return nil
	}
	s, err := NewServer()
	if err != nil {
		return err
	}
	m.keyServer = s
	go m.consumeKeyServerResults()
	return nil
}

func (m *Manager) consumeKeyServerResults() {
	for res := range m.keyServer.Result() {
		if res.Err != nil {
			m.emit(Event{Kind: "error", Provider: res.Provider, Method: res.Method, Message: res.Err.Error()})
			continue
		}
		if err := m.store.SetAPIKey(res.Provider, res.APIKey); err != nil {
			m.emit(Event{Kind: "error", Provider: res.Provider, Method: "apikey", Message: err.Error()})
			continue
		}
		m.emit(Event{Kind: "success", Provider: res.Provider, Method: "apikey"})
	}
}

// ---- OAuth flow ----

// StartOAuth launches the subscription OAuth flow for provider.
// Only one oauth flow may be in progress at a time (because the
// callback port is fixed per provider and re-used by the official CLIs).
//
// Note: "google" is intentionally not supported here. Google does not
// offer a subscription OAuth that exchanges a Google One AI / Gemini
// Advanced login for usable Generative Language API credentials, so
// the only supported google login path is the API-key flow.
func (m *Manager) StartOAuth(provider string) (string, error) {
	if provider == "kimi" {
		return m.StartKimiDeviceOAuth()
	}
	if provider == "xai" {
		return m.StartXAIDeviceOAuth()
	}
	if provider == "github-copilot" {
		return m.StartGitHubCopilotDeviceOAuth()
	}
	storeProvider := provider
	var op OAuthProvider
	switch provider {
	case "anthropic":
		op = AnthropicOAuth
	case "openai", "openai-codex":
		op = OpenAIOAuth
		storeProvider = "openai"
	case "google":
		return "", fmt.Errorf("google login is api-key only; use api key login for gemini")
	case "deepseek":
		return "", fmt.Errorf("deepseek login is api-key only; use api key login")
	default:
		return "", fmt.Errorf("provider must be anthropic, openai, openai-codex, kimi, xai, github-copilot, deepseek, or google")
	}

	m.mu.Lock()
	if m.oauthServer != nil {
		m.oauthServer.Shutdown()
		m.oauthServer = nil
	}
	if m.oauthCancel != nil {
		m.oauthCancel()
	}
	m.clearManualOAuthLocked()
	m.mu.Unlock()

	pkce, err := NewPKCE()
	if err != nil {
		return "", err
	}
	authURL, state, err := op.AuthorizeURL(pkce)
	if err != nil {
		return "", err
	}

	cs, err := NewCallbackServer(op, state)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.oauthServer = cs
	m.oauthCtx = ctx
	m.oauthCancel = cancel
	m.manualOp = &op
	m.manualStoreProvider = storeProvider
	m.manualEventProvider = provider
	m.manualPKCE = pkce
	m.manualState = state
	m.mu.Unlock()

	go m.awaitOAuth(ctx, op, storeProvider, provider, cs, pkce, state)
	go m.maybeOpen(authURL)
	m.emit(Event{Kind: "started", Provider: provider, Method: "oauth", URL: authURL})
	return authURL, nil
}

func (m *Manager) awaitOAuth(ctx context.Context, op OAuthProvider, storeProvider, eventProvider string, cs *CallbackServer, pkce PKCE, state string) {
	defer cs.Shutdown()

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer waitCancel()
	res, err := cs.Result(waitCtx)
	if err != nil {
		if ctx.Err() == nil {
			m.mu.Lock()
			if m.oauthCtx == ctx {
				m.clearManualOAuthLocked()
			}
			m.mu.Unlock()
			m.emit(Event{Kind: "error", Provider: eventProvider, Method: "oauth", Message: "timeout waiting for callback"})
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	if res.Err != nil {
		m.mu.Lock()
		if m.oauthCtx == ctx {
			m.clearManualOAuthLocked()
		}
		m.mu.Unlock()
		m.emit(Event{Kind: "error", Provider: eventProvider, Method: "oauth", Message: res.Err.Error()})
		return
	}
	m.mu.Lock()
	if m.oauthCtx != ctx || m.manualOp == nil {
		m.mu.Unlock()
		return
	}
	m.clearManualOAuthLocked()
	m.mu.Unlock()

	exCtx, exCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer exCancel()
	tok, err := op.Exchange(exCtx, res.Code, res.State, pkce)
	if err != nil {
		m.emit(Event{Kind: "error", Provider: eventProvider, Method: "oauth", Message: err.Error()})
		return
	}
	if err := m.store.SetOAuth(storeProvider, *tok); err != nil {
		m.emit(Event{Kind: "error", Provider: eventProvider, Method: "oauth", Message: err.Error()})
		return
	}
	m.emit(Event{Kind: "success", Provider: eventProvider, Method: "oauth"})
}

// StartKimiDeviceOAuth starts Kimi Code's device-code subscription login.
func (m *Manager) StartKimiDeviceOAuth() (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.oauthCancel != nil {
		m.oauthCancel()
	}
	m.oauthCtx = ctx
	m.oauthCancel = cancel
	m.mu.Unlock()

	dev, err := RequestKimiDeviceAuthorization(ctx)
	if err != nil {
		return "", err
	}
	url := dev.VerificationURIComplete
	go m.maybeOpen(url)
	m.emit(Event{Kind: "started", Provider: "kimi", Method: "oauth", URL: url})
	go func() {
		tok, err := PollKimiDeviceToken(ctx, dev)
		if err != nil {
			if ctx.Err() == nil {
				m.emit(Event{Kind: "error", Provider: "kimi", Method: "oauth", Message: err.Error()})
			}
			return
		}
		if err := m.store.SetOAuth("kimi", *tok); err != nil {
			m.emit(Event{Kind: "error", Provider: "kimi", Method: "oauth", Message: err.Error()})
			return
		}
		m.emit(Event{Kind: "success", Provider: "kimi", Method: "oauth"})
	}()
	return url, nil
}

// StartXAIDeviceOAuth starts xAI's device-code subscription login.
func (m *Manager) StartXAIDeviceOAuth() (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.oauthCancel != nil {
		m.oauthCancel()
	}
	m.oauthCtx = ctx
	m.oauthCancel = cancel
	m.mu.Unlock()

	dev, err := RequestXAIDeviceAuthorization(ctx)
	if err != nil {
		return "", err
	}
	loginURL := dev.VerificationURIComplete
	go m.maybeOpen(loginURL)
	m.emit(Event{Kind: "started", Provider: "xai", Method: "oauth", URL: loginURL})
	go func() {
		tok, err := PollXAIDeviceToken(ctx, dev)
		if err != nil {
			if ctx.Err() == nil {
				m.emit(Event{Kind: "error", Provider: "xai", Method: "oauth", Message: err.Error()})
			}
			return
		}
		if err := m.store.SetOAuth("xai", *tok); err != nil {
			m.emit(Event{Kind: "error", Provider: "xai", Method: "oauth", Message: err.Error()})
			return
		}
		m.emit(Event{Kind: "success", Provider: "xai", Method: "oauth"})
	}()
	return loginURL, nil
}

// StartGitHubCopilotDeviceOAuth starts GitHub Copilot's device-code subscription login.
func (m *Manager) StartGitHubCopilotDeviceOAuth() (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.oauthCancel != nil {
		m.oauthCancel()
	}
	m.oauthCtx = ctx
	m.oauthCancel = cancel
	m.mu.Unlock()

	dev, err := RequestGitHubCopilotDeviceAuthorization(ctx)
	if err != nil {
		return "", err
	}
	loginURL := dev.VerificationURI
	if dev.UserCode != "" {
		loginURL += "?user_code=" + url.QueryEscape(dev.UserCode)
	}
	go m.maybeOpen(loginURL)
	m.emit(Event{Kind: "started", Provider: "github-copilot", Method: "oauth", URL: loginURL})
	go func() {
		tok, err := PollGitHubCopilotDeviceToken(ctx, dev)
		if err != nil {
			if ctx.Err() == nil {
				m.emit(Event{Kind: "error", Provider: "github-copilot", Method: "oauth", Message: err.Error()})
			}
			return
		}
		if err := m.store.SetOAuth("github-copilot", *tok); err != nil {
			m.emit(Event{Kind: "error", Provider: "github-copilot", Method: "oauth", Message: err.Error()})
			return
		}
		m.emit(Event{Kind: "success", Provider: "github-copilot", Method: "oauth"})
	}()
	return loginURL, nil
}

// StartManualOAuth begins an OAuth flow but does NOT start a local
// callback server or open a browser. The returned URL is shown to the
// user so they can complete the authorization on another device; the
// resulting code is pasted back via CompleteManualOAuth.
func (m *Manager) StartManualOAuth(provider string) (string, error) {
	if provider == "kimi" {
		return m.StartKimiDeviceOAuth()
	}
	if provider == "xai" {
		return m.StartXAIDeviceOAuth()
	}
	if provider == "github-copilot" {
		return m.StartGitHubCopilotDeviceOAuth()
	}
	storeProvider := provider
	var op OAuthProvider
	switch provider {
	case "anthropic":
		op = AnthropicManualOAuth
	case "openai", "openai-codex":
		return "", fmt.Errorf("openai login requires the browser callback flow; manual code login is not supported")
	case "google":
		return "", fmt.Errorf("google login is api-key only; use api key login for gemini")
	case "deepseek":
		return "", fmt.Errorf("deepseek login is api-key only; use api key login")
	default:
		return "", fmt.Errorf("provider must be anthropic, openai, openai-codex, kimi, xai, github-copilot, deepseek, or google")
	}

	m.CancelOAuth()
	pkce, err := NewPKCE()
	if err != nil {
		return "", err
	}
	authURL, state, err := op.AuthorizeURL(pkce)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	m.manualOp = &op
	m.manualStoreProvider = storeProvider
	m.manualEventProvider = provider
	m.manualPKCE = pkce
	m.manualState = state
	m.mu.Unlock()

	m.emit(Event{Kind: "started", Provider: provider, Method: "oauth", URL: authURL})
	return authURL, nil
}

// CompleteManualOAuth exchanges the user-pasted authorization code for
// a token and stores it. Accepts either a raw code or a "code#state"
// token shown by providers like Anthropic when code=true is set.
func (m *Manager) CompleteManualOAuth(ctx context.Context, input string) error {
	code, pastedState := parseManualCodeInput(strings.TrimSpace(input))
	m.mu.Lock()
	if m.manualOp == nil {
		m.mu.Unlock()
		return fmt.Errorf("no manual oauth flow in progress")
	}
	if pastedState != "" && pastedState != m.manualState {
		m.mu.Unlock()
		return fmt.Errorf("oauth state mismatch")
	}
	if code == "" {
		m.mu.Unlock()
		return fmt.Errorf("empty code")
	}
	op := *m.manualOp
	storeProvider := m.manualStoreProvider
	eventProvider := m.manualEventProvider
	pkce := m.manualPKCE
	state := m.manualState
	m.clearManualOAuthLocked()
	if m.oauthServer != nil {
		if m.oauthCancel != nil {
			m.oauthCancel()
			m.oauthCancel = nil
		}
		m.oauthServer.Shutdown()
		m.oauthServer = nil
	}
	m.mu.Unlock()
	exCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tok, err := op.Exchange(exCtx, code, state, pkce)
	if err != nil {
		m.emit(Event{Kind: "error", Provider: eventProvider, Method: "oauth", Message: err.Error()})
		return err
	}
	if err := m.store.SetOAuth(storeProvider, *tok); err != nil {
		m.emit(Event{Kind: "error", Provider: eventProvider, Method: "oauth", Message: err.Error()})
		return err
	}
	m.emit(Event{Kind: "success", Provider: eventProvider, Method: "oauth"})
	return nil
}

// parseManualCodeInput accepts any of:
//   - a bare authorization code
//   - a "code#state" pair
//   - a full redirect URL like http(s)://host:port/callback?code=X&state=Y
//
// and returns the extracted code and (if any) state.
func parseManualCodeInput(s string) (code, state string) {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		if u, err := url.Parse(s); err == nil {
			q := u.Query()
			return q.Get("code"), q.Get("state")
		}
	}
	if idx := strings.IndexByte(s, '#'); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
}

// HasBrowser reports whether the current environment probably has a
// working interactive browser reachable from localhost. Used by the
// login flow to auto-switch to paste-code mode on headless boxes
// (containers, SSH without display forwarding, etc.) instead of
// trying to bind a callback port the user can never reach.
func HasBrowser() bool {
	if os.Getenv("ZOT_NO_BROWSER") != "" {
		return false
	}
	if os.Getenv("ZOT_FORCE_BROWSER") != "" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return false
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		txt := string(b)
		if strings.Contains(txt, "docker") || strings.Contains(txt, "kubepods") || strings.Contains(txt, "containerd") {
			return false
		}
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		return true
	}
}

// CancelOAuth aborts any in-flight OAuth flow.
func (m *Manager) CancelOAuth() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.oauthCancel != nil {
		m.oauthCancel()
		m.oauthCancel = nil
	}
	if m.oauthServer != nil {
		m.oauthServer.Shutdown()
		m.oauthServer = nil
	}
	m.clearManualOAuthLocked()
}

func (m *Manager) clearManualOAuthLocked() {
	m.manualOp = nil
	m.manualStoreProvider = ""
	m.manualEventProvider = ""
	m.manualPKCE = PKCE{}
	m.manualState = ""
}

// ---- shared ----

func (m *Manager) emit(e Event) {
	select {
	case m.events <- e:
	default:
	}
}

// maybeOpen tries to open u in the system browser.
func (m *Manager) maybeOpen(u string) {
	if !m.openBrowser {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
	m.emit(Event{Kind: "browser_open", URL: u})
}

// RefreshIfNeeded returns the currently-usable credential for provider,
// refreshing an expired OAuth access token if necessary.
func (m *Manager) RefreshIfNeeded(ctx context.Context, provider string) (string, string, error) {
	creds, err := m.store.Load()
	if err != nil {
		return "", "", err
	}
	p := creds.get(provider)
	if p == nil {
		return "", "", fmt.Errorf("unknown provider %q", provider)
	}
	if p.APIKey != "" {
		return p.APIKey, "apikey", nil
	}
	if p.APIKeyCommand != nil {
		key, err := ResolveAPIKeyCommand(ctx, *p.APIKeyCommand)
		if err != nil {
			return "", "", fmt.Errorf("resolve api key for %s: %w", provider, err)
		}
		return key, "apikey", nil
	}
	if p.OAuth == nil {
		return "", "", fmt.Errorf("no credentials for %s", provider)
	}
	if !p.OAuth.Expired() {
		return p.OAuth.AccessToken, "oauth", nil
	}
	if p.OAuth.RefreshToken == "" {
		return "", "", fmt.Errorf("%s access token expired and no refresh token is available; please /login again", provider)
	}
	var op OAuthProvider
	switch provider {
	case "anthropic":
		op = AnthropicOAuth
	case "openai":
		op = OpenAIOAuth
	}
	tok, err := op.Refresh(ctx, p.OAuth.RefreshToken)
	if err != nil {
		return "", "", err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = p.OAuth.RefreshToken
	}
	if err := m.store.SetOAuth(provider, *tok); err != nil {
		return "", "", err
	}
	return tok.AccessToken, "oauth", nil
}
