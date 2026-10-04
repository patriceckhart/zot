package agent

import (
	"context"
	"encoding/base64"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func startExtensionSession(mgr *extensions.Manager, ag *core.Agent, cwd, reason string) {
	id := ""
	if ag != nil {
		id = ag.SessionID
	}
	mgr.StartSession(id, cwd, reason)
}

func wireLifecycleEvents(ag *core.Agent, mgr *extensions.Manager) {
	ag.OnEvent = func(ev core.AgentEvent) {
		_, cwd := mgr.SessionContext()
		startExtensionSession(mgr, ag, cwd, "session_switch")
		fanoutAgentEvent(mgr, ev)
	}
}

// Bound observation payloads independently from tool output and transcripts.
// The budget includes base64 bytes; images are omitted rather than corrupted.
const eventResultBudget = 256 * 1024

func extensionEventResult(result core.ToolResult) *extproto.EventResult {
	out := &extproto.EventResult{IsError: result.IsError, Content: []extproto.ContentBlock{}}
	remaining := eventResultBudget
	for _, block := range result.Content {
		if len(out.Content) >= 128 {
			out.Truncated = true
			break
		}
		switch b := block.(type) {
		case provider.TextBlock:
			text := b.Text
			if len(text) > remaining {
				end := remaining
				for end > 0 && !utf8.RuneStart(text[end]) {
					end--
				}
				text = text[:end]
				out.Truncated = true
			}
			remaining -= len(text)
			out.Content = append(out.Content, extproto.ContentBlock{Type: "text", Text: text, Format: b.Format})
		case provider.ImageBlock:
			size := base64.StdEncoding.EncodedLen(len(b.Data)) + len(b.MimeType)
			if size > remaining {
				out.Truncated = true
				continue
			}
			remaining -= size
			out.Content = append(out.Content, extproto.ContentBlock{Type: "image", MimeType: b.MimeType, Data: base64.StdEncoding.EncodeToString(b.Data)})
		default:
			out.Truncated = true
		}
	}
	return out
}

func emitPermissionDecision(ctx context.Context, mgr *extensions.Manager, call provider.ToolCallBlock, d core.ConfirmDecision) {
	decision := "denied"
	if d.Allow {
		decision = "approved"
	}
	if ctx.Err() != nil {
		decision = "cancelled"
	}
	origin, _ := ctx.Value(extensionOriginKey{}).(string)
	mgr.EmitEvent(extproto.EventFromHost{Event: "permission_decision", OriginExtension: origin, ToolID: call.ID, ToolName: call.Name, Decision: decision, Source: d.Source, Reason: d.Reason, Stage: "pre_execution"})
}

func emitSwarmLifecycle(mgr *extensions.Manager, e swarm.LifecycleEvent) {
	mgr.EmitEvent(extproto.EventFromHost{Event: e.Event, SessionID: e.SessionID, AgentID: e.AgentID, AgentRunID: e.AgentRunID, Name: e.Name, CWD: e.CWD, Status: e.Status, Error: e.Error})
}
