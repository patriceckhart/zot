package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func transformFixture() provider.Request {
	return provider.Request{Model: "model", System: "system secret", Messages: []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{
			provider.TextBlock{Text: "secret", Format: "markdown"},
			provider.TextBlock{Text: "signed secret", ThoughtSignature: "signature"},
			provider.ImageBlock{Data: []byte("image secret"), MimeType: "image/png"},
			provider.ReasoningBlock{Summary: "reasoning secret", Encrypted: "ciphertext"},
		}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call", Name: "tool", Arguments: json.RawMessage(`{ "path": "secret", "nested": [1,true,null] }`)}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call", Content: []provider.Content{provider.ToolResultBlock{CallID: "nested", Content: []provider.Content{provider.TextBlock{Text: "result secret"}}}}}}},
	}, Tools: []provider.Tool{{Name: "tool", Description: "tool secret", Schema: json.RawMessage(`{"type":"object","description":"schema secret"}`), NamespaceInstructions: "namespace secret"}}}
}

func TestRequestTransformSegmentsAndCopy(t *testing.T) {
	req := transformFixture()
	before := transformFixture()
	segments, err := RequestSegments(req)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]RequestSegment)
	for _, s := range segments {
		byID[s.ID] = s
	}
	for _, id := range []string{"system", "m0.c0", "m1.c0", "m2.c0.r.c0.r.c0", "t0.description", "t0.schema", "t0.namespace_instructions"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing segment %s", id)
		}
	}
	for _, id := range []string{"m0.c1", "m0.c2", "m0.c3"} {
		s := byID[id]
		if s.Skipped == "" || s.Text != "" || len(s.Value) != 0 {
			t.Errorf("opaque content exposed: %s", id)
		}
	}
	replacements := map[string]json.RawMessage{
		"system":          json.RawMessage(`"clean system"`),
		"m0.c0":           json.RawMessage(`""`),
		"m0.c1":           json.RawMessage(`"cannot change signed text"`),
		"m1.c0":           json.RawMessage(`{"path":"clean","nested":[2,false,null]}`),
		"m2.c0.r.c0.r.c0": json.RawMessage(`"clean result"`),
		"t0.description":  json.RawMessage(`"clean description"`),
		"t0.schema":       json.RawMessage(`{"type":"object","description":"clean schema"}`),
		"unknown":         json.RawMessage(`{"ignored":true}`),
	}
	out, err := ReplaceRequestSegments(req, replacements)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req, before) {
		t.Fatal("transcript or tool registry mutated")
	}
	if out.System != "clean system" || out.Messages[0].Content[0].(provider.TextBlock).Text != "" {
		t.Fatal("text not replaced")
	}
	if !reflect.DeepEqual(out.Messages[0].Content[1:], req.Messages[0].Content[1:]) {
		t.Fatal("opaque or signed data changed")
	}
	if out.Messages[0].Content[0].(provider.TextBlock).Format != "markdown" || out.Tools[0].Name != "tool" {
		t.Fatal("metadata changed")
	}
	if out.Tools[0].Description != "clean description" {
		t.Fatal("tool description not replaced")
	}
	unchanged, err := ReplaceRequestSegments(req, nil)
	if err != nil || !reflect.DeepEqual(unchanged, req) {
		t.Fatal("missing replacements changed bytes or metadata")
	}
}

func TestRequestTransformValidation(t *testing.T) {
	for _, raw := range []string{`null`, `42`, `{"different":"key"}`, `{"path":42,"nested":[1,true,null]}`, `{"path":"s","nested":[1,true]}`, `{"path":"s","nested":[1,true,null],"extra":0}`} {
		req := transformFixture()
		if _, err := ReplaceRequestSegments(req, map[string]json.RawMessage{"system": json.RawMessage(`"changed"`), "m1.c0": json.RawMessage(raw)}); err == nil {
			t.Errorf("accepted JSON shape change: %s", raw)
		}
		if req.System != "system secret" {
			t.Fatal("partial application mutated input")
		}
	}
	if _, err := ReplaceRequestSegments(transformFixture(), map[string]json.RawMessage{"system": json.RawMessage(`null`)}); err == nil {
		t.Fatal("accepted null text")
	}
	if _, err := RequestSegments(provider.Request{System: strings.Repeat("x", MaxRequestTransformBytes+1)}); err == nil {
		t.Fatal("accepted oversized text")
	}
	blocks := make([]provider.Content, MaxRequestTransformSegments)
	for i := range blocks {
		blocks[i] = provider.TextBlock{}
	}
	if _, err := RequestSegments(provider.Request{Messages: []provider.Message{{Content: blocks}}}); err == nil {
		t.Fatal("accepted excessive segments")
	}
}

func TestRequestTransformHash(t *testing.T) {
	req := transformFixture()
	a, _ := RequestSegments(req)
	req.Messages[1].Content[0] = provider.ToolCallBlock{Arguments: json.RawMessage(`{"nested":[1,true,null],"path":"secret"}`)}
	b, _ := RequestSegments(req)
	h := RequestTransformHash("p", "model", "chat", a)
	if h != RequestTransformHash("p", "model", "chat", b) {
		t.Fatal("JSON whitespace or key order changed hash")
	}
	for _, different := range []string{RequestTransformHash("other", "model", "chat", a), RequestTransformHash("p", "other", "chat", a), RequestTransformHash("p", "model", "compaction_side", a)} {
		if h == different {
			t.Fatal("request identity omitted from hash")
		}
	}
}

func TestBeforeRequestChatRetriesAndTranscript(t *testing.T) {
	client := &retryFakeClient{}
	a := NewAgent(client, "model", "base", nil)
	a.RetryBaseDelay = 1
	a.BeforeStart = func(_ context.Context, s string) string { return s + " prepared" }
	calls := 0
	a.BeforeRequest = func(_ context.Context, purpose string, req provider.Request) (provider.Request, error) {
		calls++
		if purpose != "chat" || req.System != "base prepared" {
			t.Error("wrong purpose or hook order")
		}
		return ReplaceRequestSegments(req, map[string]json.RawMessage{"m0.c0": json.RawMessage(`"redacted"`)})
	}
	if err := a.Prompt(context.Background(), "secret", nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || extractText(a.Messages()[0]) != "secret" {
		t.Fatal("retry hook missing or transcript mutated")
	}
}

func TestBeforeRequestToolLoop(t *testing.T) {
	tool := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	close(tool.release)
	a := NewAgent(&queueFakeClient{}, "model", "system", Registry{"echo": tool})
	calls := 0
	a.BeforeRequest = func(_ context.Context, purpose string, req provider.Request) (provider.Request, error) {
		calls++
		if calls == 1 {
			return req, nil
		}
		segments, err := RequestSegments(req)
		if err != nil {
			return req, err
		}
		for _, segment := range segments {
			if segment.Text == "tool ok" {
				return ReplaceRequestSegments(req, map[string]json.RawMessage{segment.ID: json.RawMessage(`"redacted result"`)})
			}
		}
		t.Error("tool result absent from next-turn transform")
		return req, nil
	}
	if err := a.Prompt(context.Background(), "work", nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("hook calls = %d, want two tool-loop turns", calls)
	}
	result := a.Messages()[2].Content[0].(provider.ToolResultBlock)
	if result.Content[0].(provider.TextBlock).Text != "tool ok" {
		t.Fatal("transcript tool result mutated")
	}
}

func TestBeforeRequestCompactionAndFailures(t *testing.T) {
	for _, mode := range []string{"side", "in_place", "fallback", "reject"} {
		t.Run(mode, func(t *testing.T) {
			client := &inPlaceClient{}
			a := NewAgent(client, "model", "system", nil)
			if mode != "side" {
				a.CompactionMode = "in_place"
			}
			if mode == "fallback" {
				client.firstErr = errors.New("maximum context length exceeded")
			}
			var purposes []string
			a.BeforeRequest = func(_ context.Context, purpose string, req provider.Request) (provider.Request, error) {
				purposes = append(purposes, purpose)
				if mode == "reject" {
					return req, errors.New("maximum context length exceeded")
				}
				return ReplaceRequestSegments(req, map[string]json.RawMessage{"system": json.RawMessage(`"transformed"`)})
			}
			before := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "secret"}}}}
			_, _, err := a.Summarize(context.Background(), before, "", nil)
			if mode == "reject" {
				if err == nil || len(client.requests) != 0 || len(purposes) != 1 || RetryableProviderError(err) || provider.IsContextOverflowError(err) {
					t.Fatal("transform failure retried, sent or ignored")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"compaction_in_place"}
			if mode == "side" {
				want = []string{"compaction_side"}
			}
			if mode == "fallback" {
				want = append(want, "compaction_side")
			}
			if !reflect.DeepEqual(purposes, want) {
				t.Fatalf("purposes=%v want %v", purposes, want)
			}
			for _, req := range client.requests {
				if req.System != "transformed" {
					t.Fatal("untransformed compaction request")
				}
			}
		})
	}
	client := &inPlaceClient{}
	a := NewAgent(client, "model", "system", nil)
	a.BeforeRequest = func(_ context.Context, _ string, req provider.Request) (provider.Request, error) {
		return req, errors.New("503 retry")
	}
	if err := a.Prompt(context.Background(), "secret", nil, nil); err == nil || len(client.requests) != 0 || RetryableProviderError(err) {
		t.Fatal("chat transform failure was not terminal")
	}
}
