package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/assets"
)

// CallbackResult is what an OAuth callback server returns once the
// browser hits the redirect URI.
type CallbackResult struct {
	Code    string
	State   string
	Err     error
	RawPath string
}

// CallbackServer is a single-shot OAuth callback listener bound to the
// fixed port that the provider has whitelisted for its client.
type CallbackServer struct {
	l        net.Listener
	srv      *http.Server
	provider OAuthProvider
	state    string
	result   chan CallbackResult
	once     sync.Once
}

// NewCallbackServer starts a listener on p.RedirectPort/p.RedirectPath.
// Returns an error if that port is already in use (e.g. another login
// flow already running, or the official CLI is running concurrently).
func NewCallbackServer(p OAuthProvider, expectedState string) (*CallbackServer, error) {
	addr := fmt.Sprintf("%s:%d", p.RedirectHost, p.RedirectPort)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w (is another zot/claude-code/codex login already running?)", addr, err)
	}
	cs := &CallbackServer{
		l:        l,
		provider: p,
		state:    expectedState,
		result:   make(chan CallbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(p.RedirectPath, cs.handle)
	mux.HandleFunc("/logo.png", serveLogo)
	cs.srv = &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	go func() { _ = cs.srv.Serve(l) }()
	return cs, nil
}

// URL returns the full redirect URI this server is listening on.
func (cs *CallbackServer) URL() string { return cs.provider.RedirectURI() }

// Result blocks until the callback is received or ctx is cancelled.
func (cs *CallbackServer) Result(ctx context.Context) (CallbackResult, error) {
	select {
	case r := <-cs.result:
		return r, nil
	case <-ctx.Done():
		return CallbackResult{}, ctx.Err()
	}
}

// Shutdown stops the server. Safe to call more than once.
func (cs *CallbackServer) Shutdown() {
	cs.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = cs.srv.Shutdown(ctx)
		cancel()
	})
}

func (cs *CallbackServer) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if errParam := q.Get("error"); errParam != "" {
		msg := errParam
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(oauthErrorHTML(msg)))
		cs.deliver(CallbackResult{Err: fmt.Errorf(msg), RawPath: r.URL.RequestURI()})
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(oauthErrorHTML("missing authorization code")))
		cs.deliver(CallbackResult{Err: fmt.Errorf("missing code"), RawPath: r.URL.RequestURI()})
		return
	}
	if cs.state != "" && state != cs.state {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(oauthErrorHTML("state mismatch")))
		cs.deliver(CallbackResult{Err: fmt.Errorf("state mismatch"), RawPath: r.URL.RequestURI()})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(oauthSuccessHTML(cs.provider.Name)))
	cs.deliver(CallbackResult{Code: code, State: state, RawPath: r.URL.RequestURI()})
}

func (cs *CallbackServer) deliver(r CallbackResult) {
	select {
	case cs.result <- r:
	default:
	}
}

// ---- static HTML for the callback tab ----

// All zot-served pages share a single style: white background, black
// text, the cyan pixel-art `z` logo at the top, thin black rules,
// monospace type.
const monoStyle = `<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; }
  body {
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    background: #ffffff;
    color: #000000;
    max-width: 44rem;
    margin: 0 auto;
    padding: 3rem 1.5rem;
    line-height: 1.55;
  }
  .logo {
    display: block;
    width: 120px;
    height: auto;
    image-rendering: pixelated;
    image-rendering: crisp-edges;
    margin: 0 0 1.5rem;
  }
  h1 {
    font-size: 1rem;
    font-weight: 600;
    margin: 0 0 0.25rem;
    letter-spacing: 0.01em;
  }
  h1 .mark { display: inline-block; width: 1.25rem; }
  .rule { border: 0; border-top: 1px solid #000000; margin: 1.5rem 0; }
  .muted { color: #595959; }
  .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; word-break: break-all; }
  .msg { padding: 0.75rem 0; }
  a { color: inherit; }
  input[type=password], input[type=text] {
    width: 100%; padding: 0.5rem 0.6rem;
    border: 1px solid #000000; background: #ffffff; color: #000000;
    font-family: inherit; font-size: 0.95rem;
  }
  button {
    padding: 0.5rem 1.25rem;
    background: #000000; color: #ffffff;
    border: 1px solid #000000; font-family: inherit; font-size: 0.95rem;
    cursor: pointer;
  }
  button:hover { background: #ffffff; color: #000000; }
</style>`

// logoTag is the <img> element used at the top of every zot-served
// page. The image bytes are served from /logo.png by the same server.
const logoTag = `<img class="logo" src="/logo.png" alt="zot" />`

// serveLogo writes the embedded PNG with appropriate caching headers.
// Shared between the oauth callback server and the api-key login server.
func serveLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "image/png")
	w.Header().Set("cache-control", "public, max-age=86400")
	_, _ = w.Write(assets.LogoPNG)
}

func oauthSuccessHTML(provider string) string {
	p := strings.ToLower(provider)
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"/><title>zot - logged in</title>` + monoStyle + `</head><body>
` + logoTag + `
<h1><span class="mark">✓</span> logged in to ` + p + `</h1>
<hr class="rule">
<p class="msg">zot received the callback. you can close this tab.</p>
</body></html>`
}

func oauthErrorHTML(msg string) string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"/><title>zot · error</title>` + monoStyle + `</head><body>
` + logoTag + `
<h1><span class="mark">✗</span> login failed</h1>
<hr class="rule">
<p class="msg mono">` + htmlEscape(msg) + `</p>
<p class="muted">go back to zot and try again.</p>
</body></html>`
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return r.Replace(s)
}
