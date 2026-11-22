package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// An image-only tool result must not serialize to an empty
// function_call_output (the Responses API may reject it) and a
// following user-message image must serialize as input_image so the
// model actually receives the bytes.
func TestCodexImageToolResultMirror(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "").(*codexClient)

	wire, err := c.buildRequest(Request{
		Model: "gpt-5.5",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "look at this"}}},
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "call_1", Name: "read", Arguments: []byte(`{"path":"x.png"}`)},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "call_1", Content: []Content{
					ImageBlock{MimeType: "image/png", Data: []byte("png-bytes")},
				}},
			}},
			// The agent loop appends this mirror after an image tool result.
			{Role: RoleUser, Content: []Content{
				TextBlock{Text: "Tool output included the following image content:"},
				ImageBlock{MimeType: "image/png", Data: []byte("png-bytes")},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var sawFnOutput, sawInputImage bool
	for _, item := range wire.Input {
		switch v := item.(type) {
		case codexFunctionCallOutput:
			sawFnOutput = true
			if strings.TrimSpace(v.Output) == "" {
				t.Fatalf("image-only tool result produced empty function_call_output")
			}
			if !strings.Contains(strings.ToLower(v.Output), "image") {
				t.Fatalf("placeholder should mention image, got %q", v.Output)
			}
		case codexInputMessage:
			for _, ct := range v.Content {
				if img, ok := ct.(codexInputImage); ok && img.Type == "input_image" {
					sawInputImage = true
				}
			}
		}
	}
	if !sawFnOutput {
		t.Fatalf("no function_call_output emitted")
	}
	if !sawInputImage {
		t.Fatalf("mirrored user image was not serialized as input_image")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func TestCopilotResponsesToolImagesReachModelOnce(t *testing.T) {
	c := NewGithubCopilotClient("synthetic-token").(*copilotClient).router.byAPI[APIResponses].(*codexClient)
	image := ImageBlock{MimeType: "image/png", Data: []byte("png-bytes")}
	messages := []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "look"}}},
		{Role: RoleAssistant, Content: []Content{
			ToolCallBlock{ID: "call_1", Name: "read", Arguments: []byte(`{"path":"x.png"}`)},
			ToolCallBlock{ID: "call_2", Name: "read", Arguments: []byte(`{"path":"y.txt"}`)},
		}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call_1", Content: []Content{image}}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call_2", Content: []Content{TextBlock{Text: "text result"}}}}},
	}
	for _, tc := range []struct {
		name     string
		messages []Message
	}{
		{"synthesized", messages},
		{"persisted mirror", append(append([]Message{}, messages...), Message{Role: RoleUser, Content: []Content{TextBlock{Text: "Tool output included the following image content:"}, image}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := c.buildRequest(Request{Model: "gpt-5.5", Messages: tc.messages})
			if err != nil {
				t.Fatal(err)
			}
			images := 0
			var outputs []string
			for _, item := range wire.Input {
				if output, ok := item.(codexFunctionCallOutput); ok {
					outputs = append(outputs, output.CallID)
				}
				if message, ok := item.(codexInputMessage); ok {
					for _, content := range message.Content {
						if _, ok := content.(codexInputImage); ok {
							images++
							if len(outputs) != 2 || outputs[0] != "call_1" || outputs[1] != "call_2" {
								t.Errorf("tool outputs before image = %v, want [call_1 call_2]", outputs)
							}
						}
					}
				}
			}
			if images != 1 {
				t.Fatalf("input images=%d, want 1", images)
			}
		})
	}
}

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCodexPreviewModelUsesCodexCLIShape(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "https://example.test/backend-api/codex/responses").(*codexClient)
	var gotReq *http.Request
	var gotBody codexRequest
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	events, err := c.Stream(context.Background(), Request{
		Model:    "gpt-5.6-terra",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q", gotReq.Header.Get("originator"))
	}
	if gotReq.Header.Get("user-agent") != "codex_cli_rs/0.0.0" {
		t.Fatalf("user-agent = %q", gotReq.Header.Get("user-agent"))
	}
	if gotBody.PromptCacheKey == "" {
		t.Fatal("prompt_cache_key was not set")
	}
	if gotReq.Header.Get("session-id") != gotBody.PromptCacheKey {
		t.Fatalf("session-id = %q, prompt_cache_key = %q", gotReq.Header.Get("session-id"), gotBody.PromptCacheKey)
	}
}

func TestGPT56UsesNativeMaxReasoningEffort(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "").(*codexClient)
	wire, err := c.buildRequest(Request{Model: "gpt-5.6-sol", Reasoning: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Reasoning == nil || wire.Reasoning.Effort != "max" {
		t.Fatalf("reasoning = %+v", wire.Reasoning)
	}
}

func TestResponsesRequestUsesExplicitReasoningLevelMap(t *testing.T) {
	SetLiveModels([]Model{{
		Provider:          "custom-responses",
		ID:                "reasoning-model",
		API:               APIResponses,
		Reasoning:         true,
		ReasoningLevelMap: map[string]string{"max": "max"},
	}})
	t.Cleanup(func() { SetLiveModels(nil) })

	named := NewOpenAIResponsesNamed("token", "https://example.test/v1", "custom-responses").(*renamedClient)
	c := named.inner.(*codexClient)
	wire, err := c.buildRequest(Request{Model: "reasoning-model", Reasoning: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Reasoning == nil || wire.Reasoning.Effort != "max" {
		t.Fatalf("reasoning = %+v", wire.Reasoning)
	}
}

func TestXAIResponsesUsesNamedProviderCatalogAndEndpoint(t *testing.T) {
	named := NewOpenAIResponsesNamed("token", "https://api.x.ai/v1", "xai").(*renamedClient)
	c := named.inner.(*codexClient)

	if c.baseURL != "https://api.x.ai/v1/responses" {
		t.Fatalf("base URL = %q", c.baseURL)
	}
	for _, model := range []string{"grok-4.5", "grok-build-0.1"} {
		if _, err := c.buildRequest(Request{Model: model}); err != nil {
			t.Errorf("build request for %s: %v", model, err)
		}
	}
}

func TestOpenAIGPT56DoesNotUseCodexCLIRouting(t *testing.T) {
	named := NewOpenAIResponsesNamed("token", "https://example.test/v1/responses", "openai").(*renamedClient)
	c := named.inner.(*codexClient)
	var gotReq *http.Request
	var gotBody codexRequest
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	events, err := c.Stream(context.Background(), Request{
		Model:    "gpt-5.6-sol",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("session-id") != "" {
		t.Fatalf("session-id = %q", gotReq.Header.Get("session-id"))
	}
	if gotBody.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key = %q", gotBody.PromptCacheKey)
	}
}

func TestCodexNestedStreamError(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "").(*codexClient)
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("data: {\"type\":\"error\",\"error\":{\"code\":\"model_not_available\",\"message\":\"limited preview\"}}\n\n")),
	}
	out := make(chan Event, 16)
	go c.runStream(context.Background(), resp, Request{Model: "gpt-5.6-sol"}, out)

	var got error
	for ev := range out {
		if done, ok := ev.(EventDone); ok {
			got = done.Err
		}
	}
	if got == nil || got.Error() != "codex error: limited preview" {
		t.Fatalf("error = %v", got)
	}
}

// TestCodexSubscriptionAlwaysUsesCodexCLIShape pins the ChatGPT
// subscription backend to the Codex CLI request identity for every
// model. The backend load-sheds unrecognized originator/user-agent
// pairs with "Our servers are currently overloaded" stream errors
// even when capacity is fine, so the CLI shape is required for
// reliable service, not just for preview-model admission.
func TestCodexSubscriptionAlwaysUsesCodexCLIShape(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "https://example.test/backend-api/codex/responses").(*codexClient)
	var gotReq *http.Request
	var body bytes.Buffer
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r
		_, _ = body.ReadFrom(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	events, err := c.Stream(context.Background(), Request{
		Model:    "gpt-5.6-sol",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q", gotReq.Header.Get("originator"))
	}
	if gotReq.Header.Get("user-agent") != "codex_cli_rs/0.0.0" {
		t.Fatalf("user-agent = %q", gotReq.Header.Get("user-agent"))
	}
	if gotReq.Header.Get("session-id") == "" {
		t.Fatal("session-id header missing")
	}
	if !strings.Contains(body.String(), "prompt_cache_key") {
		t.Fatalf("request missing prompt_cache_key: %s", body.String())
	}
}

func TestCodexRetriesUnsupportedPromptCacheRetentionWithNativeIdentity(t *testing.T) {
	c := NewOpenAICodex("token", "acct", "https://example.test/backend-api/codex/responses").(*codexClient)
	var headers []http.Header
	var bodies []string
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		headers = append(headers, r.Header.Clone())
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(body))
		if len(headers) == 1 {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{
					"error": {
						"message": "prompt_cache_retention is not supported on this model",
						"type": "invalid_request_error",
						"param": "prompt_cache_retention",
						"code": "invalid_parameter"
					}
				}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	events, err := c.Stream(context.Background(), Request{
		Model:    "gpt-5.6-sol",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for event := range events {
		if done, ok := event.(EventDone); ok && done.Err != nil {
			t.Fatalf("retry stream failed: %v", done.Err)
		}
	}

	if len(headers) != 2 {
		t.Fatalf("requests = %d, want 2", len(headers))
	}
	if got := headers[0].Get("originator"); got != "codex_cli_rs" {
		t.Fatalf("initial originator = %q", got)
	}
	if got := headers[1].Get("originator"); got != "zot" {
		t.Fatalf("retry originator = %q", got)
	}
	sessionID := headers[1].Get("session-id")
	if sessionID == "" || headers[1].Get("x-client-request-id") != sessionID {
		t.Fatalf("retry session headers = %#v", headers[1])
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], `"prompt_cache_key"`) {
		t.Fatalf("retry body did not preserve prompt cache key: %v", bodies)
	}
}
