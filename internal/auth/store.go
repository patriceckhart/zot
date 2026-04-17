// Package auth handles credential storage and the two login flows
// supported by zot: API key and (experimental) subscription OAuth.
//
// All credentials live in $ZOT_HOME/auth.json (mode 0600).
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Credentials is the on-disk schema.
type Credentials struct {
	Anthropic ProviderCreds `json:"anthropic,omitempty"`
	OpenAI    ProviderCreds `json:"openai,omitempty"`
}

// ProviderCreds holds credentials for a single provider. Only one of
// APIKey or OAuth is populated at a time.
type ProviderCreds struct {
	APIKey string      `json:"api_key,omitempty"`
	OAuth  *OAuthToken `json:"oauth,omitempty"`
}

// OAuthToken is an OAuth 2 token set with refresh support.
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	// ClientID that issued this token (informational).
	ClientID string `json:"client_id,omitempty"`
	// IDToken is the OIDC id_token (provider-specific; currently only
	// used by the OpenAI Codex flow to derive the ChatGPT account id).
	IDToken string `json:"id_token,omitempty"`
	// AccountID is the ChatGPT account id extracted from IDToken, used
	// as the `chatgpt-account-id` header when calling chatgpt.com/backend-api.
	AccountID string `json:"account_id,omitempty"`
}

// Expired reports whether the token has passed its expiry (with a 60s
// safety margin). Zero expiry is treated as non-expiring.
func (t *OAuthToken) Expired() bool {
	if t == nil || t.Expiry.IsZero() {
		return false
	}
	return time.Now().After(t.Expiry.Add(-60 * time.Second))
}

// Has reports whether at least one credential is present for provider.
func (c *Credentials) Has(provider string) bool {
	p := c.get(provider)
	return p != nil && (p.APIKey != "" || p.OAuth != nil)
}

// Method returns "apikey", "oauth", or "" for the given provider.
func (c *Credentials) Method(provider string) string {
	p := c.get(provider)
	if p == nil {
		return ""
	}
	if p.APIKey != "" {
		return "apikey"
	}
	if p.OAuth != nil {
		return "oauth"
	}
	return ""
}

func (c *Credentials) get(provider string) *ProviderCreds {
	switch provider {
	case "anthropic":
		return &c.Anthropic
	case "openai":
		return &c.OpenAI
	}
	return nil
}

// Store is a mutex-guarded read/write handle to the auth file.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a Store bound to path.
func NewStore(path string) *Store { return &Store{path: path} }

// Load reads the current credentials. Returns a zero Credentials if the
// file does not exist.
func (s *Store) Load() (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (Credentials, error) {
	var c Credentials
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return c, nil
}

// SetAPIKey replaces the API key for provider and saves to disk.
func (s *Store) SetAPIKey(provider, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadLocked()
	if err != nil {
		return err
	}
	p := c.get(provider)
	if p == nil {
		return fmt.Errorf("unknown provider %q", provider)
	}
	p.APIKey = key
	p.OAuth = nil
	return s.saveLocked(c)
}

// SetOAuth replaces the OAuth token for provider and saves to disk.
func (s *Store) SetOAuth(provider string, tok OAuthToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadLocked()
	if err != nil {
		return err
	}
	p := c.get(provider)
	if p == nil {
		return fmt.Errorf("unknown provider %q", provider)
	}
	p.APIKey = ""
	p.OAuth = &tok
	return s.saveLocked(c)
}

// Clear removes all credentials for provider.
func (s *Store) Clear(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.loadLocked()
	if err != nil {
		return err
	}
	p := c.get(provider)
	if p == nil {
		return fmt.Errorf("unknown provider %q", provider)
	}
	*p = ProviderCreds{}
	return s.saveLocked(c)
}

func (s *Store) saveLocked(c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically: write temp then rename.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
