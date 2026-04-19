package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/internal/extproto"
)

// EmitEvent fires a one-way lifecycle event to every extension that
// subscribed to it via SubscribeFromExt.events. Non-blocking: each
// extension's pipe write happens on a per-call goroutine so a slow
// extension can't stall the agent loop.
//
// Event names are documented on extproto.EventFromHost. Unknown event
// names are still routed (subscribers can use any string they want).
func (m *Manager) EmitEvent(ev extproto.EventFromHost) {
	ev.Type = "event"
	frame, err := extproto.Encode(ev)
	if err != nil {
		return
	}
	m.mu.RLock()
	subs := make([]*Extension, 0, len(m.ext))
	for _, ext := range m.ext {
		ext.mu.Lock()
		_, subscribed := ext.eventSubs[ev.Event]
		ext.mu.Unlock()
		if subscribed {
			subs = append(subs, ext)
		}
	}
	m.mu.RUnlock()

	for _, ext := range subs {
		go func(ext *Extension) {
			defer func() {
				// A panicking write to a closed pipe shouldn't kill
				// the calling goroutine.
				_ = recover()
			}()
			_, _ = ext.stdin.Write(frame)
		}(ext)
	}
}

// InterceptToolCall asks every extension that subscribed to tool_call
// interception whether the call may proceed. Subscribers are invoked
// serially; the first one to return Block: true wins, with its Reason
// surfaced as the refusal text. allowed=true means proceed.
//
// Each subscriber gets up to interceptTimeout to reply; missing the
// deadline counts as "allow" (i.e. an unresponsive interceptor never
// stalls the agent).
func (m *Manager) InterceptToolCall(ctx context.Context, toolID, toolName string, args json.RawMessage) (allowed bool, reason string) {
	m.mu.RLock()
	subs := make([]*Extension, 0, len(m.ext))
	for _, ext := range m.ext {
		ext.mu.Lock()
		_, subscribed := ext.interceptSubs["tool_call"]
		ext.mu.Unlock()
		if subscribed {
			subs = append(subs, ext)
		}
	}
	m.mu.RUnlock()
	if len(subs) == 0 {
		return true, ""
	}

	for _, ext := range subs {
		ok, reason := m.askIntercept(ctx, ext, toolID, toolName, args)
		if !ok {
			return false, reason
		}
	}
	return true, ""
}

const interceptTimeout = 5 * time.Second

func (m *Manager) askIntercept(ctx context.Context, ext *Extension, toolID, toolName string, args json.RawMessage) (allowed bool, reason string) {
	id := newCorrelationID()
	ch := make(chan extproto.EventInterceptResponseFromExt, 1)
	ext.mu.Lock()
	ext.pendingIntercept[id] = ch
	ext.mu.Unlock()

	frame, _ := extproto.Encode(extproto.EventInterceptFromHost{
		Type:     "event_intercept",
		ID:       id,
		Event:    "tool_call",
		ToolID:   toolID,
		ToolName: toolName,
		ToolArgs: args,
	})
	if _, err := ext.stdin.Write(frame); err != nil {
		ext.mu.Lock()
		delete(ext.pendingIntercept, id)
		ext.mu.Unlock()
		fmt.Fprintf(ext.logFile, "[zot] intercept write failed: %v\n", err)
		return true, ""
	}

	select {
	case resp := <-ch:
		return !resp.Block, resp.Reason
	case <-time.After(interceptTimeout):
		ext.mu.Lock()
		delete(ext.pendingIntercept, id)
		ext.mu.Unlock()
		fmt.Fprintf(ext.logFile, "[zot] intercept %s timed out; allowing\n", toolName)
		return true, ""
	case <-ctx.Done():
		ext.mu.Lock()
		delete(ext.pendingIntercept, id)
		ext.mu.Unlock()
		return true, ""
	}
}
