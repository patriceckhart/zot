package extensions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
)

// Extension tools default to never. Declared safe, idempotent, and reconcile
// policies map onto the core contract; anything else stays never. A durable
// host's operation key travels in the call frame.
func TestExtensionToolReplayPolicyAndOperationKey(t *testing.T) {
	m, ext, frames, replies := toolPeer(t)
	cases := map[string]core.ToolReplayPolicy{"": core.ReplayNever, "never": core.ReplayNever, "safe": core.ReplaySafe, "idempotent": core.ReplayIdempotent, "reconcile": core.ReplayReconcile, "bogus": core.ReplayNever}
	for declared, want := range cases {
		tool := NewTool(m, ToolInfo{Name: "ask", Extension: ext.Manifest.Name, Replay: declared})
		if got := core.ReplayPolicyOf(tool); got != want {
			t.Fatalf("replay %q: got %s want %s", declared, got, want)
		}
	}
	done := make(chan core.ToolResult, 1)
	go func() {
		ctx := core.WithToolOperationKey(context.Background(), "zot-op-test")
		result, _ := NewTool(m, ToolInfo{Name: "ask", Extension: ext.Manifest.Name, Replay: "idempotent"}).Execute(ctx, json.RawMessage(`{}`), nil)
		done <- result
	}()
	call := toolCallFrame(t, frames)
	if call.OperationKey != "zot-op-test" {
		t.Fatalf("operation key not delivered: %+v", call)
	}
	sendToolFrame(t, replies, extproto.ToolResultFromExt{Type: "tool_result", ID: call.ID, Content: []extproto.ContentBlock{{Type: "text", Text: "ok"}}})
	<-done
	// Without a durable host the frame carries no key.
	go func() {
		result, _ := NewTool(m, ToolInfo{Name: "ask", Extension: ext.Manifest.Name}).Execute(context.Background(), json.RawMessage(`{}`), nil)
		done <- result
	}()
	call = toolCallFrame(t, frames)
	if call.OperationKey != "" {
		t.Fatalf("unexpected operation key: %+v", call)
	}
	sendToolFrame(t, replies, extproto.ToolResultFromExt{Type: "tool_result", ID: call.ID, Content: []extproto.ContentBlock{{Type: "text", Text: "ok"}}})
	<-done
}

// An extension declaring replay "reconcile" is asked with a tool_call carrying
// reconcile true; its structured answer maps onto the core outcome and a
// malformed answer is unknown.
func TestExtensionToolReconcileOverTheWire(t *testing.T) {
	m, ext, frames, replies := toolPeer(t)
	tool := NewTool(m, ToolInfo{Name: "ask", Extension: ext.Manifest.Name, Replay: "reconcile"})
	if core.ReplayPolicyOf(tool) != core.ReplayReconcile {
		t.Fatal("reconcile policy not mapped")
	}
	rec := tool.(core.ToolReconciler)
	type answer struct {
		outcome core.ReconcileOutcome
		result  core.ToolResult
		err     error
	}
	run := func(reply extproto.ToolResultFromExt) answer {
		done := make(chan answer, 1)
		go func() {
			o, r, err := rec.Reconcile(context.Background(), "zot-op-1", json.RawMessage(`{}`))
			done <- answer{o, r, err}
		}()
		call := toolCallFrame(t, frames)
		if !call.Reconcile || call.OperationKey != "zot-op-1" {
			t.Fatalf("reconcile frame: %+v", call)
		}
		reply.ID = call.ID
		reply.Type = "tool_result"
		sendToolFrame(t, replies, reply)
		return <-done
	}
	if a := run(extproto.ToolResultFromExt{StructuredContent: json.RawMessage(`{"state":"completed"}`), Content: []extproto.ContentBlock{{Type: "text", Text: "sent"}}}); a.err != nil || a.outcome != core.ReconcileCompleted || len(a.result.Content) != 1 {
		t.Fatalf("completed: %+v", a)
	}
	if a := run(extproto.ToolResultFromExt{StructuredContent: json.RawMessage(`{"state":"not_started"}`)}); a.outcome != core.ReconcileNotStarted {
		t.Fatalf("not started: %+v", a)
	}
	if a := run(extproto.ToolResultFromExt{Content: []extproto.ContentBlock{{Type: "text", Text: "no answer"}}}); a.outcome != core.ReconcileUnknown {
		t.Fatalf("malformed must be unknown: %+v", a)
	}
}
