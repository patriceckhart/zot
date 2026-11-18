package extensions

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestRequestTransformPrependUserContext(t *testing.T) {
	m := New(t.TempDir(), "", "test", "provider", "model", nil)
	addRequestExtension(t, m, "bootstrap", extproto.RequestTransformOptions{}, func(ev extproto.RequestTransformFromHost) []byte {
		return []byte(`{"type":"request_transform_response","id":"` + ev.ID + `","prepend_user_context":"bootstrap"}`)
	})
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "question"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call", Name: "read", Arguments: json.RawMessage(`{}`)}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call", Content: []provider.Content{provider.TextBlock{Text: "result"}}}}},
	}
	req := provider.Request{Model: "model", System: "system", Messages: messages}
	for range 2 {
		out, err := m.TransformRequest(context.Background(), "session", "provider", "chat", req)
		if err != nil {
			t.Fatal(err)
		}
		if out.System != req.System || len(out.Messages) != len(messages)+1 {
			t.Fatalf("unexpected transformed request: system changed=%t, messages=%d", out.System != req.System, len(out.Messages))
		}
		if out.Messages[0].Role != provider.RoleUser || !reflect.DeepEqual(out.Messages[0].Content, []provider.Content{provider.TextBlock{Text: "bootstrap"}}) {
			t.Fatal("context is not a user text message")
		}
		if !reflect.DeepEqual(out.Messages[1:], messages) || !reflect.DeepEqual(req.Messages, messages) {
			t.Fatal("context injection changed history or tool pairing")
		}
		out.Messages[1] = provider.Message{}
		if !reflect.DeepEqual(req.Messages, messages) || req.Messages[0].Role != provider.RoleUser {
			t.Fatal("context injection shares the message slice with history")
		}
	}
}

func TestRequestTransformUserContextValidation(t *testing.T) {
	for _, tc := range []struct {
		name, purpose, raw string
		valid              bool
	}{
		{"text", "chat", `"context"`, true},
		{"empty", "chat", `""`, true},
		{"null", "chat", `null`, false},
		{"number", "chat", `42`, false},
		{"object", "chat", `{}`, false},
		{"oversized", "chat", `"` + strings.Repeat("s", 1<<20) + `"`, false},
		{"side-compaction", "compaction_side", `"context"`, false},
		{"in-place-compaction", "compaction_in_place", `"context"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, policy := range []string{"fail", "pass"} {
				m := New(t.TempDir(), "", "test", "provider", "model", nil)
				addRequestExtension(t, m, "bootstrap", extproto.RequestTransformOptions{OnTimeout: policy}, func(ev extproto.RequestTransformFromHost) []byte {
					return []byte(`{"type":"request_transform_response","id":"` + ev.ID + `","replacements":{"system":"changed"},"prepend_user_context":` + tc.raw + `}`)
				})
				req := provider.Request{System: "original"}
				out, err := m.TransformRequest(context.Background(), "session", "provider", tc.purpose, req)
				if (err == nil) != (tc.valid || policy == "pass") {
					t.Fatalf("policy %s: unexpected error %v", policy, err)
				}
				if !tc.valid && !reflect.DeepEqual(out, req) {
					t.Fatal("invalid context partially applied the response")
				}
				if err != nil && strings.Contains(err.Error(), "context\"") {
					t.Fatal("validation error exposed input")
				}
			}
		})
	}
}

func TestRequestTransformUserContextChaining(t *testing.T) {
	m := New(t.TempDir(), "", "test", "provider", "model", nil)
	var secondSawFirst bool
	for _, name := range []string{"first", "second"} {
		addRequestExtension(t, m, name, extproto.RequestTransformOptions{}, func(ev extproto.RequestTransformFromHost) []byte {
			if name == "second" {
				for _, segment := range ev.Segments {
					secondSawFirst = secondSawFirst || segment.ID == "m0.c0" && segment.Text == "first"
				}
			}
			return []byte(`{"type":"request_transform_response","id":"` + ev.ID + `","prepend_user_context":"` + name + `"}`)
		})
	}
	out, err := m.TransformRequest(context.Background(), "session", "provider", "chat", provider.Request{})
	if err != nil || !secondSawFirst || len(out.Messages) != 2 {
		t.Fatalf("context chaining failed: second saw first=%t, messages=%d, error=%v", secondSawFirst, len(out.Messages), err)
	}
}
