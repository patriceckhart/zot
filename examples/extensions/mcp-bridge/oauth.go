package main

import (
 "context"
 "crypto/sha256"
 "encoding/json"
 "errors"
 "fmt"
 "net"
 "net/http"
 "net/url"
 "os"
 "path/filepath"
 "time"

 "github.com/mark3labs/mcp-go/client/transport"
)

// Credentials are scoped to the exact resource URL, never to a display name.
type oauthStore struct { path string }
type oauthCredentials struct {
 ClientID string `json:"client_id"`
 ClientSecret string `json:"client_secret,omitempty"`
 Token *transport.Token `json:"token,omitempty"`
}
func oauthStoreFor(resource string) oauthStore {
 return oauthStore{filepath.Join(zotHome(), "mcp-oauth", fmt.Sprintf("%x.json", sha256.Sum256([]byte(resource))))}
}
func (s oauthStore) read() (oauthCredentials, error) {
 var c oauthCredentials
 data, err := os.ReadFile(s.path)
 if errors.Is(err, os.ErrNotExist) { return c, nil }
 if err != nil { return c, err }
 if json.Unmarshal(data, &c) != nil { return c, errors.New("invalid OAuth credential file") }
 return c, nil
}
func (s oauthStore) save(c oauthCredentials) error {
 if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil { return err }
 data, err := json.Marshal(c)
 if err != nil { return errors.New("cannot encode OAuth credentials") }
 return writeFileAtomic(s.path, data, 0600)
}
func (s oauthStore) GetToken(ctx context.Context) (*transport.Token, error) {
 if err := ctx.Err(); err != nil { return nil, err }
 c, err := s.read()
 if err != nil { return nil, err }
 if c.Token == nil { return nil, transport.ErrNoToken }
 return c.Token, nil
}
func (s oauthStore) SaveToken(ctx context.Context, token *transport.Token) error {
 if err := ctx.Err(); err != nil { return err }
 c, err := s.read()
 if err != nil { return err }
 c.Token = token
 return s.save(c)
}
func (s *managedServer) oauthConfig() (*transport.OAuthConfig, error) {
 store := oauthStoreFor(s.config.URL)
 c, err := store.read()
 if err != nil { return nil, err }
 if c.ClientID == "" { return nil, nil }
 return &transport.OAuthConfig{ClientID:c.ClientID, ClientSecret:c.ClientSecret, TokenStore:store, PKCEEnabled:true}, nil
}

// Login is explicit: discovery and background tool calls never open a browser.
// ponytail: public clients with dynamic registration only; add pre-registered clients when needed.
func (s *managedServer) login(ctx context.Context, showURL func(string)) error {
 if s.config.Transport != "streamable-http" && s.config.Transport != "sse" { return errors.New("OAuth login requires an HTTP transport") }
 u, err := url.Parse(s.config.URL)
 if err != nil || u.Scheme != "https" || u.Host == "" { return errors.New("OAuth login requires an HTTPS resource URL") }
 ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
 defer cancel()
 listener, err := net.Listen("tcp", "127.0.0.1:0")
 if err != nil { return err }
 defer listener.Close()
 redirect := "http://"+listener.Addr().String()+"/callback"
 state, err := transport.GenerateState()
 if err != nil { return err }
 verifier, err := transport.GenerateCodeVerifier()
 if err != nil { return err }
 codes := make(chan string, 1)
 mux := http.NewServeMux()
 mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
  w.Header().Set("Cache-Control", "no-store")
  if r.Method != http.MethodGet || r.URL.Query().Get("state") != state { http.Error(w,"Invalid OAuth callback",400); return }
  code := r.URL.Query().Get("code")
  if code == "" { http.Error(w,"Authorization declined or missing code",400); return }
  select { case codes <- code: fmt.Fprint(w,"Authorization received. Return to zot."); default: http.Error(w,"Callback already received",409) }
 })
 server := &http.Server{Handler:mux, ReadHeaderTimeout:5*time.Second}
 defer server.Close()
 go server.Serve(listener)
 // Discover the challenge without forwarding configured credentials to redirects.
 hc := &http.Client{Timeout:30*time.Second, CheckRedirect:func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
 req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.URL, nil)
 if err != nil { return err }
 req.Header.Set("Accept", "application/json, text/event-stream")
 resp, err := hc.Do(req)
 if err != nil { return errors.New("OAuth resource discovery failed") }
 resp.Body.Close()
 store := oauthStoreFor(s.config.URL)
 memory := transport.NewMemoryTokenStore()
 h := transport.NewOAuthHandler(transport.OAuthConfig{RedirectURI:redirect, PKCEEnabled:true, TokenStore:memory, HTTPClient:hc})
 h.SetBaseURL(s.config.URL)
 h.HandleUnauthorizedResponse(resp)
 if err := h.RegisterClient(ctx,"zot-mcp-bridge"); err != nil { return errors.New("OAuth client registration failed; server must support dynamic public-client registration") }
 h.SetExpectedState(state)
 authURL, err := h.GetAuthorizationURL(ctx,state,transport.GenerateCodeChallenge(verifier))
 if err != nil { return errors.New("OAuth authorization URL discovery failed") }
 parsed, err := url.Parse(authURL)
 if err != nil || parsed.Scheme != "https" { return errors.New("OAuth authorization endpoint must use HTTPS") }
 showURL(authURL)
 select {
 case <-ctx.Done(): return ctx.Err()
 case code := <-codes:
  if err := h.ProcessAuthorizationResponse(ctx,code,state,verifier); err != nil { return errors.New("OAuth code exchange failed") }
 }
 token, err := memory.GetToken(ctx)
 if err != nil { return err }
 if err := store.save(oauthCredentials{ClientID:h.GetClientID(), ClientSecret:h.GetClientSecret(), Token:token}); err != nil { return err }
 return nil
}
