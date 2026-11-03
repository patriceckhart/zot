package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// openRouterAttributionHeaders is the documented app-attribution set.
// See https://openrouter.ai/docs/app-attribution
var openRouterAttributionHeaders = map[string]string{
	"HTTP-Referer":            "https://www.zot.sh",
	"X-OpenRouter-Title":      "zot",
	"X-Title":                 "zot",
	"X-OpenRouter-Categories": "cli-agent",
}

func checkOpenRouterAttribution(t *testing.T, h http.Header) {
	t.Helper()
	for name, want := range openRouterAttributionHeaders {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q; want %q", name, got, want)
		}
	}
}

func checkNoAttribution(t *testing.T, h http.Header) {
	t.Helper()
	for name := range openRouterAttributionHeaders {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q; want unset", name, got)
		}
	}
}

// rewriteTransport sends requests to a local test server while leaving the
// request URL intact, so host-based OpenRouter detection still sees
// openrouter.ai. No network traffic leaves the test. inner is resolved when
// the transport is built, so swapping http.DefaultTransport cannot recurse.
type rewriteTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	inner := t.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(clone)
}

func newRewriteTransport(t *testing.T, srv *httptest.Server) rewriteTransport {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return rewriteTransport{target: target, inner: http.DefaultTransport}
}

func localOpenRouterClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	return &http.Client{Transport: newRewriteTransport(t, srv)}
}

func TestOpenRouterStreamSendsAttributionHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkOpenRouterAttribution(t, r.Header)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := WithHTTPClient(NewOpenRouter("token", ""), localOpenRouterClient(t, srv))
	events, err := c.Stream(context.Background(), Request{
		Model:    "anthropic/claude-sonnet-4.5",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
}

func TestNonOpenRouterStreamOmitsAttributionHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkNoAttribution(t, r.Header)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAI("token", srv.URL)
	events, err := c.Stream(context.Background(), Request{
		Model:    "gpt-5",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
}

func TestAttributionHeadersForProviderNameAndHost(t *testing.T) {
	// Custom gateway pointed at OpenRouter: attributed even though the
	// provider name is not "openrouter".
	for _, base := range []string{"https://openrouter.ai/api/v1", "https://eu.openrouter.ai/api/v1"} {
		req, err := http.NewRequest("POST", chatCompletionsURL(base), nil)
		if err != nil {
			t.Fatal(err)
		}
		(&openaiClient{name: "my-gateway"}).setAttributionHeaders(req)
		checkOpenRouterAttribution(t, req.Header)
	}

	// Provider named openrouter with an overridden endpoint keeps sending
	// attribution; the user asked for OpenRouter.
	req, err := http.NewRequest("POST", "https://api.example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	NewOpenRouter("token", "https://api.example.com/v1").(*openaiClient).setAttributionHeaders(req)
	checkOpenRouterAttribution(t, req.Header)

	// Unrelated OpenAI-compatible provider: no attribution headers.
	other, err := http.NewRequest("POST", "https://api.example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	(&openaiClient{name: "openai"}).setAttributionHeaders(other)
	checkNoAttribution(t, other.Header)
}

func TestOpenRouterHostMatchesSubdomainsOnly(t *testing.T) {
	cases := map[string]bool{
		"openrouter.ai":         true,
		"OpenRouter.AI.":        true,
		"eu.openrouter.ai":      true,
		"notopenrouter.ai":      false,
		"openrouter.ai.evil.io": false,
		"api.openrouter.com":    false,
		"":                      false,
	}
	for host, want := range cases {
		if got := openRouterHost(host); got != want {
			t.Errorf("openRouterHost(%q) = %v; want %v", host, got, want)
		}
	}
}

func TestOpenRouterNonChatInferenceSendsAttributionHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkOpenRouterAttribution(t, r.Header)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	model := NonChatModel{
		Model: Model{Provider: "openrouter", ID: "test", API: "openrouter-images", BaseURL: openrouterDefaultBaseURL},
		Type:  "image",
		Input: []string{"text"},
	}
	client := NonChatClient{HTTP: localOpenRouterClient(t, srv)}
	if _, err := client.post(context.Background(), model.BaseURL+"/chat/completions", "synthetic", map[string]any{"model": model.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverOpenRouterSendsAttributionHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkOpenRouterAttribution(t, r.Header)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	original := http.DefaultTransport
	http.DefaultTransport = newRewriteTransport(t, srv)
	t.Cleanup(func() { http.DefaultTransport = original })

	if _, err := DiscoverOpenRouter(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}
