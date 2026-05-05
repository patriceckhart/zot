package auth

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// LoginResult is delivered on the channel returned by Server.Result().
type LoginResult struct {
	Provider string
	Method   string // "apikey" | "oauth"
	APIKey   string // populated when Method == "apikey"
	Code     string // populated when Method == "oauth"
	State    string // OAuth state (caller should verify)
	Err      error
}

// Server is a tiny local HTTP server used by the login flows. It binds
// to 127.0.0.1 on a random free port and serves:
//
//	GET  /                      landing page (menu)
//	GET  /apikey?provider=...   API key form
//	POST /apikey                form submit -> probes -> stores via caller
//	GET  /callback              OAuth callback (query: code, state)
//	GET  /success               generic success page
//	GET  /error                 generic error page
//
// The caller receives login events on Result(). The server stays up
// until Shutdown() is called.
type Server struct {
	l        net.Listener
	srv      *http.Server
	baseURL  string
	results  chan LoginResult
	probeFn  func(ctx context.Context, provider, key string) error
	mu       sync.Mutex
	shutdown bool
}

// NewServer starts a new login server on a random free port bound to loopback.
func NewServer() (*Server, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		l:       l,
		baseURL: "http://" + l.Addr().String(),
		results: make(chan LoginResult, 4),
		probeFn: ProbeAPIKey,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/apikey", s.handleAPIKey)
	mux.HandleFunc("/callback", s.handleCallback)
	mux.HandleFunc("/success", s.handleSuccess)
	mux.HandleFunc("/error", s.handleError)
	mux.HandleFunc("/logo.png", serveLogo)
	s.srv = &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	go func() { _ = s.srv.Serve(l) }()
	return s, nil
}

// URL returns the base URL the server is listening on.
func (s *Server) URL() string { return s.baseURL }

// Port returns the TCP port the server is bound to.
func (s *Server) Port() int {
	return s.l.Addr().(*net.TCPAddr).Port
}

// Result returns the channel receiving LoginResult events.
func (s *Server) Result() <-chan LoginResult { return s.results }

// Shutdown stops the server. It is safe to call multiple times.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.shutdown = true
	s.mu.Unlock()
	return s.srv.Shutdown(ctx)
}

// ---- handlers ----

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tpl.ExecuteTemplate(w, "index", map[string]any{
		"Port": s.Port(),
	})
}

func (s *Server) handleAPIKey(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		provider := r.URL.Query().Get("provider")
		if provider != "anthropic" && provider != "openai" && provider != "kimi" {
			http.Error(w, "provider must be anthropic, openai, or kimi", http.StatusBadRequest)
			return
		}
		tpl.ExecuteTemplate(w, "apikey", map[string]any{"Provider": provider})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		provider := strings.TrimSpace(r.FormValue("provider"))
		key := strings.TrimSpace(r.FormValue("api_key"))
		if provider == "" || key == "" {
			s.errorPage(w, "missing provider or api key")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := s.probeFn(ctx, provider, key); err != nil {
			s.errorPage(w, err.Error())
			s.results <- LoginResult{Provider: provider, Method: "apikey", Err: err}
			return
		}
		s.successPage(w, provider, "api key")
		s.results <- LoginResult{Provider: provider, Method: "apikey", APIKey: key}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	provider := q.Get("provider")
	if provider == "" {
		// Some OAuth providers don't echo back custom params; try state-encoded form.
		provider = decodeStateProvider(q.Get("state"))
	}
	if errParam := q.Get("error"); errParam != "" {
		msg := errParam
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		s.errorPage(w, msg)
		s.results <- LoginResult{Provider: provider, Method: "oauth", Err: fmt.Errorf(msg)}
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" {
		s.errorPage(w, "missing authorization code")
		s.results <- LoginResult{Provider: provider, Method: "oauth", Err: fmt.Errorf("missing code")}
		return
	}
	s.successPage(w, provider, "subscription")
	s.results <- LoginResult{Provider: provider, Method: "oauth", Code: code, State: state}
}

func (s *Server) handleSuccess(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.successPage(w, q.Get("provider"), q.Get("method"))
}

func (s *Server) handleError(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r.URL.Query().Get("message"))
}

func (s *Server) successPage(w http.ResponseWriter, provider, method string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	tpl.ExecuteTemplate(w, "success", map[string]any{
		"Provider": provider,
		"Method":   method,
	})
}

func (s *Server) errorPage(w http.ResponseWriter, msg string) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	tpl.ExecuteTemplate(w, "error", map[string]any{"Message": msg})
}

// decodeStateProvider extracts the provider from a state string of the
// form "<provider>:<nonce>".
func decodeStateProvider(state string) string {
	if i := strings.IndexByte(state, ':'); i > 0 {
		return state[:i]
	}
	return ""
}

// BuildRedirectURI returns the callback URL the OAuth server should
// redirect to.
func (s *Server) BuildRedirectURI() string {
	return s.baseURL + "/callback"
}

// Redirect sends an HTTP redirect to u. Used by the TUI to tell the
// browser to bounce through our local server. Not currently used; kept
// for future flows.
func Redirect(w http.ResponseWriter, r *http.Request, u *url.URL) {
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// ---- templates ----
//
// All pages share the monochrome monoStyle defined in callback.go so
// the browser tab looks like the tui: black on white, monospace, thin
// rules, no rounded boxes, no color.

var tpl = template.Must(template.New("index").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"/><title>zot login</title>` + monoStyle + `</head><body>
` + logoTag + `
<h1><span class="zot">zot</span> login</h1>
<hr class="rule">
<p>paste an api key for anthropic, openai, or kimi. <span class="zot">zot</span> probes the provider once, then saves the key to <span class="mono">~/Library/Application Support/zot/auth.json</span>.</p>
<p>
  <a href="/apikey?provider=anthropic">anthropic api key →</a><br>
  <a href="/apikey?provider=openai">openai api key →</a><br>
  <a href="/apikey?provider=kimi">kimi api key →</a>
</p>
<hr class="rule">
<p class="muted">for a subscription login (claude pro/max - chatgpt plus/pro - kimi code), close this tab and run /login inside <span class="zot">zot</span>.</p>
</body></html>`))

func init() {
	template.Must(tpl.New("apikey").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"/><title>zot login</title>` + monoStyle + `<style>
  form { display: flex; flex-direction: column; gap: 0.75rem; }
  label { font-size: 0.875rem; }
</style></head><body>
` + logoTag + `
<h1><span class="zot">zot</span> login - {{.Provider}} api key</h1>
<hr class="rule">
<p>paste your {{.Provider}} api key. <span class="zot">zot</span> will probe the provider with it once, then save it if the key is accepted.</p>
<form method="POST" action="/apikey">
  <input type="hidden" name="provider" value="{{.Provider}}" />
  <label for="api_key">api key</label>
  <input id="api_key" name="api_key" type="password" autocomplete="off" autofocus />
  <button type="submit">log in</button>
</form>
</body></html>`))

	template.Must(tpl.New("success").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"/><title>zot - logged in</title>` + monoStyle + `</head><body>
` + logoTag + `
<h1><span class="mark">✓</span> logged in to {{.Provider}}</h1>
<hr class="rule">
<p class="msg">method: {{.Method}}</p>
<p class="muted"><span class="zot">zot</span> received the callback. you can close this tab and return to the terminal.</p>
</body></html>`))

	template.Must(tpl.New("error").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"/><title>zot - error</title>` + monoStyle + `</head><body>
` + logoTag + `
<h1><span class="mark">✗</span> login failed</h1>
<hr class="rule">
<p class="msg mono">{{.Message}}</p>
<p class="muted">go back to <span class="zot">zot</span> and try again.</p>
</body></html>`))
}
