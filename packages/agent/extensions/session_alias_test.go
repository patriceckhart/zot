package extensions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestLifecycleSessionAliases(t *testing.T) {
	m := New(t.TempDir(), "/work", "test", "", "", nil)
	frames, _ := lifecyclePeer(t, m, "session_start", "session_end", "tool_result", "subagent_stop")
	t.Cleanup(func() { m.Stop(time.Second) })
	read := func(event, id, alias string) {
		t.Helper()
		raw := nextLifecycleFrame(t, frames)
		var got extproto.EventFromHost
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.Event != event || got.SessionID != id || got.SessionAlias != alias {
			t.Fatalf("event = %+v, want %s, %s, %q", got, event, id, alias)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["session_alias"]; present != (alias != "") {
			t.Fatalf("optional alias encoded incorrectly: %s", raw)
		}
	}
	m.StartSessionWithAlias("one", "tidy-birch", "/work", "")
	read("session_start", "one", "tidy-birch")
	m.StartSessionWithAlias("one", "tidy-birch", "/work", "")
	m.EmitEvent(extproto.EventFromHost{Event: "tool_result"})
	read("tool_result", "one", "tidy-birch")
	m.StartSessionWithAlias("two", "chatty-lake", "/work", "session_switch")
	read("session_end", "one", "tidy-birch")
	read("session_start", "two", "chatty-lake")
	// A delayed event for another conversation must not inherit the active alias.
	m.EmitEvent(extproto.EventFromHost{Event: "subagent_stop", SessionID: "one"})
	read("subagent_stop", "one", "")
	m.EmitEvent(extproto.EventFromHost{Event: "subagent_stop", SessionID: "one", SessionAlias: "tidy-birch"})
	read("subagent_stop", "one", "tidy-birch")
	m.StartSession("legacy", "/work", "session_switch")
	read("session_end", "two", "chatty-lake")
	read("session_start", "legacy", "")
	m.EndSession("shutdown")
	read("session_end", "legacy", "")
}

func TestInterceptSessionAliases(t *testing.T) {
	m := New(t.TempDir(), "/work", "test", "p", "m", nil)
	m.StartSessionWithAlias("one", "tidy-birch", "/work", "")
	wantID, wantAlias := "one", "tidy-birch"
	ext := addStartExtension(t, m, "alias", func(ev extproto.EventInterceptFromHost) string {
		if ev.SessionID != wantID || ev.SessionAlias != wantAlias {
			t.Errorf("intercept context = %+v, want %s, %q", ev, wantID, wantAlias)
		}
		return `{"type":"event_intercept_response","id":"` + ev.ID + `"}`
	})
	base := "base"
	m.InterceptBeforeAgentStart(context.Background(), extproto.EventInterceptFromHost{SystemPrompt: &base, SessionID: "one"})
	m.askIntercept(context.Background(), ext, extproto.EventInterceptFromHost{Event: "tool_call"})
	wantID, wantAlias = "other", ""
	m.askIntercept(context.Background(), ext, extproto.EventInterceptFromHost{Event: "tool_call", SessionID: "other"})
	m.StartSession("legacy", "/work", "session_switch")
	wantID = "legacy"
	m.InterceptBeforeAgentStart(context.Background(), extproto.EventInterceptFromHost{SystemPrompt: &base})
}
