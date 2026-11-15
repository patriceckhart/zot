package core

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

type inPlaceClient struct {
	requests  []provider.Request
	firstErr  error
	setupErr  bool
	toolCall  bool
	responses [][]provider.Event
}

func (*inPlaceClient) Name() string { return "test" }
func (c *inPlaceClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	if index := len(c.requests) - 1; index < len(c.responses) {
		ch := make(chan provider.Event, len(c.responses[index]))
		for _, event := range c.responses[index] {
			ch <- event
		}
		close(ch)
		return ch, nil
	}
	var err error
	if len(c.requests) == 1 {
		err = c.firstErr
	}
	if c.setupErr && err != nil {
		return nil, err
	}
	ch := make(chan provider.Event, 3)
	if err == nil {
		ch <- provider.EventTextDelta{Delta: "summary"}
		if c.toolCall {
			ch <- provider.EventToolStart{ID: "unexpected", Name: "test"}
		}
	}
	ch <- provider.EventDone{Err: err}
	close(ch)
	return ch, nil
}

func TestInPlaceCompactionPrefix(t *testing.T) {
	for _, method := range []string{"compact", "summarize"} {
		t.Run(method, func(t *testing.T) {
			client := &inPlaceClient{}
			a := NewAgent(client, "test", "active system", NewRegistry(&outcomeTool{}))
			a.CompactionMode = "in_place"
			a.CompactionPrompt = "Keep exact paths."
			a.CompactionInstructions = "Keep blockers."
			a.Reasoning = "high"
			a.MaxTokens = 8192
			a.SessionID = "test-session"
			messages := []provider.Message{
				{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}, provider.ImageBlock{MimeType: "image/png", Data: []byte("image")}}},
				{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "progress"}}, Meta: map[string]string{provider.ReadableReasoningMetaKey: "display only", "replay": "retained"}},
				{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "recent tail"}}},
			}
			a.SetMessages(messages)
			var err error
			prefix := messages
			if method == "compact" {
				prefix = messages[:2]
				_, err = a.Compact(context.Background(), 1, nil)
			} else {
				_, _, err = a.Summarize(context.Background(), messages, "Keep next steps.", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			req := client.requests[0]
			if req.System != a.System || req.Model != a.Model || !reflect.DeepEqual(req.Tools, a.Tools.Specs()) || req.Reasoning != a.Reasoning || req.MaxTokens != a.MaxTokens || req.SessionID != a.SessionID {
				t.Fatalf("active request settings not preserved: %+v", req)
			}
			if len(req.Messages) != len(prefix)+1 || !reflect.DeepEqual(req.Messages[:len(prefix)], withoutToolDisplay(prefix)) {
				t.Fatalf("request prefix changed: %+v", req.Messages)
			}
			instruction := req.Messages[len(prefix)].Content[0].(provider.TextBlock).Text
			for _, want := range []string{a.CompactionPrompt, a.CompactionInstructions, "Do not continue the task or call tools"} {
				if !strings.Contains(instruction, want) {
					t.Fatalf("instruction missing %q: %s", want, instruction)
				}
			}
			if strings.Contains(instruction, compactionPrompt) {
				t.Fatal("custom template did not replace built-in template")
			}
			if method == "summarize" {
				if !strings.Contains(instruction, "Keep next steps.") || !reflect.DeepEqual(a.Messages(), messages) {
					t.Fatal("per-call instruction lost or Summarize changed transcript")
				}
			} else {
				after := a.Messages()
				if len(after) != 2 || !reflect.DeepEqual(after[1], messages[2]) {
					t.Fatalf("tail not preserved: %+v", after)
				}
			}
		})
	}
}

func TestInPlaceCompactionFallback(t *testing.T) {
	for _, setup := range []bool{false, true} {
		for _, message := range []string{"maximum context length exceeded", "HTTP 413 payload too large", "authentication failed"} {
			t.Run(message+"/setup="+strconv.FormatBool(setup), func(t *testing.T) {
				client := &inPlaceClient{firstErr: errors.New(message), setupErr: setup}
				a := NewAgent(client, "test", "active system", nil)
				a.CompactionMode = "in_place"
				before := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}}}
				a.SetMessages(before)
				_, err := a.Compact(context.Background(), 0, nil)
				if message == "authentication failed" {
					if err == nil || len(client.requests) != 1 || !reflect.DeepEqual(a.Messages(), before) {
						t.Fatal("non-overflow error retried or transcript changed")
					}
					return
				}
				if err != nil || len(client.requests) != 2 {
					t.Fatalf("fallback failed: requests=%d, err=%v", len(client.requests), err)
				}
				fallback := client.requests[1]
				if fallback.System != summarizationSystem || len(fallback.Tools) != 0 || len(fallback.Messages) != 1 {
					t.Fatalf("not a side-compaction request: %+v", fallback)
				}
			})
		}
	}
}

func TestInPlaceCompactionPreparesSystemAndRepairsToolPairs(t *testing.T) {
	client := &inPlaceClient{}
	a := NewAgent(client, "test", "base system", nil)
	a.CompactionMode = "in_place"
	a.BeforeStart = func(_ context.Context, system string) string { return system + " prepared" }
	before := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "pending", Name: "test"}}},
	}
	a.SetMessages(before)
	if _, _, err := a.Summarize(context.Background(), before, "", nil); err != nil {
		t.Fatal(err)
	}
	req := client.requests[0]
	if req.System != "base system prepared" {
		t.Fatalf("unprepared system: %q", req.System)
	}
	want := repairToolUseResultPairs(before)
	if len(req.Messages) != len(want)+1 || !reflect.DeepEqual(req.Messages[:len(want)], want) {
		t.Fatalf("tool pairs not repaired: %+v", req.Messages)
	}
	if !reflect.DeepEqual(a.Messages(), before) {
		t.Fatal("summary mutated transcript during tool-pair repair")
	}
}

func TestInPlaceCompactionToolRecovery(t *testing.T) {
	toolEvents := map[string][]provider.Event{
		"start":   {provider.EventToolStart{ID: "unexpected", Name: "outcome"}},
		"args":    {provider.EventToolArgs{Delta: "{}"}},
		"end":     {provider.EventToolEnd{}},
		"stop":    {provider.EventDone{Stop: provider.StopToolUse}},
		"message": {provider.EventDone{Message: provider.Message{Content: []provider.Content{provider.ToolCallBlock{ID: "unexpected", Name: "outcome"}}}}},
	}
	for name, events := range toolEvents {
		t.Run(name, func(t *testing.T) {
			for _, fallback := range []bool{false, true} {
				t.Run("fallback="+strconv.FormatBool(fallback), func(t *testing.T) {
					failed := append([]provider.Event{
						provider.EventTextDelta{Delta: "discarded partial summary"},
						provider.EventUsage{Usage: provider.Usage{InputTokens: 10}},
					}, events...)
					client := &inPlaceClient{responses: [][]provider.Event{failed}}
					if fallback {
						client.responses = append(client.responses, failed)
					}
					tool := &outcomeTool{}
					a := NewAgent(client, "test", "active system", NewRegistry(tool))
					var inputTokens int
					a.OnUsage = func(usage provider.Usage) { inputTokens = usage.InputTokens }
					a.CompactionMode = "in_place"
					a.CompactionPrompt = "Keep paths."
					a.CompactionInstructions = "Keep blockers."
					before := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}}}
					a.SetMessages(before)
					var purposes []string
					a.BeforeRequest = func(_ context.Context, purpose string, req provider.Request) (provider.Request, error) {
						purposes = append(purposes, purpose)
						return req, nil
					}
					summary, err := a.Compact(context.Background(), 0, nil)
					if err != nil || summary != "summary" {
						t.Fatalf("recovery failed: summary=%q, err=%v", summary, err)
					}
					wantPurposes := []string{"compaction_in_place", "compaction_in_place"}
					if fallback {
						wantPurposes = append(wantPurposes, "compaction_side")
						last := client.requests[len(client.requests)-1]
						if len(last.Tools) != 0 || last.System != customSummarizationSystem {
							t.Fatal("fallback did not use tool-free side request")
						}
					}
					if !reflect.DeepEqual(purposes, wantPurposes) {
						t.Fatalf("purposes=%v, want %v", purposes, wantPurposes)
					}
					first, retry := client.requests[0], client.requests[1]
					if first.System != retry.System || !reflect.DeepEqual(first.Tools, retry.Tools) || !reflect.DeepEqual(first.Messages[:len(before)], retry.Messages[:len(before)]) {
						t.Fatal("retry changed cache prefix")
					}
					instruction := extractText(retry.Messages[len(before)])
					for _, want := range []string{"No tools now. Just summarize.", a.CompactionPrompt, a.CompactionInstructions} {
						if !strings.Contains(instruction, want) {
							t.Fatalf("retry missing %q: %s", want, instruction)
						}
					}
					if strings.Contains(extractText(first.Messages[len(before)]), "No tools now. Just summarize.") {
						t.Fatal("retry mutated original request")
					}
					if got := a.Messages(); len(got) != 1 || extractText(got[0]) != "## Context Summary (compacted)\n\nsummary" {
						t.Fatalf("summary not committed or failed output retained: %+v", got)
					}
					wantInputTokens := 10
					if fallback {
						wantInputTokens = 20
					}
					if tool.called || inputTokens != wantInputTokens {
						t.Fatalf("tool called=%v, input tokens=%d, want %d", tool.called, inputTokens, wantInputTokens)
					}
				})
			}
		})
	}
}

func TestInPlaceCompactionToolRecoveryErrors(t *testing.T) {
	for _, mode := range []string{"overflow", "authentication", "transform", "cancelled", "fallback_failure"} {
		t.Run(mode, func(t *testing.T) {
			tool := []provider.Event{provider.EventToolStart{ID: "unexpected", Name: "test"}}
			client := &inPlaceClient{responses: [][]provider.Event{tool}}
			switch mode {
			case "overflow":
				client.responses = append(client.responses, []provider.Event{provider.EventDone{Err: errors.New("maximum context length exceeded")}})
			case "authentication":
				client.responses = append(client.responses, []provider.Event{provider.EventDone{Err: errors.New("authentication failed")}})
			case "fallback_failure":
				client.responses = append(client.responses, tool, []provider.Event{provider.EventDone{Err: errors.New("side failed")}})
			}
			a := NewAgent(client, "test", "system", nil)
			a.CompactionMode = "in_place"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			a.BeforeRequest = func(_ context.Context, _ string, req provider.Request) (provider.Request, error) {
				calls++
				if calls == 2 {
					if mode == "transform" {
						return req, errors.New("maximum context length exceeded")
					}
					if mode == "cancelled" {
						cancel()
						return req, ctx.Err()
					}
				}
				return req, nil
			}
			before := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}}}
			a.SetMessages(before)
			_, err := a.Compact(ctx, 0, nil)
			wantRequests := 2
			switch mode {
			case "overflow":
				wantRequests = 3
				if err != nil {
					t.Fatal(err)
				}
			case "fallback_failure":
				wantRequests = 3
			case "transform", "cancelled":
				wantRequests = 1
			}
			if len(client.requests) != wantRequests {
				t.Fatalf("requests=%d, want %d", len(client.requests), wantRequests)
			}
			if mode != "overflow" && (err == nil || !reflect.DeepEqual(a.Messages(), before)) {
				t.Fatalf("failed recovery changed transcript or succeeded: %v", err)
			}
		})
	}
}

func TestInPlaceCompactionFailurePreservesTranscript(t *testing.T) {
	for _, mode := range []string{"cancelled", "tool", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			client := &inPlaceClient{toolCall: mode == "tool"}
			a := NewAgent(client, "test", "active system", nil)
			a.CompactionMode = "in_place"
			ctx := context.Background()
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if mode == "invalid" {
				a.CompactionMode = "typo"
			}
			before := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}}}
			a.SetMessages(before)
			if _, err := a.Compact(ctx, 0, nil); err == nil || !reflect.DeepEqual(a.Messages(), before) {
				t.Fatalf("failed compaction changed transcript or succeeded: %v", err)
			}
			if mode == "tool" {
				if len(client.requests) != 3 {
					t.Fatalf("requests=%d, want two in-place attempts and one side fallback", len(client.requests))
				}
			} else if len(client.requests) > 1 {
				t.Fatal("unexpected retry or fallback")
			}
		})
	}
}
