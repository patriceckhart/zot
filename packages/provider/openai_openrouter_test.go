package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenRouterBuildRequestIncludesSessionID(t *testing.T) {
	c := NewOpenRouter("token", "").(*openaiClient)
	wire, err := c.buildRequest(Request{
		Model:     "@preset/flash",
		SessionID: "sess-abc",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.SessionID != "sess-abc" {
		t.Fatalf("SessionID = %q; want sess-abc", wire.SessionID)
	}
}

func TestOpenAIBuildRequestOmitsSessionID(t *testing.T) {
	c := NewOpenAI("token", "").(*openaiClient)
	wire, err := c.buildRequest(Request{
		Model:        "gpt-5",
		SessionID:    "sess-abc",
		MaxToolCalls: 4,
		Messages:     []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.SessionID != "" {
		t.Fatalf("SessionID = %q; want empty on non-openrouter clients", wire.SessionID)
	}
	if wire.MaxToolCalls != 0 {
		t.Fatalf("MaxToolCalls = %d; want empty on non-openrouter clients", wire.MaxToolCalls)
	}
}

func TestOpenRouterOmitsServerToolCallsFromHistory(t *testing.T) {
	c := NewOpenRouter("token", "").(*openaiClient)
	wire, err := c.buildRequest(Request{
		Model: "@preset/flash",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "time?"}}},
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "srv-1", Name: "openrouter:datetime", Arguments: json.RawMessage(`{}`), Server: true},
				TextBlock{Text: "it is noon"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 2 {
		t.Fatalf("messages = %d; want 2", len(wire.Messages))
	}
	am := wire.Messages[1]
	if len(am.ToolCalls) != 0 {
		t.Fatalf("replayed server tool_calls = %+v; want none", am.ToolCalls)
	}
	if am.Content != "it is noon" {
		t.Fatalf("content = %#v", am.Content)
	}
}

func TestOpenRouterServerToolStreamDoesNotStopForClientExecution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"session_id":"sess-1"`) {
			t.Errorf("request body missing session_id: %s", body)
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
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_dt\",\"type\":\"openrouter:datetime\",\"name\":\"openrouter:datetime\",\"arguments\":\"{}\"}]},\"finish_reason\":null}]}\n\n")
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2026-08-30\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenRouter("token", srv.URL)
	evs, err := c.Stream(context.Background(), Request{
		Model:     "@preset/flash",
		SessionID: "sess-1",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "time?"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var done EventDone
	for ev := range evs {
		if e, ok := ev.(EventDone); ok {
			done = e
		}
	}
	if done.Stop != StopEnd {
		t.Fatalf("stop = %v; want end (server tools must not trigger client execution)", done.Stop)
	}
	var sawServer bool
	for _, c := range done.Message.Content {
		if tc, ok := c.(ToolCallBlock); ok {
			if !tc.Server || tc.Name != "openrouter:datetime" {
				t.Fatalf("tool call = %+v; want server openrouter:datetime", tc)
			}
			sawServer = true
		}
	}
	if !sawServer {
		t.Fatalf("assembled message missing server tool call: %+v", done.Message.Content)
	}
}

func TestOpenRouterAdvertisesServerTools(t *testing.T) {
	c := NewOpenRouter("token", "").(*openaiClient)
	wire, err := c.buildRequest(Request{
		Model: "@preset/flash",
		Tools: []Tool{
			{Name: "openrouter:datetime"},
			{Name: "openrouter:web_search", Schema: json.RawMessage(`{"max_results":1}`)},
			{Name: "read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 3 {
		t.Fatalf("tools = %d; want 3", len(wire.Tools))
	}
	if wire.Tools[0].Type != "openrouter:datetime" || wire.Tools[0].Function != nil {
		t.Fatalf("datetime tool = %+v", wire.Tools[0])
	}
	if wire.Tools[1].Type != "openrouter:web_search" || string(wire.Tools[1].Parameters) != `{"max_results":1}` {
		t.Fatalf("web_search tool = %+v", wire.Tools[1])
	}
	if wire.Tools[2].Type != "function" || wire.Tools[2].Function == nil || wire.Tools[2].Function.Name != "read" {
		t.Fatalf("function tool = %+v", wire.Tools[2])
	}
}
