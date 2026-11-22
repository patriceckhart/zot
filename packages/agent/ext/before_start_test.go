package ext

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestBeforeAgentStartSDK(t *testing.T) {
	for _, mode := range []string{"replace", "empty", "omit"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness("start-ext")
			h.ext.InterceptBeforeAgentStart(func(ev BeforeAgentStartEvent) *string {
				if ev.SystemPrompt != "base" || ev.CWD != "/work" || ev.Provider != "p" || ev.Model != "m" || ev.SessionID != "s" || ev.SessionAlias != "tidy-birch" || ev.AgentRunID != "r" {
					t.Error("wrong event metadata")
				}
				if mode == "omit" {
					return nil
				}
				value := ""
				if mode == "replace" {
					value = "  new\n"
				}
				return &value
			})
			done := make(chan struct{})
			go func() { defer close(done); h.ext.Run() }()
			t.Cleanup(func() { h.hostW.Close(); <-done; h.ext.out.(io.Closer).Close() })
			h.drainUntil(t, "hello")
			h.sendToExt(t, extproto.HelloAckFromHost{Type: "hello_ack", ProtocolVersion: extproto.ProtocolVersion})
			sub := h.drainUntil(t, "subscribe")
			var subscription extproto.SubscribeFromExt
			if err := json.Unmarshal(sub.raw, &subscription); err != nil {
				t.Fatal(err)
			}
			if len(subscription.Intercept) != 1 || subscription.Intercept[0] != "before_agent_start" {
				t.Fatalf("subscription = %+v", subscription)
			}
			h.drainUntil(t, "ready")
			base := "base"
			h.sendToExt(t, extproto.EventInterceptFromHost{Type: "event_intercept", Event: "before_agent_start", ID: "1", SystemPrompt: &base, CWD: "/work", Provider: "p", Model: "m", SessionID: "s", SessionAlias: "tidy-birch", AgentRunID: "r"})
			frame := h.drainUntil(t, "event_intercept_response")
			var result extproto.EventInterceptResponseFromExt
			if err := json.Unmarshal(frame.raw, &result); err != nil {
				t.Fatal(err)
			}
			if result.ID != "1" {
				t.Fatal("missing correlation ID")
			}
			want := `"  new\n"`
			if mode == "empty" {
				want = `""`
			}
			if mode == "omit" {
				want = ""
			}
			if string(result.SystemPrompt) != want {
				t.Fatalf("got %s want %s", result.SystemPrompt, want)
			}
		})
	}
}
