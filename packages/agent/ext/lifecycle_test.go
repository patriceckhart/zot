package ext

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestLifecycleEventFields(t *testing.T) {
	h := newHarness("lifecycle")
	defer h.hostW.Close()
	names := []string{"session_start", "user_prompt_submit", "tool_result", "session_end", "pre_compact", "post_compact", "subagent_start", "subagent_stop", "permission_decision"}
	received := make(chan Event, len(names))
	for _, name := range names {
		h.ext.On(name, func(e Event) { received <- e })
	}
	go h.ext.Run()
	h.handshake(t)
	count, estimate, executed := 0, 10, false
	result := &extproto.EventResult{Content: []extproto.ContentBlock{{Type: "text", Text: "output"}}, IsError: true, Truncated: true}
	for _, name := range names {
		h.sendToExt(t, extproto.EventFromHost{Type: "event", Event: name, SessionID: "session", SessionAlias: "tidy-birch", AgentRunID: "run", CWD: "/work", Sequence: 42, Queued: true, ImageCount: 1, Status: "blocked", Reason: "reason", Source: "policy", Decision: "denied", Stage: "pre_execution", Result: result, Executed: &executed, CompactionID: "compact", MessageCount: &count, TokenEstimate: &estimate, AgentID: "child", Name: "reviewer", ToolID: "call", ToolName: "bash", ToolArgs: json.RawMessage(`{}`), Text: "prompt"})
		select {
		case e := <-received:
			want := Event{Name: name, SessionID: "session", SessionAlias: "tidy-birch", AgentRunID: "run", CWD: "/work", Sequence: 42, Queued: true, ImageCount: 1, Status: "blocked", Reason: "reason", Source: "policy", Decision: "denied", Stage: "pre_execution", Result: result, Executed: &executed, CompactionID: "compact", MessageCount: &count, TokenEstimate: &estimate, AgentID: "child", AgentName: "reviewer", ToolID: "call", ToolName: "bash", ToolArgs: json.RawMessage(`{}`), Text: "prompt"}
			if !reflect.DeepEqual(e, want) {
				t.Fatalf("event = %+v, want %+v", e, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	// Older hosts do not send session_alias.
	h.sendToExt(t, extproto.EventFromHost{Type: "event", Event: "session_start", SessionID: "legacy"})
	select {
	case e := <-received:
		if e.SessionID != "legacy" || e.SessionAlias != "" {
			t.Fatalf("legacy event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("missing legacy event")
	}
}
