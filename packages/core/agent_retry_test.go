package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

type retryFakeClient struct {
	calls int32
}

func (c *retryFakeClient) Name() string { return "retry-fake" }

func (c *retryFakeClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "retry-fake", Model: req.Model}
		if call == 1 {
			out <- provider.EventDone{Stop: provider.StopError, Err: fmt.Errorf("anthropic overloaded_error: Overloaded")}
			return
		}
		out <- provider.EventTextDelta{Delta: "ok"}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "ok"}},
		}}
	}()
	return out, nil
}

func TestAgentRetriesOverloadedStreamError(t *testing.T) {
	client := &retryFakeClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	var turnErrs []string
	err := a.Prompt(context.Background(), "hello", nil, func(ev AgentEvent) {
		if e, ok := ev.(EvTurnEnd); ok && e.Err != nil {
			turnErrs = append(turnErrs, e.Err.Error())
		}
	})
	if err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 2 {
		t.Fatalf("Stream calls = %d; want 2", got)
	}
	if len(turnErrs) != 1 || !strings.Contains(turnErrs[0], "overloaded_error") {
		t.Fatalf("turn errors = %v; want one overloaded error before retry", turnErrs)
	}
	msgs := a.Messages()
	if len(msgs) != 2 {
		t.Fatalf("message count = %d; want user + final assistant", len(msgs))
	}
	if got := extractText(msgs[1]); got != "ok" {
		t.Fatalf("final assistant text = %q; want ok", got)
	}
}

// codexRetryFakeClient reproduces a transient OpenAI Codex backend
// failure on the first call and succeeds afterwards.
type codexRetryFakeClient struct {
	calls    int32
	firstErr string
}

func (c *codexRetryFakeClient) Name() string { return "openai-codex" }

func (c *codexRetryFakeClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "openai-codex", Model: req.Model}
		if call == 1 {
			out <- provider.EventDone{Stop: provider.StopError, Err: fmt.Errorf("%s", c.firstErr)}
			return
		}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "ok"}},
		}}
	}()
	return out, nil
}

func TestAgentRetriesCodexProcessingError(t *testing.T) {
	cases := []struct {
		name string
		err  string
	}{
		{
			name: "processing error",
			err:  "codex error: An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. Please include the request ID 60c8ebbd-20bd-42e4-b756-6e844041cfc0 in your message.",
		},
		{
			name: "servers overloaded",
			err:  "codex error: Our servers are currently overloaded. Please try again later.",
		},
		{
			name: "try again later only",
			err:  "codex error: Something went wrong. Please try again later.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &codexRetryFakeClient{firstErr: tc.err}
			a := NewAgent(client, "gpt-5.6-sol", "system", Registry{})
			a.RetryBaseDelay = time.Millisecond

			if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
				t.Fatalf("Prompt returned %v; want retry to succeed", err)
			}
			if got := atomic.LoadInt32(&client.calls); got != 2 {
				t.Fatalf("Stream calls = %d; want 2", got)
			}
			msgs := a.Messages()
			if len(msgs) != 2 || extractText(msgs[1]) != "ok" {
				t.Fatalf("messages = %v; want user + ok assistant", msgs)
			}
		})
	}
}

// TestCanRetryErrorCapacityMessages pins the classification of Codex
// capacity wording and makes sure quota/usage limits stay terminal even
// when they carry "try again later" style advice.
func TestCanRetryErrorCapacityMessages(t *testing.T) {
	a := NewAgent(nil, "gpt-5.6-sol", "system", Registry{})
	cases := []struct {
		msg  string
		want bool
	}{
		{"codex error: Our servers are currently overloaded. Please try again later.", true},
		{"codex error: Our servers are busy right now.", true},
		{"codex error: Please try again later.", true},
		{"codex error: You have hit your monthly usage limit. Try again later.", false},
		{"codex error: quota exceeded, try again later", false},
		{"codex error: unsupported parameter: reasoning", false},
	}
	for _, tc := range cases {
		if got := a.canRetryError(errors.New(tc.msg), 0); got != tc.want {
			t.Errorf("canRetryError(%q) = %v; want %v", tc.msg, got, tc.want)
		}
	}
}

type partialRetryFakeClient struct {
	calls int32
}

func (c *partialRetryFakeClient) Name() string { return "partial-retry-fake" }

func (c *partialRetryFakeClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "partial-retry-fake", Model: req.Model}
		if call == 1 {
			out <- provider.EventTextDelta{Delta: "partial"}
			out <- provider.EventDone{Stop: provider.StopError, Err: fmt.Errorf("provider returned error: 503"), Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "partial"}},
			}}
			return
		}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "recovered"}},
		}}
	}()
	return out, nil
}

func TestAgentDropsPartialAssistantBeforeRetry(t *testing.T) {
	client := &partialRetryFakeClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	msgs := a.Messages()
	if len(msgs) != 2 {
		t.Fatalf("message count = %d; want user + recovered assistant", len(msgs))
	}
	if got := extractText(msgs[1]); got != "recovered" {
		t.Fatalf("final assistant text = %q; want recovered", got)
	}
}

// captureClient records the last Request it received so tests can
// assert what the agent put on the wire.
type captureClient struct {
	lastReq provider.Request
}

func (c *captureClient) Name() string { return "capture" }

func (c *captureClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.lastReq = req
	out := make(chan provider.Event, 3)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "capture", Model: req.Model}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "ok"}},
		}}
	}()
	return out, nil
}

func TestAgentPropagatesMaxTokens(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.MaxTokens = 64000

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if client.lastReq.MaxTokens != 64000 {
		t.Fatalf("request MaxTokens = %d; want 64000 (Agent.MaxTokens not propagated)", client.lastReq.MaxTokens)
	}
}

func TestAgentPropagatesMaxToolCalls(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.MaxToolCalls = 4

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if client.lastReq.MaxToolCalls != 4 {
		t.Fatalf("request MaxToolCalls = %d; want 4", client.lastReq.MaxToolCalls)
	}
}

func TestAgentPropagatesTemperature(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	temp := float32(0)
	a.Temperature = &temp

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if client.lastReq.Temperature == nil || *client.lastReq.Temperature != temp {
		t.Fatalf("request Temperature = %v; want %v", client.lastReq.Temperature, temp)
	}
}
