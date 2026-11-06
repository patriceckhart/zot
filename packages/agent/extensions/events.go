package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

// EmitEvent fires a one-way lifecycle event to every extension that
// subscribed to it via SubscribeFromExt.events. Notification enqueue is
// ordered and non-blocking; a slow consumer is disconnected on overflow.
//
// Event names are documented on extproto.EventFromHost. Unknown event
// names are still routed (subscribers can use any string they want).
func (m *Manager) EmitEvent(ev extproto.EventFromHost) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.stopping {
		return
	}
	m.emitEventLocked(ev)
}

func (m *Manager) emitEventLocked(ev extproto.EventFromHost) {
	ev.Type = "event"
	m.sequence++
	ev.Sequence = m.sequence
	if ev.SessionID == "" {
		ev.SessionID = m.sessionID
	}
	if ev.SessionAlias == "" && ev.SessionID != "" && ev.SessionID == m.sessionID {
		ev.SessionAlias = m.sessionAlias
	}
	if ev.CWD == "" {
		ev.CWD = m.sessionCWD
	}
	if ev.CWD == "" {
		ev.CWD = m.cwd
	}
	// A malformed model call must still have an observable terminal result.
	if len(ev.ToolArgs) > 0 && !json.Valid(ev.ToolArgs) {
		ev.ToolArgsRaw = string(ev.ToolArgs)
		ev.ToolArgs = nil
	}
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
		if ext.stdin == nil {
			continue
		}
		var err error
		if pipe, ok := ext.stdin.(*orderedPipe); ok {
			err = pipe.enqueue(frame, nil)
		} else {
			// Tests may provide a synchronous in-memory writer.
			_, err = ext.stdin.Write(frame)
		}
		if err != nil && ext.logFile != nil {
			fmt.Fprintf(ext.logFile, "[zot] lifecycle delivery failed: %v\n", err)
		}
	}
}

// InterceptResult aggregates the outcome of walking every subscribed
// interceptor for one event. The zero value means "allow, no
// modification". Callers check Block first; if allowed, use the
// optional rewrite fields (ModifiedArgs for tool_call, ReplaceText
// for assistant_message) to carry the rewrite into the action.
type InterceptResult struct {
	SystemPrompt json.RawMessage
	failure      string
	Block        bool
	Reason       string
	ModifiedArgs json.RawMessage
	ReplaceText  string
}

const interceptTimeout = 5 * time.Second

// InterceptToolCall is the typed entry point for tool_call
// interception. Subscribers are invoked serially; the first to return
// Block=true wins. Rewrites (ModifiedArgs) from earlier subscribers
// flow into later ones, so a chain of guards can successively redact
// / patch the args.
func (m *Manager) InterceptToolCall(ctx context.Context, toolID, toolName string, args json.RawMessage) InterceptResult {
	subs := m.interceptSubsFor("tool_call")
	if len(subs) == 0 {
		return InterceptResult{}
	}
	current := args
	for _, ext := range subs {
		r := m.askIntercept(ctx, ext, extproto.EventInterceptFromHost{
			OriginExtension: m.ToolCallOrigin(toolID),
			Event:           "tool_call",
			ToolID:          toolID,
			ToolName:        toolName,
			ToolArgs:        current,
		})
		if r.Block {
			r.ModifiedArgs = current
			return r
		}
		if len(r.ModifiedArgs) > 0 && json.Valid(r.ModifiedArgs) {
			current = r.ModifiedArgs
		}
	}
	out := InterceptResult{}
	if !jsonEqual(current, args) {
		out.ModifiedArgs = current
	}
	return out
}

// InterceptTurnStart asks every subscriber whether the upcoming turn
// may run. Block=true aborts the turn with Reason shown to the user.
// Rewrites are not supported for this event.
func (m *Manager) InterceptTurnStart(ctx context.Context, step int) InterceptResult {
	subs := m.interceptSubsFor("turn_start")
	if len(subs) == 0 {
		return InterceptResult{}
	}
	for _, ext := range subs {
		r := m.askIntercept(ctx, ext, extproto.EventInterceptFromHost{
			Event: "turn_start",
			Step:  step,
		})
		if r.Block {
			return r
		}
	}
	return InterceptResult{}
}

// InterceptAssistantMessage asks every subscriber to approve, block,
// or rewrite the assistant's final visible text. Block=true hides the
// message from the user entirely; a non-empty ReplaceText rewrites
// what the user sees while keeping the model's original text in the
// transcript. Successive rewrites chain: each subscriber sees the
// previous subscriber's output.
func (m *Manager) InterceptAssistantMessage(ctx context.Context, text string) InterceptResult {
	subs := m.interceptSubsFor("assistant_message")
	if len(subs) == 0 {
		return InterceptResult{}
	}
	current := text
	for _, ext := range subs {
		r := m.askIntercept(ctx, ext, extproto.EventInterceptFromHost{
			Event: "assistant_message",
			Text:  current,
		})
		if r.Block {
			return r
		}
		if r.ReplaceText != "" {
			current = r.ReplaceText
		}
	}
	out := InterceptResult{}
	if current != text {
		out.ReplaceText = current
	}
	return out
}

// interceptSubsFor returns the snapshot of extensions that subscribed
// to intercepting the named event.
func (m *Manager) interceptSubsFor(event string) []*Extension {
	m.mu.RLock()
	defer m.mu.RUnlock()
	subs := make([]*Extension, 0, len(m.ext))
	for _, ext := range m.ext {
		ext.mu.Lock()
		_, subscribed := ext.interceptSubs[event]
		ext.mu.Unlock()
		if subscribed {
			subs = append(subs, ext)
		}
	}
	return subs
}

// askIntercept sends one EventInterceptFromHost to ext and waits for
// the reply, a timeout, or context cancellation. Returns a typed
// result. before_agent_start callers bound the complete operation with a
// context deadline; other events retain the legacy response-only timeout.
func (m *Manager) askIntercept(ctx context.Context, ext *Extension, payload extproto.EventInterceptFromHost) InterceptResult {
	m.lifecycleMu.Lock()
	if payload.SessionID == "" {
		payload.SessionID = m.sessionID
	}
	if payload.SessionAlias == "" && payload.SessionID != "" && payload.SessionID == m.sessionID {
		payload.SessionAlias = m.sessionAlias
	}
	m.lifecycleMu.Unlock()

	id := newCorrelationID()
	ch := make(chan extproto.EventInterceptResponseFromExt, 1)
	ext.mu.Lock()
	ext.pendingIntercept[id] = ch
	ext.mu.Unlock()

	payload.Type = "event_intercept"
	payload.ID = id
	frame, err := extproto.Encode(payload)
	if err != nil {
		ext.mu.Lock()
		delete(ext.pendingIntercept, id)
		ext.mu.Unlock()
		return InterceptResult{}
	}
	var writeErr error
	if payload.Event == "before_agent_start" {
		// Bound pipe writes too. Closing the pipe releases a blocked writer.
		done := make(chan error, 1)
		go func() { _, err := ext.stdin.Write(frame); done <- err }()
		select {
		case writeErr = <-done:
		case <-ctx.Done():
			_ = ext.stdin.Close()
			<-done
			writeErr = ctx.Err()
		}
	} else {
		_, writeErr = ext.stdin.Write(frame)
	}
	if err := writeErr; err != nil {
		ext.mu.Lock()
		delete(ext.pendingIntercept, id)
		ext.mu.Unlock()
		fmt.Fprintf(ext.logFile, "[zot] intercept write failed: %v\n", err)
		return InterceptResult{failure: "request write failed"}
	}

	select {
	case resp := <-ch:
		return InterceptResult{
			SystemPrompt: resp.SystemPrompt,
			Block:        resp.Block,
			Reason:       resp.Reason,
			ModifiedArgs: resp.ModifiedArgs,
			ReplaceText:  resp.ReplaceText,
		}
	case <-time.After(interceptTimeout):
		ext.mu.Lock()
		delete(ext.pendingIntercept, id)
		ext.mu.Unlock()
		fmt.Fprintf(ext.logFile, "[zot] intercept %s timed out; allowing\n", payload.Event)
		return InterceptResult{failure: "response timed out"}
	case <-ctx.Done():
		ext.mu.Lock()
		delete(ext.pendingIntercept, id)
		ext.mu.Unlock()
		return InterceptResult{failure: "response canceled or timed out"}
	}
}

// jsonEqual reports whether a and b encode the same JSON value. Used
// to detect whether the interceptor chain actually mutated the args.
func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == len(b) {
		same := true
		for i := range a {
			if a[i] != b[i] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	// Fallback to structural compare so whitespace differences don't
	// register as a mutation. (We don't actually rely on this to be
	// cheap, callers only use it to pick a code path.)
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	ae, _ := json.Marshal(av)
	be, _ := json.Marshal(bv)
	return string(ae) == string(be)
}
