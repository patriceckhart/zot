package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// OpenCode Go requires x-opencode-session for routing/caching; without it
// the endpoint 400s with MissingSessionID.
func TestOpenCodeGoSessionHeader(t *testing.T) {
	t.Run("chat completions", func(t *testing.T) {
		c := NewOpenAICompat("opencode-go", "key", "https://opencode.ai/zen/go/v1", "").(*openaiClient)
		var gotReq *http.Request
		c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotReq = r
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})

		events, err := c.Stream(context.Background(), Request{
			Model:     "deepseek-v4-flash",
			SessionID: "sess-123",
			Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for range events {
		}
		if gotReq == nil {
			t.Fatal("request was not sent")
		}
		if got := gotReq.Header.Get("x-opencode-session"); got != "sess-123" {
			t.Fatalf("x-opencode-session = %q", got)
		}
	})

	t.Run("responses", func(t *testing.T) {
		named := NewOpenAIResponsesNamed("key", "https://opencode.ai/zen/go/v1", "opencode-go").(*renamedClient)
		c := named.inner.(*codexClient)
		transport := c.http.Transport.(*openaiResponsesTransport)
		var gotReq *http.Request
		transport.inner = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotReq = r
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})

		events, err := c.Stream(context.Background(), Request{
			Model:     "gpt-5.6-luna",
			SessionID: "sess-456",
			Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for range events {
		}
		if gotReq == nil {
			t.Fatal("request was not sent")
		}
		if got := gotReq.Header.Get("x-opencode-session"); got != "sess-456" {
			t.Fatalf("x-opencode-session = %q", got)
		}
	})

	t.Run("other providers unaffected", func(t *testing.T) {
		c := NewOpenAICompat("openai", "key", "", "").(*openaiClient)
		var gotReq *http.Request
		c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotReq = r
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})

		events, err := c.Stream(context.Background(), Request{
			Model:     "gpt-5.6",
			SessionID: "sess-789",
			Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for range events {
		}
		if got := gotReq.Header.Get("x-opencode-session"); got != "" {
			t.Fatalf("x-opencode-session = %q, want empty", got)
		}
	})
}
