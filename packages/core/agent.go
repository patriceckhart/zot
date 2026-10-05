package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// Agent is a stateful conversation bound to a provider client, a model,
// and a set of tools.
type Agent struct {
	Client      provider.Client
	Model       string
	System      string
	Tools       Registry
	MaxSteps    int
	Reasoning   string
	Temperature *float32

	// MaxTokens caps the model's output tokens per turn. Zero leaves
	// the field unset on the provider request, letting each provider
	// apply its own default (which can be conservative, e.g. Bedrock
	// defaults to 4096, truncating long writes/edits). Hosts populate
	// this from the resolved model's MaxOutput so large single-turn
	// responses aren't silently cut off with stopReason=length.
	MaxTokens int

	// SessionID is the zot conversation id, forwarded to providers that
	// support sticky routing. Empty means omitted.
	SessionID string

	// MaxToolCalls caps provider-executed server-tool calls per turn.
	// Zero leaves the field unset.
	MaxToolCalls int

	// BeforeToolExecute, if set, is called immediately before each
	// tool runs. Returning (allowed=false, reason) short-circuits
	// the call with an error result containing reason. Optionally,
	// returning a non-nil modifiedArgs replaces the JSON args the
	// tool will see, which lets guards redact / augment / patch the
	// model's request without rewriting the transcript. Empty or
	// malformed modifiedArgs is ignored.
	BeforeToolExecute func(call provider.ToolCallBlock) (allowed bool, reason string, modifiedArgs json.RawMessage)

	// BeforeToolExecuteContext is the context-aware variant. When set it takes
	// precedence over BeforeToolExecute, retaining the legacy hook for embedders.
	BeforeToolExecuteContext func(context.Context, provider.ToolCallBlock) (bool, string, json.RawMessage)

	// BeforeStart replaces the complete system prompt once per runtime session
	// or explicit prompt/model reset, before any turn starts. It is not called
	// for ordinary follow-up messages or tool-loop steps.
	BeforeStart func(context.Context, string) string

	// BeforeTurn, if set, is called before each turn's model call.
	// Returning (allowed=false, reason) aborts the turn; reason is
	// surfaced as an assistant-like status line. Used for rate-
	// limiting, business-hour gates, and deny-by-default setups.
	BeforeTurn func(step int) (allowed bool, reason string)

	// BeforeNextTurn runs synchronously before a continuation model call,
	// after the previous response and all tool results have been appended.
	// It can compact the transcript safely before queued messages are drained.
	// It does not run for the first call or when the loop is stopping.
	// Returning an error stops the run without sending another request.
	BeforeNextTurn func(context.Context) error

	// BeforeAssistantMessage, if set, is called after the model's
	// final assistant message is assembled but before it's appended
	// to the transcript. Returning (allowed=false) suppresses both
	// the transcript append and the UI event. A non-empty
	// replacement rewrites the visible text for the user while
	// leaving the model's original text in the transcript (so the
	// model can still see what it said in subsequent turns).
	BeforeAssistantMessage func(text string) (allowed bool, reason, replacement string)

	// MaxRetries controls agent-level retries for transient provider
	// failures that arrive after the HTTP stream opens (for example
	// Anthropic overloaded_error). Zero disables this retry layer.
	// RetryBaseDelay is doubled for each attempt; zero uses 2s.
	MaxRetries     int
	RetryBaseDelay time.Duration

	// OnEvent, if set, mirrors every AgentEvent the loop emits to
	// this callback in addition to the per-Prompt sink. Used by the
	// extension manager to fan events out to subscribed extensions
	// without each caller having to compose sinks manually. Prompt submission
	// and compaction lifecycle notifications go only to OnEvent, preserving the
	// existing per-Prompt stream consumed by RPC and embedded SDK clients.
	OnEvent func(AgentEvent)

	// OnMessageAppended, if set, fires every time a message is
	// appended to the in-memory transcript by the agent loop — the
	// initial user prompt, each finalised assistant message, and
	// each tool-results message (plus the synthetic OpenAI image
	// mirror, if any). Hosts wire this to the on-disk session so
	// that turns are durable as soon as they happen, instead of
	// only being flushed on a clean exit.
	OnMessageAppended func(provider.Message)

	// OnUsage, if set, fires after every turn's usage row arrives,
	// carrying the cumulative usage for the session. Hosts wire
	// this to the on-disk session so the persisted total stays
	// current and a crash recovers the right cost figure.
	OnUsage func(cumulative provider.Usage)

	// OnTranscriptCompacted, if set, fires after Compact replaces the
	// in-memory transcript with the synthetic summary plus kept tail.
	// Hosts wire this to append an explicit compaction checkpoint to
	// the session log; per-message append hooks do not fire for this
	// wholesale transcript replacement.
	OnTranscriptCompacted func(messages []provider.Message)

	// Preparation caches the effective prompt separately from its unmodified
	// base so a model or session reset cannot stack extension appendices.
	startPrepared                                    bool
	startGeneration                                  uint64
	startBase, startSystem, startModel, startSession string

	mu        sync.Mutex
	messages  []provider.Message
	toolState map[string]json.RawMessage
	// rev increments whenever the transcript slice is replaced or a
	// message is appended. The TUI uses it as a cheap redraw cache key
	// so editor-only typing doesn't copy/rebuild a long transcript on
	// every keypress.
	rev  uint64
	cost CostTracker

	// queued holds user messages submitted while the agent is busy.
	// The loop appends them as normal user messages at safe
	// boundaries: before the next model call after a tool batch, or
	// after a text-only assistant turn finishes. It never interrupts
	// a running tool or cancels an in-flight provider request.
	queued []string
}

// NewAgent returns an Agent with sensible defaults.
func NewAgent(client provider.Client, model, system string, tools Registry) *Agent {
	return &Agent{
		Client:         client,
		Model:          model,
		System:         system,
		Tools:          tools,
		MaxSteps:       0, // 0 = unlimited
		MaxRetries:     3,
		RetryBaseDelay: 2 * time.Second,
	}
}

// QueueMessage queues text to be injected as a user message at the
// next safe boundary of the active agent loop. It is non-blocking in
// the sense that it never waits for model/tool work; it only takes
// the transcript mutex briefly. Empty/whitespace-only messages are
// ignored.
func (a *Agent) QueueMessage(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if a.OnEvent != nil {
		a.OnEvent(EvPromptSubmit{Text: text, Queued: true})
	}
	a.mu.Lock()
	a.queued = append(a.queued, text)
	a.mu.Unlock()
	return true
}

// PendingQueuedMessages returns a snapshot of user messages waiting
// to be injected. Used by hosts to render the visible "sliding in"
// chips without consuming them.
func (a *Agent) PendingQueuedMessages() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.queued))
	copy(out, a.queued)
	return out
}

// QueuedMessageCount returns the number of messages waiting to be
// injected at the next safe boundary.
func (a *Agent) QueuedMessageCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queued)
}

// PopQueuedMessage removes and returns the most recently queued
// message. Hosts use this for the slide-back keybinding.
func (a *Agent) PopQueuedMessage() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.queued)
	if n == 0 {
		return "", false
	}
	text := a.queued[n-1]
	a.queued = a.queued[:n-1]
	return text, true
}

// DrainQueuedMessages discards and returns every queued message.
// Hosts use this on explicit cancel/clear so stale follow-ups do
// not run after the user aborted the turn.
func (a *Agent) DrainQueuedMessages() []string {
	return a.drainQueuedMessages()
}

func (a *Agent) drainQueuedMessages() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.queued))
	copy(out, a.queued)
	a.queued = nil
	return out
}

func (a *Agent) appendQueuedAsUser(texts []string, sink func(AgentEvent)) {
	for _, text := range texts {
		msg := provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: text}},
			Time:    time.Now(),
		}
		a.mu.Lock()
		a.messages = append(a.messages, msg)
		a.rev++
		a.mu.Unlock()
		a.fireMessageAppended(msg)
		if sink != nil {
			sink(EvUserMessage{Message: msg})
		}
	}
}

// Messages returns a copy of the current transcript.
func (a *Agent) Messages() []provider.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]provider.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// ContextSnapshot returns the provider-neutral inputs that make up the
// current model context. Hosts use the snapshot for read-only inspection
// without racing a tool-registry replacement or transcript append.
func (a *Agent) ContextSnapshot() (system string, tools []provider.Tool, messages []provider.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	messages = withoutToolDisplay(a.messages)
	return a.System, a.Tools.Specs(), messages
}

// Revision returns a monotonically increasing transcript version.
// It is cheap to query and changes whenever Messages() would return
// different transcript content because of append/set operations.
func (a *Agent) Revision() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rev
}

// SetTools swaps the tool registry. Used by /reload-ext to hand
// the agent a fresh registry after extension subprocesses have been
// respawned (and their freshly-registered tools merged in).
func (a *Agent) SetTools(reg Registry) {
	a.mu.Lock()
	a.Tools = reg
	a.mu.Unlock()
}

// SetMessages replaces the transcript (used when resuming a session).
func (a *Agent) SetMessages(msgs []provider.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages[:0], msgs...)
	a.toolState = readToolState(msgs)
	if len(msgs) == 0 {
		a.resetStartLocked()
	}
	a.rev++
}

// AppendUserContext adds a user-role message to the transcript without
// starting a model turn. Hosts use it for context gathered outside the agent
// loop, such as the output of an explicitly invoked shell command.
func (a *Agent) AppendUserContext(text string, meta map[string]string) {
	msg := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Time:    time.Now(),
		Meta:    meta,
	}
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.rev++
	a.mu.Unlock()
	a.fireMessageAppended(msg)
}

// Cost returns the cumulative usage.
func (a *Agent) Cost() provider.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cost.Total
}

// SeedCost sets the cumulative usage as a baseline before the first
// turn runs. Used when transferring state from another agent (model
// or provider switch) so the running cost meter doesn't reset to 0.
func (a *Agent) SeedCost(u provider.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cost.Seed(u)
}

// LastTurnUsage returns the per-turn usage of the most recent
// completed turn. Drives the "context used" gauge in the status bar
// without waiting for the next turn to land.
func (a *Agent) LastTurnUsage() provider.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cost.LastTurn
}

// SeedLastTurnUsage primes the per-turn snapshot. Used on resume so
// the gauge reflects the prompt size of the last turn in the session
// file instead of starting at zero.
func (a *Agent) SeedLastTurnUsage(u provider.Usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cost.LastTurn = u
}

// fireMessageAppended invokes OnMessageAppended without holding the
// agent mutex, so the host's persistence callback can take its own
// locks without deadlocking the agent loop. Tolerates a nil hook so
// non-persisting callers (tests, RPC mode) don't have to set it.
func (a *Agent) fireMessageAppended(m provider.Message) {
	if a.OnMessageAppended != nil {
		a.OnMessageAppended(m)
	}
}

// Prompt sends a user message and runs the agent loop until the model
// stops or an error occurs. Events are delivered via sink in order.
// sink must not block the caller for long; buffer as needed.
func (a *Agent) Prompt(ctx context.Context, text string, images []provider.ImageBlock, sink func(AgentEvent)) error {
	return a.promptWithPrelude(ctx, text, images, nil, "", sink)
}

// PromptWithTool runs a host-requested tool after the user prompt and before
// the first model turn. The call and result are persisted as a matched pair.
// A cancelled execution still records its result, but does not call the model.
func (a *Agent) PromptWithTool(ctx context.Context, text string, call provider.ToolCallBlock, origin string, sink func(AgentEvent)) error {
	if strings.TrimSpace(text) == "" || call.ID == "" || call.Name == "" || !json.Valid(call.Arguments) || !strings.HasPrefix(strings.TrimSpace(string(call.Arguments)), "{") {
		return fmt.Errorf("tool prompt requires text, tool ID, name and JSON object arguments")
	}
	return a.promptWithPrelude(ctx, text, nil, &call, origin, sink)
}

func (a *Agent) promptWithPrelude(ctx context.Context, text string, images []provider.ImageBlock, call *provider.ToolCallBlock, origin string, sink func(AgentEvent)) error {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink = a.wrapSink(sink)
	if a.OnEvent != nil {
		a.OnEvent(EvPromptSubmit{Text: text, ImageCount: len(images)})
	}
	content := []provider.Content{}
	if text != "" {
		content = append(content, provider.TextBlock{Text: text})
	}
	for _, img := range images {
		content = append(content, img)
	}
	user := provider.Message{Role: provider.RoleUser, Content: content, Time: time.Now()}

	a.mu.Lock()
	a.messages = append(a.messages, user)
	a.rev++
	a.mu.Unlock()
	a.fireMessageAppended(user)
	sink(EvUserMessage{Message: user})

	if call != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		sink(EvToolCall{ID: call.ID, Name: call.Name, Args: call.Arguments})
		result := a.runOneTool(ctx, *call, sink)
		assistant := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{*call}, Time: time.Now(), Meta: map[string]string{"origin_extension": origin, "synthetic_tool_call": "true"}}
		tool := provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: call.ID, Content: result.Content, IsError: result.IsError}}, Time: time.Now(), Meta: toolResultMetadata(result.State, map[string][]provider.NestedToolCall{call.ID: result.NestedCalls})}
		for _, name := range result.ActivateTools {
			if _, err := a.Tools.Get(name); err == nil && !containsString(tool.AddedToolNames, name) {
				tool.AddedToolNames = append(tool.AddedToolNames, name)
			}
		}
		var mirror provider.Message
		if a.Client != nil && (a.Client.Name() == "openai" || a.Client.Name() == "openai-codex") {
			mirror = mirrorToolImagesAsUser(tool)
		}
		a.mu.Lock()
		a.messages = append(a.messages, assistant, tool)
		a.rev += 2
		if len(mirror.Content) > 0 {
			a.messages = append(a.messages, mirror)
			a.rev++
		}
		a.mu.Unlock()
		a.fireMessageAppended(assistant)
		a.fireMessageAppended(tool)
		if len(mirror.Content) > 0 {
			a.fireMessageAppended(mirror)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return a.runLoop(ctx, sink)
}

// Continue runs the agent loop against the existing transcript. Used
// after appending tool results manually or to retry.
func (a *Agent) Continue(ctx context.Context, sink func(AgentEvent)) error {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink = a.wrapSink(sink)
	return a.runLoop(ctx, sink)
}

// wrapSink composes the per-call sink with a.OnEvent (if set) so the
// extension manager (or any other observer) sees every AgentEvent
// without having to thread itself through every Prompt callsite.
func (a *Agent) wrapSink(sink func(AgentEvent)) func(AgentEvent) {
	if a.OnEvent == nil {
		return sink
	}
	obs := a.OnEvent
	return func(ev AgentEvent) {
		obs(ev)
		sink(ev)
	}
}

func (a *Agent) runLoop(ctx context.Context, sink func(AgentEvent)) error {
	for step := 1; a.MaxSteps <= 0 || step <= a.MaxSteps; step++ {
		// Preparation is cached across steps and user messages. Only an
		// explicit prompt/model/session change invokes BeforeStart again.
		if err := a.prepareStart(ctx); err != nil {
			sink(EvDone{})
			return err
		}
		if step > 1 && a.BeforeNextTurn != nil {
			if err := a.BeforeNextTurn(ctx); err != nil {
				sink(EvDone{})
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			sink(EvDone{})
			return err
		}
		// Messages queued while the agent was busy are delivered
		// before the next model call. This is the safe boundary:
		// any previous tool batch has already completed and its
		// results have been appended, but no new provider request has
		// started yet.
		if pending := a.drainQueuedMessages(); len(pending) > 0 {
			a.appendQueuedAsUser(pending, sink)
		}

		sink(EvTurnStart{Step: step})
		if a.BeforeTurn != nil {
			if allowed, reason := a.BeforeTurn(step); !allowed {
				if reason == "" {
					reason = "turn blocked by extension guard"
				}
				sink(EvTurnEnd{Stop: provider.StopError, Err: fmt.Errorf("%s", reason)})
				sink(EvDone{})
				return nil
			}
		}

		var (
			stop         provider.StopReason
			assistantMsg provider.Message
			err          error
		)
		for attempt := 0; ; attempt++ {
			stop, assistantMsg, err = a.oneTurn(ctx, sink)
			sink(EvTurnEnd{Stop: stop, Err: err})
			if err == nil || !a.canRetryError(err, attempt) {
				break
			}
			a.dropLastAssistantMessage()
			if sleepErr := sleepRetry(ctx, a.retryDelay(attempt)); sleepErr != nil {
				return sleepErr
			}
		}
		if err != nil {
			return err
		}

		if stop == provider.StopToolUse {
			// Execute each client tool call, append a single tool-results message, continue.
			toolMsg, hadError := a.executeTools(ctx, assistantMsg, sink)
			if len(toolMsg.Content) == 0 {
				// Provider-executed (server) tools need no client results.
				continue
			}
			a.mu.Lock()
			a.messages = append(a.messages, toolMsg)
			a.rev++
			// OpenAI's chat-completions tool message shape is text-centric.
			// Vision models reliably consume images when they arrive as user
			// content, so when a tool result contains images we mirror them
			// into a synthetic user message immediately after the tool result.
			// This keeps the transcript self-contained for providers that can
			// see image blocks in tool messages while making OpenAI vision
			// models actually receive the image bytes.
			//
			// The OpenAI Responses route ("openai-codex") has the same
			// text-centric tool-output shape: a function_call_output only
			// carries a string, so images in a tool result never reach the
			// model. Both providers serialize images correctly when they
			// arrive as user content, so the mirror covers them both.
			var imageMirror provider.Message
			if a.Client != nil && (a.Client.Name() == "openai" || a.Client.Name() == "openai-codex") {
				if mirror := mirrorToolImagesAsUser(toolMsg); len(mirror.Content) > 0 {
					a.messages = append(a.messages, mirror)
					a.rev++
					imageMirror = mirror
				}
			}
			a.mu.Unlock()
			a.fireMessageAppended(toolMsg)
			if len(imageMirror.Content) > 0 {
				a.fireMessageAppended(imageMirror)
			}
			// If context was cancelled during tool execution, bail out.
			if err := ctx.Err(); err != nil {
				sink(EvDone{})
				return err
			}
			_ = hadError
			continue
		}

		// If the assistant stopped without tool calls but a message was
		// queued while it was speaking, loop once more so that message
		// is appended and answered instead of waiting until a later
		// top-level prompt.
		if ctx.Err() == nil && a.QueuedMessageCount() > 0 {
			continue
		}

		// Terminal stop (end, length, error, aborted).
		sink(EvDone{})
		return nil
	}
	if a.MaxSteps > 0 {
		sink(EvDone{})
		return fmt.Errorf("max steps (%d) exceeded", a.MaxSteps)
	}
	return nil
}

// Turn performs exactly one model request against the current transcript and
// appends the assembled assistant message. It runs no tools and no agent-level
// retries, so a durable host can commit the request intent before and the
// response after this single boundary. The returned message is the complete
// assistant message even when the stream was aborted.
func (a *Agent) Turn(ctx context.Context, sink func(AgentEvent)) (provider.StopReason, provider.Message, error) {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink = a.wrapSink(sink)
	if err := ctx.Err(); err != nil {
		return provider.StopAborted, provider.Message{}, err
	}
	stop, msg, err := a.oneTurn(ctx, sink)
	sink(EvTurnEnd{Stop: stop, Err: err})
	return stop, msg, err
}

func (a *Agent) canRetryError(err error, attempt int) bool {
	if err == nil || a.MaxRetries <= 0 || attempt >= a.MaxRetries {
		return false
	}
	return RetryableProviderError(err)
}

// RetryableProviderError classifies a provider failure as transient. Context
// cancellation and usage or billing limits are never retryable.
func RetryableProviderError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if msg == "" || isNonRetryableProviderLimit(msg) {
		return false
	}
	needles := []string{
		"overloaded", "provider returned error", "rate limit", "ratelimit", "too many requests",
		"429", "http 429", "500", "http 500", "502", "http 502", "503", "http 503", "504", "http 504",
		"service unavailable", "server error", "internal error", "network error", "connection error",
		"connection refused", "connection lost", "fetch failed", "upstream connect", "reset before headers",
		"socket hang up", "ended without", "stream ended before", "did not get a response", "timed out",
		"timeout", "terminated", "unexpected eof", "transport failure",
		// OpenAI's ChatGPT/Codex backend returns this generic message (with a
		// request ID) for transient server failures and explicitly says
		// "You can retry your request".
		"an error occurred while processing your request",
		// Explicit retry guidance emitted by provider backends (OpenAI
		// Responses, AWS Bedrock stream exceptions) with varying prefixes.
		"you can retry your request", "try your request again", "please retry your request",
		// Capacity messages from the ChatGPT/Codex backend, e.g.
		// "Our servers are currently overloaded. Please try again later."
		// The trailing advice also shows up on its own for transient
		// capacity failures; usage/quota limits are filtered out above by
		// isNonRetryableProviderLimit before this list is consulted.
		"servers are currently overloaded", "servers are busy", "try again later",
	}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func isNonRetryableProviderLimit(msg string) bool {
	needles := []string{
		"usage limit", "monthly usage limit", "freeusagelimit", "gousagelimit",
		"available balance", "insufficient_quota", "out of budget", "quota exceeded", "billing",
	}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func (a *Agent) retryDelay(attempt int) time.Duration {
	base := a.RetryBaseDelay
	if base <= 0 {
		base = 2 * time.Second
	}
	return base * time.Duration(1<<attempt)
}

func sleepRetry(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *Agent) dropLastAssistantMessage() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.messages); n > 0 && a.messages[n-1].Role == provider.RoleAssistant {
		a.messages = a.messages[:n-1]
		a.rev++
	}
}

// oneTurn calls the LLM once, forwards events, returns the stop reason
// and the assembled assistant message (already appended to the transcript).
func (a *Agent) oneTurn(ctx context.Context, sink func(AgentEvent)) (provider.StopReason, provider.Message, error) {
	var req provider.Request
	for {
		if err := a.prepareStart(ctx); err != nil {
			return provider.StopError, provider.Message{}, err
		}
		a.mu.Lock()
		// A reset can also arrive after runLoop's preparation, for example
		// while BeforeTurn waits. Validate and snapshot under the same lock
		// so this request never receives an unprepared replacement prompt.
		if a.BeforeStart != nil && !a.startCurrentLocked() {
			a.mu.Unlock()
			continue
		}
		req = provider.Request{
			Model:  a.Model,
			System: a.System,
			// Repair any dangling tool_use blocks before sending. A turn
			// aborted mid-flight (cancel, connection drop, ECONNREFUSED to a
			// dev server, etc.) can leave an assistant tool_use with no
			// matching tool_result in the live transcript. The load-time
			// repair in OpenSession only runs on restart, so without this the
			// next in-process request is rejected by providers like Anthropic
			// with "tool_use ids were found without tool_result blocks". The
			// repair is pure and a no-op on already-valid transcripts.
			Messages:     repairToolUseResultPairs(withoutToolDisplay(a.messages)),
			Tools:        a.Tools.Specs(),
			Reasoning:    a.Reasoning,
			MaxTokens:    a.MaxTokens,
			Temperature:  a.Temperature,
			SessionID:    a.SessionID,
			MaxToolCalls: a.MaxToolCalls,
		}
		a.mu.Unlock()
		break
	}
	stream, err := a.Client.Stream(ctx, req)
	if err != nil {
		return provider.StopError, provider.Message{}, err
	}

	sink(EvAssistantStart{})

	var (
		stop     provider.StopReason
		finalErr error
		finalMsg provider.Message
	)

	for ev := range stream {
		switch e := ev.(type) {
		case provider.EventStart:
			// nothing
		case provider.EventTextDelta:
			sink(EvTextDelta{Delta: e.Delta})
		case provider.EventToolStart:
			sink(EvToolUseStart{ID: e.ID, Name: e.Name})
		case provider.EventToolArgs:
			sink(EvToolUseArgs{ID: e.ID, Delta: e.Delta})
		case provider.EventToolEnd:
			sink(EvToolUseEnd{ID: e.ID})
		case provider.EventUsage:
			cum := a.cost.Add(e.Usage)
			sink(EvUsage{Usage: e.Usage, Cumulative: cum})
			if a.OnUsage != nil {
				a.OnUsage(cum)
			}
		case provider.EventDone:
			stop = e.Stop
			finalErr = e.Err
			finalMsg = e.Message
		}
	}

	// Append assistant message to transcript. Aborted turns (Esc / Ctrl+C)
	// produce partial content. When the partial message is text only we
	// keep whatever was streamed up to the cancel so the user does not
	// lose visible work (a cut-off summary is still useful). If the
	// partial message already contained tool-call blocks we drop the
	// whole thing, because an unmatched tool_use would fail the next
	// turn with a tool_result mismatch error.
	keep := len(finalMsg.Content) > 0
	if stop == provider.StopAborted && keep {
		hasToolCall := false
		for _, c := range finalMsg.Content {
			if _, ok := c.(provider.ToolCallBlock); ok {
				hasToolCall = true
				break
			}
		}
		if hasToolCall {
			keep = false
		}
	}
	if keep {
		emit := finalMsg
		suppress := false

		// BeforeAssistantMessage hook: extensions can suppress or
		// rewrite the visible text. The transcript keeps the
		// model's original output so the model still sees what it
		// said on subsequent turns.
		if a.BeforeAssistantMessage != nil {
			orig := extractText(finalMsg)
			if orig != "" {
				allowed, _, replacement := a.BeforeAssistantMessage(orig)
				if !allowed {
					suppress = true
				} else if replacement != "" && replacement != orig {
					emit = replaceText(finalMsg, replacement)
				}
			}
		}

		a.mu.Lock()
		a.messages = append(a.messages, finalMsg)
		a.rev++
		a.mu.Unlock()
		a.fireMessageAppended(finalMsg)
		if !suppress {
			sink(EvAssistantMessage{Message: emit})
		}
		// Now surface tool calls as EvToolCall events so UIs can render them
		// in order before the tool results arrive.
		for _, c := range finalMsg.Content {
			if tc, ok := c.(provider.ToolCallBlock); ok {
				sink(EvToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Arguments})
			}
		}
	}

	return stop, finalMsg, finalErr
}

// CallTool executes a host-initiated tool without adding a model tool-use
// message to the transcript. The caller supplies a unique ID and event sink.
// It shares the model tool path's guards, confirmation hook and error handling.
func (a *Agent) CallTool(ctx context.Context, id, name string, args json.RawMessage, sink func(AgentEvent)) ToolResult {
	if sink == nil {
		sink = func(AgentEvent) {}
	}
	sink(EvToolCall{ID: id, Name: name, Args: args})
	return a.runOneTool(ctx, provider.ToolCallBlock{ID: id, Name: name, Arguments: args}, sink)
}

// executeTools runs every tool call in the assistant message and returns
// a single tool-role message carrying all results.
func (a *Agent) executeTools(ctx context.Context, msg provider.Message, sink func(AgentEvent)) (provider.Message, bool) {
	var results []provider.Content
	var addedTools []string
	state := map[string]json.RawMessage{}
	nested := map[string][]provider.NestedToolCall{}
	hadError := false

	for _, c := range msg.Content {
		tc, ok := c.(provider.ToolCallBlock)
		if !ok || tc.Server {
			continue
		}
		res := a.runOneTool(ctx, tc, sink)
		if len(res.NestedCalls) > 0 {
			nested[tc.ID] = res.NestedCalls
		}
		if res.IsError {
			hadError = true
		}
		for key, value := range res.State {
			state[key] = value
		}
		results = append(results, provider.ToolResultBlock{
			CallID:  tc.ID,
			Content: res.Content,
			IsError: res.IsError,
		})
		for _, name := range res.ActivateTools {
			if _, err := a.Tools.Get(name); err == nil && !containsString(addedTools, name) {
				addedTools = append(addedTools, name)
			}
		}
	}

	return provider.Message{
		Role:           provider.RoleTool,
		Content:        results,
		Time:           time.Now(),
		AddedToolNames: addedTools,
		Meta:           toolResultMetadata(state, nested),
	}, hadError
}

func (a *Agent) runOneTool(ctx context.Context, tc provider.ToolCallBlock, sink func(AgentEvent)) ToolResult {
	return a.runTool(ctx, tc, sink, nil)
}

// runTool also accepts host-provided operations that are not in the advertised
// registry. They still pass through the same guards and lifecycle handling.
func (a *Agent) runTool(ctx context.Context, tc provider.ToolCallBlock, sink func(AgentEvent), tool Tool) (result ToolResult) {
	args := tc.Arguments
	status := ""
	executed := false
	defer func() {
		if status == "" {
			status = "completed"
			if result.IsError {
				status = "failed"
				switch result.Status {
				case "cancelled", "timed_out", "blocked":
					status = result.Status
				}
			}
		}
		if sink != nil {
			sink(EvToolResult{ID: tc.ID, Name: tc.Name, Args: args, Status: status, Executed: executed, Result: result})
		}
	}()
	if err := ctx.Err(); err != nil {
		status = executionErrorStatus(err)
		return ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "aborted: " + err.Error()}}}
	}
	if tool == nil {
		a.mu.Lock()
		registered, err := a.Tools.Get(tc.Name)
		a.mu.Unlock()
		if err != nil {
			return ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
				IsError: true,
			}
		}
		tool = registered
	}

	// Intercept hook: an extension or other guard can refuse the
	// call before any side effect happens, OR rewrite the args
	// seen by the tool. The model sees the reason as the tool
	// error, learns from it, and (typically) proposes a different
	// action; rewrites are invisible to the model (they apply only
	// to the execution).
	before := a.BeforeToolExecute
	if a.BeforeToolExecuteContext != nil {
		before = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
			return a.BeforeToolExecuteContext(ctx, call)
		}
	}
	if before != nil {
		allowed, reason, modified := before(tc)
		if len(modified) > 0 && json.Valid(modified) {
			args = modified
		}
		if !allowed {
			status = "blocked"
			if ctx.Err() != nil {
				status = executionErrorStatus(ctx.Err())
			}
			if reason == "" {
				reason = "tool call refused by extension guard"
			}
			return ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: reason}},
				IsError: true,
			}
		}
	}

	if err := ctx.Err(); err != nil {
		status = executionErrorStatus(err)
		return ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "aborted: " + err.Error()}}}
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	// Recover panics so a buggy tool does not crash the agent.
	var res ToolResult
	var runtime *ToolRuntime
	func() {
		defer func() {
			if r := recover(); r != nil {
				res = ToolResult{
					Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("panic: %v", r)}},
					IsError: true,
				}
			}
		}()
		executed = true
		toolCtx := a.withToolRuntime(ctx, tc, sink)
		runtime = ToolRuntimeFromContext(toolCtx)
		out, err := tool.Execute(toolCtx, args, func(text string) {
			if sink != nil {
				sink(EvToolProgress{ID: tc.ID, Text: text})
			}
		})
		if err != nil {
			status = executionErrorStatus(err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				res = ToolResult{
					Content: []provider.Content{provider.TextBlock{Text: "aborted: " + err.Error()}},
					IsError: true,
					Status:  status,
				}
				return
			}
			res = ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
				IsError: true,
			}
			if status == "unknown" {
				// Preserve the distinction for durable hosts; the model
				// still sees an error result in ordinary sessions.
				res.Status = status
			}
			return
		}
		res = out
	}()
	res.NestedCalls = runtime.nestedCalls()
	for _, name := range runtime.activatedTools() {
		if !containsString(res.ActivateTools, name) {
			res.ActivateTools = append(res.ActivateTools, name)
		}
	}
	if res.IsError {
		res.State = nil
	}
	if !res.IsError && len(res.State) > 0 {
		a.mu.Lock()
		if a.toolState == nil {
			a.toolState = map[string]json.RawMessage{}
		}
		for key, value := range res.State {
			if json.Valid(value) {
				a.toolState[key] = append(json.RawMessage(nil), value...)
			}
		}
		a.mu.Unlock()
	}
	if res.Usage != nil && ToolRuntimeFromContext(ctx) == nil {
		a.mu.Lock()
		lastTurn := a.cost.LastTurn
		cumulative := a.cost.Add(*res.Usage)
		a.cost.LastTurn = lastTurn
		a.mu.Unlock()
		if sink != nil {
			sink(EvUsage{Usage: *res.Usage, Cumulative: cumulative, Auxiliary: true})
		}
		if a.OnUsage != nil {
			a.OnUsage(cumulative)
		}
	}
	return res
}

func executionErrorStatus(err error) string {
	if errors.Is(err, ErrToolOutcomeUnknown) {
		return "unknown"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed_out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var policyErr *ToolPolicyError
	if errors.As(err, &policyErr) {
		return "blocked"
	}
	return "failed"
}

// extractText concatenates all TextBlock content in a message. Used
// by BeforeAssistantMessage so guards see a single string instead of
// having to walk provider.Content themselves.
func mirrorToolImagesAsUser(msg provider.Message) provider.Message {
	var content []provider.Content
	for _, c := range msg.Content {
		tr, ok := c.(provider.ToolResultBlock)
		if !ok {
			continue
		}
		for _, inner := range tr.Content {
			switch v := inner.(type) {
			case provider.TextBlock:
				// Keep short textual context so the model understands why
				// the images appeared, but don't duplicate giant read
				// outputs verbatim.
				if len(v.Text) > 0 && len(v.Text) <= 500 {
					content = append(content, v)
				}
			case provider.ImageBlock:
				content = append(content, v)
			}
		}
	}
	if len(content) == 0 {
		return provider.Message{}
	}
	prefix := provider.TextBlock{Text: "Tool output included the following image content:"}
	content = append([]provider.Content{prefix}, content...)
	return provider.Message{Role: provider.RoleUser, Content: content, Time: time.Now()}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// MessageText joins the text blocks of a message with newlines. Tool calls,
// images, and reasoning blocks are omitted.
func MessageText(msg provider.Message) string { return extractText(msg) }

// ToolResultText joins the text blocks of a tool result for display and
// indexing. Images are omitted.
func ToolResultText(res ToolResult) string {
	return extractText(provider.Message{Content: res.Content})
}

func extractText(msg provider.Message) string {
	var out string
	for _, c := range msg.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if out != "" {
				out += "\n"
			}
			out += tb.Text
		}
	}
	return out
}

// replaceText returns a copy of msg with every TextBlock replaced by
// a single TextBlock containing replacement. Non-text content (tool
// calls, etc.) is preserved in order.
func replaceText(msg provider.Message, replacement string) provider.Message {
	out := provider.Message{Role: msg.Role}
	out.Content = make([]provider.Content, 0, len(msg.Content))
	replaced := false
	for _, c := range msg.Content {
		if _, ok := c.(provider.TextBlock); ok {
			if !replaced {
				out.Content = append(out.Content, provider.TextBlock{Text: replacement})
				replaced = true
			}
			continue
		}
		out.Content = append(out.Content, c)
	}
	if !replaced {
		out.Content = append(out.Content, provider.TextBlock{Text: replacement})
	}
	return out
}
