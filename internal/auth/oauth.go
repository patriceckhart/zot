package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthProvider configures a single subscription OAuth flow.
//
// WARNING: the client ids used here belong to the official first-party
// CLIs (Claude Code for Anthropic, Codex CLI for OpenAI). Using them from
// third-party software is against the respective terms of service and
// may be revoked at any time. zot exposes this only behind
// --experimental-oauth.
type OAuthProvider struct {
	Name           string
	AuthURL        string
	TokenURL       string
	ClientID       string
	Scopes         []string
	RedirectHost   string // usually "localhost"
	RedirectPort   int    // must match the client registration at the provider
	RedirectPath   string // e.g. "/callback" or "/auth/callback"
	ExtraAuthArgs  map[string]string
	ExtraTokenArgs map[string]string
	// TokenBodyJSON selects JSON body vs application/x-www-form-urlencoded
	// for token requests. Anthropic requires JSON; OpenAI requires form.
	TokenBodyJSON bool
	// StateEqualsVerifier: if true, the OAuth state parameter is set to the
	// PKCE verifier instead of a random value (Anthropic's Claude Code does this).
	StateEqualsVerifier bool
	// IncludeStateInTokenRequest: if true, the state value is included in
	// the token-exchange payload (Anthropic requires this; OpenAI does not).
	IncludeStateInTokenRequest bool
}

// RedirectURI returns the full redirect URI for this provider. For the
// manual variants (no local callback server) RedirectPath is already an
// absolute https URL, in which case it's returned as-is.
func (p OAuthProvider) RedirectURI() string {
	if p.RedirectHost == "" && (strings.HasPrefix(p.RedirectPath, "http://") || strings.HasPrefix(p.RedirectPath, "https://")) {
		return p.RedirectPath
	}
	return fmt.Sprintf("http://%s:%d%s", p.RedirectHost, p.RedirectPort, p.RedirectPath)
}

// Built-in providers. Values mirror Anthropic's Claude Code CLI and
// OpenAI's Codex CLI. Changing any of these fields will most likely
// cause the flow to fail with redirect_uri_mismatch or invalid_client.
var (
	// Anthropic Claude Pro/Max: used by the Claude Code CLI.
	AnthropicOAuth = OAuthProvider{
		Name:         "anthropic",
		AuthURL:      "https://claude.ai/oauth/authorize",
		TokenURL:     "https://platform.claude.com/v1/oauth/token",
		ClientID:     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:       []string{"org:create_api_key", "user:profile", "user:inference"},
		RedirectHost: "localhost",
		RedirectPort: 53692,
		RedirectPath: "/callback",
		ExtraAuthArgs: map[string]string{
			"code": "true",
		},
		TokenBodyJSON:              true,
		StateEqualsVerifier:        true,
		IncludeStateInTokenRequest: true,
	}

	// Anthropic manual / headless variant: redirects to Anthropic's
	// copy-code page instead of a local loopback port, used when there
	// is no browser on the machine running zot (e.g. inside a Docker
	// container or over plain SSH).
	AnthropicManualOAuth = OAuthProvider{
		Name:         "anthropic",
		AuthURL:      "https://claude.ai/oauth/authorize",
		TokenURL:     "https://console.anthropic.com/v1/oauth/token",
		ClientID:     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:       []string{"org:create_api_key", "user:profile", "user:inference"},
		RedirectPath: "https://console.anthropic.com/oauth/code/callback",
		ExtraAuthArgs: map[string]string{
			"code": "true",
		},
		TokenBodyJSON:              true,
		StateEqualsVerifier:        true,
		IncludeStateInTokenRequest: true,
	}

	// OpenAI ChatGPT subscription: used by Codex CLI.
	OpenAIOAuth = OAuthProvider{
		Name:         "openai",
		AuthURL:      "https://auth.openai.com/oauth/authorize",
		TokenURL:     "https://auth.openai.com/oauth/token",
		ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		Scopes:       []string{"openid", "profile", "email", "offline_access"},
		RedirectHost: "localhost",
		RedirectPort: 1455,
		RedirectPath: "/auth/callback",
		ExtraAuthArgs: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "zot",
		},
	}
)

// PKCE holds a verifier/challenge pair.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a random PKCE pair using SHA-256.
func NewPKCE() (PKCE, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return PKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCE{Verifier: verifier, Challenge: challenge}, nil
}

// RandomHex returns a random hex string, used as an OAuth state value.
func RandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// AuthorizeURL builds the browser URL for the initial authorization
// request. The returned state value is what we expect to see on the
// callback and what the caller should persist until that arrives.
func (p OAuthProvider) AuthorizeURL(pkce PKCE) (authURL, state string, err error) {
	if p.StateEqualsVerifier {
		state = pkce.Verifier
	} else {
		state, err = RandomHex(16)
		if err != nil {
			return "", "", err
		}
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", p.RedirectURI())
	q.Set("scope", strings.Join(p.Scopes, " "))
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	for k, v := range p.ExtraAuthArgs {
		q.Set(k, v)
	}
	return p.AuthURL + "?" + q.Encode(), state, nil
}

// Exchange swaps an authorization code for an OAuthToken.
func (p OAuthProvider) Exchange(ctx context.Context, code, state string, pkce PKCE) (*OAuthToken, error) {
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     p.ClientID,
		"code":          code,
		"redirect_uri":  p.RedirectURI(),
		"code_verifier": pkce.Verifier,
	}
	if p.IncludeStateInTokenRequest {
		payload["state"] = state
	}
	for k, v := range p.ExtraTokenArgs {
		payload[k] = v
	}
	return p.doTokenRequest(ctx, payload)
}

// Refresh uses a refresh_token to obtain a new access token.
func (p OAuthProvider) Refresh(ctx context.Context, refreshToken string) (*OAuthToken, error) {
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     p.ClientID,
		"refresh_token": refreshToken,
	}
	for k, v := range p.ExtraTokenArgs {
		payload[k] = v
	}
	return p.doTokenRequest(ctx, payload)
}

// ExtractOpenAIAccountID parses the chatgpt_account_id claim from an
// OpenAI id_token JWT. The claim sits under "https://api.openai.com/auth"
// in the JWT payload. Returns "" if the token is malformed or lacks
// the claim.
func ExtractOpenAIAccountID(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some tokens use standard base64 with padding.
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	authClaim, _ := body["https://api.openai.com/auth"].(map[string]interface{})
	if authClaim == nil {
		return ""
	}
	id, _ := authClaim["chatgpt_account_id"].(string)
	return id
}

func (p OAuthProvider) doTokenRequest(ctx context.Context, payload map[string]string) (*OAuthToken, error) {
	var (
		req         *http.Request
		err         error
		contentType string
	)
	if p.TokenBodyJSON {
		body, _ := json.Marshal(payload)
		req, err = http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader(string(body)))
		contentType = "application/json"
	} else {
		form := url.Values{}
		for k, v := range payload {
			form.Set(k, v)
		}
		req, err = http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader(form.Encode()))
		contentType = "application/x-www-form-urlencoded"
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", contentType)
	req.Header.Set("accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token http %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("parse token response: %w: %s", err, string(respBody))
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in response: %s", string(respBody))
	}
	tok := &OAuthToken{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		TokenType:    raw.TokenType,
		Scope:        raw.Scope,
		ClientID:     p.ClientID,
	}
	if raw.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	// Capture id_token & account id for providers that use OIDC (OpenAI).
	// This lets the OpenAI Codex provider send chatgpt-account-id.
	if raw.IDToken != "" {
		tok.IDToken = raw.IDToken
		tok.AccountID = ExtractOpenAIAccountID(raw.IDToken)
	}
	return tok, nil
}
