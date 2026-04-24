// Package agent wires the provider, core, tools, auth, and modes into a CLI.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/patriceckhart/zot/internal/auth"
)

// Config is the persisted user configuration.
type Config struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
	Theme     string `json:"theme"`

	// LastChangelogShown is the version whose release-notes
	// dialog the user has already seen. When the running binary's
	// version differs, the next interactive run shows the
	// changelog (fetched from the GitHub release page) once and
	// updates this field. Empty means "never shown".
	LastChangelogShown string `json:"last_changelog_shown,omitempty"`
}

// ZotHome returns $ZOT_HOME or the OS-default data dir.
//
// All zot state (config.json, auth.json, sessions/, logs/) lives under
// this directory.
func ZotHome() string {
	if v := os.Getenv("ZOT_HOME"); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "zot")
		}
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "zot")
		}
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "zot")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "zot")
	}
	return ".zot"
}

// ConfigPath returns the path to config.json.
func ConfigPath() string { return filepath.Join(ZotHome(), "config.json") }

// AuthPath returns the path to auth.json.
func AuthPath() string { return filepath.Join(ZotHome(), "auth.json") }

// SessionsPath returns the directory holding session files.
func SessionsPath() string { return filepath.Join(ZotHome(), "sessions") }

// LogsPath returns the directory holding log files.
func LogsPath() string { return filepath.Join(ZotHome(), "logs") }

// LoadConfig reads the config file, returning defaults if missing.
func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}

// SaveConfig writes the config file, creating parent dirs.
func SaveConfig(c Config) error {
	if err := os.MkdirAll(ZotHome(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), b, 0o644)
}

// AuthStoreFor returns the auth.Store backed by AuthPath().
func AuthStoreFor() *auth.Store { return auth.NewStore(AuthPath()) }

// ResolveCredential returns the credential (api key or oauth access
// token), the method ("apikey"/"oauth"), and an error when no
// credential is available.
//
// Lookup order:
//  1. explicit (e.g. --api-key): treated as API key
//  2. provider-specific env var: treated as API key
//  3. auth.json: api key OR oauth, whichever is present
func ResolveCredential(provider, explicit string) (cred, method string, err error) {
	cred, method, _, err = ResolveCredentialFull(provider, explicit)
	return cred, method, err
}

// ResolveCredentialFull is like ResolveCredential but also returns a
// provider-specific accountID when the credential is an OpenAI OAuth
// token (the ChatGPT account id extracted from the stored id_token).
// accountID is "" for API-key auth and for anthropic.
func ResolveCredentialFull(provider, explicit string) (cred, method, accountID string, err error) {
	if explicit != "" {
		return explicit, "apikey", "", nil
	}
	switch provider {
	case "anthropic":
		if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "openai":
		if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	}
	c, err := AuthStoreFor().Load()
	if err != nil {
		return "", "", "", err
	}
	switch provider {
	case "anthropic":
		if c.Anthropic.APIKey != "" {
			return c.Anthropic.APIKey, "apikey", "", nil
		}
		if c.Anthropic.OAuth != nil && c.Anthropic.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpired("anthropic", c.Anthropic.OAuth)
			return tok.AccessToken, "oauth", "", nil
		}
	case "openai":
		if c.OpenAI.APIKey != "" {
			return c.OpenAI.APIKey, "apikey", "", nil
		}
		if c.OpenAI.OAuth != nil && c.OpenAI.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpired("openai", c.OpenAI.OAuth)
			return tok.AccessToken, "oauth", tok.AccountID, nil
		}
	}
	return "", "", "", fmt.Errorf("no credential for %s", provider)
}

// loadOAuthToken reads the current OAuth token from auth.json for the
// given provider. Returns nil if no token is stored.
func loadOAuthToken(providerName string) *auth.OAuthToken {
	c, err := AuthStoreFor().Load()
	if err != nil {
		return nil
	}
	switch providerName {
	case "anthropic":
		if c.Anthropic.OAuth != nil {
			return c.Anthropic.OAuth
		}
	case "openai":
		if c.OpenAI.OAuth != nil {
			return c.OpenAI.OAuth
		}
	}
	return nil
}

// refreshIfExpired returns a usable OAuth token for the given provider,
// refreshing it synchronously when it's past (or near) expiry. The
// refreshed token is persisted to auth.json.
//
// Failures return the original token unchanged — the caller then makes
// a request with the stale access_token, which will 401. That's still
// better than crashing at credential-resolution time.
func refreshIfExpired(providerName string, tok *auth.OAuthToken) (*auth.OAuthToken, error) {
	if tok == nil {
		return &auth.OAuthToken{}, fmt.Errorf("nil token")
	}
	if !tok.Expired() {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return tok, fmt.Errorf("%s oauth token expired and no refresh_token available — run /login again", providerName)
	}

	var op auth.OAuthProvider
	switch providerName {
	case "anthropic":
		op = auth.AnthropicOAuth
	case "openai":
		op = auth.OpenAIOAuth
	default:
		return tok, fmt.Errorf("unknown provider %q", providerName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	next, err := op.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		return tok, fmt.Errorf("refresh %s: %w", providerName, err)
	}
	// Preserve the refresh token if the server omitted it (Anthropic often does).
	if next.RefreshToken == "" {
		next.RefreshToken = tok.RefreshToken
	}
	// Carry over account id (openai) / id_token across refreshes.
	if next.AccountID == "" {
		next.AccountID = tok.AccountID
	}
	if next.IDToken == "" {
		next.IDToken = tok.IDToken
	}
	if err := AuthStoreFor().SetOAuth(providerName, *next); err != nil {
		return next, fmt.Errorf("persist refreshed token: %w", err)
	}
	return next, nil
}
