package ext

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestRequestTransformSDK(t *testing.T) {
	for _, mode := range []string{"replace", "reject", "panic", "unsupported", "large"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness("transform")
			h.ext.InterceptBeforeRequest(RequestTransformOptions{}, func(ctx context.Context, ev RequestTransformEvent) (map[string]json.RawMessage, error) {
				if ev.SessionID != "session" || ev.RequestHash != "hash" || ev.Purpose != "chat" {
					t.Error("missing metadata")
				}
				if mode == "reject" {
					return nil, errors.New("secret error")
				}
				if mode == "panic" {
					panic("secret panic")
				}
				return map[string]json.RawMessage{"system": json.RawMessage(`"clean"`)}, nil
			})
			done := make(chan error, 1)
			go func() { done <- h.ext.Run() }()
			t.Cleanup(func() { h.hostW.Close(); h.ext.out.(io.Closer).Close() })
			hello := h.drainUntil(t, "hello")
			var greeting extproto.HelloFromExt
			if err := json.Unmarshal(hello.raw, &greeting); err != nil {
				t.Fatal(err)
			}
			if len(greeting.Capabilities) == 0 {
				t.Fatal("missing capability")
			}
			ack := extproto.HelloAckFromHost{Type: "hello_ack", Capabilities: []string{"request_transform"}}
			if mode == "unsupported" {
				ack.Capabilities = nil
			}
			h.sendToExt(t, ack)
			if mode == "unsupported" {
				if err := <-done; !errors.Is(err, errRequestTransformUnsupported) {
					t.Fatalf("unsupported host accepted: %v", err)
				}
				return
			}
			h.drainUntil(t, "ready")
			event := RequestTransformEvent{Type: "request_transform", ID: "id", SessionID: "session", RequestHash: "hash", Purpose: "chat"}
			if mode == "large" {
				event.Segments = []RequestSegment{{ID: "system", Kind: "text", Text: strings.Repeat("x", (4<<20)+1)}}
			}
			h.sendToExt(t, event)
			frame := h.drainUntil(t, "request_transform_response")
			var response extproto.RequestTransformResponseFromExt
			if err := json.Unmarshal(frame.raw, &response); err != nil {
				t.Fatal(err)
			}
			if response.ID != "id" {
				t.Fatal("correlation missing")
			}
			if mode == "reject" || mode == "panic" {
				if response.Error == "" || strings.Contains(response.Error, "secret") {
					t.Fatal("error absent or leaked handler content")
				}
			} else if string(response.Replacements["system"]) != `"clean"` {
				t.Fatal("replacement lost")
			}
			h.hostW.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
