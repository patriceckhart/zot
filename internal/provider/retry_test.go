package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestOpenAIStreamRetriesOn503 verifies the OpenAI/Kimi stream client
// silently retries when the upstream returns 503 a couple of times,
// then succeeds. The user-visible error path should NOT fire because
// a later attempt landed on a 200.
func TestOpenAIStreamRetriesOn503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream connect error"))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	evs, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("stream returned error: %v", err)
	}
	gotText := ""
	for ev := range evs {
		if td, ok := ev.(EventTextDelta); ok {
			gotText += td.Delta
		}
	}
	if gotText != "ok" {
		t.Fatalf("text=%q", gotText)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts=%d want 3", got)
	}
}

// TestOpenAIStreamSurfaces503AfterRetriesExhausted verifies that when
// every retry attempt returns 503 the caller sees a normal "http 503"
// error, not a retry-internal placeholder. The body bytes must be
// preserved so the rescue picker / red banner can show them.
func TestOpenAIStreamSurfaces503AfterRetriesExhausted(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream connect error"))
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err == nil {
		t.Fatalf("expected error after exhausted retries")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error %q should mention 503", err)
	}
	if got := attempts.Load(); got != streamRetryAttempts+1 {
		t.Fatalf("attempts=%d want %d", got, streamRetryAttempts+1)
	}
}

// TestOpenAIStreamDoesNotRetryOn400 ensures non-transient errors are
// returned immediately without retrying. We don't want to amplify
// load on a provider that's actively rejecting our request shape.
func TestOpenAIStreamDoesNotRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want 400 err got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts=%d want 1", got)
	}
}

func TestIsTransientHTTPStatus(t *testing.T) {
	for _, code := range []int{502, 503, 504} {
		if !isTransientHTTPStatus(code) {
			t.Errorf("%d should be transient", code)
		}
	}
	for _, code := range []int{200, 400, 401, 403, 404, 429, 500, 501} {
		if isTransientHTTPStatus(code) {
			t.Errorf("%d should NOT be transient", code)
		}
	}
}

func TestIsTransientConnectError(t *testing.T) {
	good := []string{
		"read tcp: connection reset by peer",
		"dial tcp: connection refused",
		"unexpected EOF",
		"upstream connect error or disconnect/reset before headers",
		"tls handshake error: bad cert",
		"i/o timeout",
	}
	for _, m := range good {
		if !isTransientConnectError(stringErr(m)) {
			t.Errorf("%q should be transient", m)
		}
	}
	bad := []string{
		"context canceled",
		"json: cannot unmarshal",
	}
	for _, m := range bad {
		if isTransientConnectError(stringErr(m)) {
			t.Errorf("%q should NOT be transient", m)
		}
	}
}

type stringErr string

func (s stringErr) Error() string { return string(s) }
