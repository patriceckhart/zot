package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const anthropicDefaultBaseURL = "https://api.anthropic.com"
const anthropicAPIVersion = "2023-06-01"

// Stealth identity used when talking to Anthropic via subscription OAuth.
// These values mimic the official Claude Code CLI so Anthropic's edge
// accepts the request; diverging from them causes 429 rate_limit_error
// or 403 on the very first request.
const (
	claudeCodeVersion  = "2.1.75"
	claudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
)

// Claude Code's canonical tool casing. When running under OAuth we must
// advertise tool names that match this list (case-insensitive lookup),
// because Anthropic's backend cross-checks them.
var claudeCodeToolNames = map[string]string{
	"read":  "Read",
	"write": "Write",
	"edit":  "Edit",
	"bash":  "Bash",
	"grep":  "Grep",
	"glob":  "Glob",
}

func toClaudeCodeToolName(name string) string {
	if cc, ok := claudeCodeToolNames[strings.ToLower(name)]; ok {
		return cc
	}
	return name
}

func fromClaudeCodeToolName(name string, tools []Tool) string {
	lower := strings.ToLower(name)
	for _, t := range tools {
		if strings.ToLower(t.Name) == lower {
			return t.Name
		}
	}
	return name
}

// anthropicClient implements Client against the Anthropic Messages API.
type anthropicClient struct {
	apiKey   string
	baseURL  string
	oauthTok string // when non-empty, send Bearer auth instead of x-api-key
	http     *http.Client

	// name overrides the default "anthropic" identity. Anthropic-Messages-
	// compatible third-party endpoints (kimi-coding, fireworks, minimax,
	// vercel-ai-gateway, etc.) set this so cost lookup / logging / image-
	// stripping logic can route on a stable provider id.
	name string

	// headers carries extra request headers (e.g. Kimi Code's X-Msh-*).
	headers map[string]string
}

// NewAnthropic creates an Anthropic client using an API key. baseURL may be empty.
func NewAnthropic(apiKey, baseURL string) Client {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	return &anthropicClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 0},
	}
}

// NewAnthropicOAuth creates an Anthropic client using a subscription OAuth access token.
func NewAnthropicOAuth(accessToken, baseURL string) Client {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	return &anthropicClient{
		oauthTok: accessToken,
		baseURL:  strings.TrimRight(baseURL, "/"),
		http:     &http.Client{Timeout: 0},
	}
}

func (c *anthropicClient) Name() string {
	if c.name != "" {
		return c.name
	}
	return "anthropic"
}

// ---- wire types ----

type anthTextBlock struct {
	Type         string         `json:"type"` // "text"
	Text         string         `json:"text"`
	CacheControl *anthCacheCtrl `json:"cache_control,omitempty"`
}

type anthImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthImageBlock struct {
	Type         string          `json:"type"` // "image"
	Source       anthImageSource `json:"source"`
	CacheControl *anthCacheCtrl  `json:"cache_control,omitempty"`
}

type anthToolUseBlock struct {
	Type         string          `json:"type"` // "tool_use"
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	CacheControl *anthCacheCtrl  `json:"cache_control,omitempty"`
}

type anthToolResultBlock struct {
	Type         string          `json:"type"` // "tool_result"
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"` // string or array of blocks
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl *anthCacheCtrl  `json:"cache_control,omitempty"`
}

type anthCacheCtrl struct {
	Type string `json:"type"` // "ephemeral"
	TTL  string `json:"ttl,omitempty"`
}

type anthMessage struct {
	Role    string        `json:"role"`
	Content []interface{} `json:"content"`
}

type anthSystemBlock struct {
	Type         string         `json:"type"` // "text"
	Text         string         `json:"text"`
	CacheControl *anthCacheCtrl `json:"cache_control,omitempty"`
}

type anthTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *anthCacheCtrl  `json:"cache_control,omitempty"`
}

type anthThinking struct {
	// Type is "enabled" for budget-based thinking (Opus 4.6 and
	// earlier) or "adaptive" for adaptive-thinking models (Opus 4.7+).
	Type string `json:"type"`
	// BudgetTokens is only sent for Type=="enabled". Adaptive models
	// reject it (400), so it is omitted for them.
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

// anthOutputConfig carries the effort knob used by adaptive-thinking
// models to control reasoning depth ("low"|"medium"|"high"|"xhigh").
type anthOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	System       []anthSystemBlock `json:"system,omitempty"`
	Messages     []anthMessage     `json:"messages"`
	Tools        []anthTool        `json:"tools,omitempty"`
	Temperature  *float32          `json:"temperature,omitempty"`
	Thinking     *anthThinking     `json:"thinking,omitempty"`
	OutputConfig *anthOutputConfig `json:"output_config,omitempty"`
	Stream       bool              `json:"stream"`
}

// usesAdaptiveThinking reports whether a model only supports the
// adaptive thinking mode (Opus 4.7 and later). These models reject
// explicit thinking budgets and non-default sampling parameters. The
// catalog flag is authoritative; the id-substring fallback catches
// the same family when reached through an Anthropic-Messages-
// compatible proxy whose catalog row predates the flag.
func usesAdaptiveThinking(m Model) bool {
	if m.AdaptiveThinking {
		return true
	}
	id := strings.ToLower(m.ID)
	for _, marker := range []string{"opus-4-7", "opus-4.7", "opus-4-8", "opus-4.8", "sonnet-5", "fable-5"} {
		if strings.Contains(id, marker) {
			return true
		}
	}
	return false
}

// ---- request building ----

func (c *anthropicClient) buildRequest(req Request) (*anthRequest, error) {
	// Look up under the client's actual provider id, which may be "anthropic"
	// (default) or a third party that speaks the Anthropic Messages API
	// (kimi, fireworks, minimax, vercel-ai-gateway, ...). Falling back to a
	// provider-agnostic lookup keeps things working for catalog-less
	// configurations (e.g. user passes --model on an obscure third party).
	m, err := FindModel(c.Name(), req.Model)
	if err != nil {
		if m2, err2 := FindModel("", req.Model); err2 == nil {
			m = m2
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = m.MaxOutput
	}

	adaptive := usesAdaptiveThinking(m)

	out := &anthRequest{
		Model:     req.Model,
		MaxTokens: maxTok,
		Stream:    true,
	}
	// Adaptive-thinking models reject non-default sampling params
	// (temperature/top_p/top_k -> 400). Only forward temperature for
	// models that accept it.
	if !adaptive {
		out.Temperature = req.Temperature
	}

	// System prompt assembly differs between api-key and OAuth modes.
	// OAuth requests MUST begin with the Claude Code identity line or
	// Anthropic rejects them (429 rate_limit_error with zero tokens used).
	//
	// Cache budget: anthropic caps cache_control to 4 breakpoints per
	// request. We spend them on:
	//   1. claude-code identity (OAuth only; stable forever)
	//   2. user system prompt   (changes per-session at most)
	//   3. last tool definition (tools change rarely)
	//   4. last message block   (advances every turn)
	//
	// The identity line gets its OWN cache_control so the prefix
	// [identity] is cacheable independently of the user system
	// prompt. Without that, the cache prefix starts after block 2
	// and any drift in the user prompt (e.g. the Current date
	// line flipping at midnight) invalidates everything, including
	// the 17 identity tokens we have to re-send every request
	// forever.
	if c.oauthTok != "" {
		out.System = []anthSystemBlock{{
			Type:         "text",
			Text:         claudeCodeIdentity,
			CacheControl: &anthCacheCtrl{Type: "ephemeral"},
		}}
		if req.System != "" {
			out.System = append(out.System, anthSystemBlock{
				Type:         "text",
				Text:         req.System,
				CacheControl: &anthCacheCtrl{Type: "ephemeral"},
			})
		}
	} else if req.System != "" {
		out.System = []anthSystemBlock{{
			Type:         "text",
			Text:         req.System,
			CacheControl: &anthCacheCtrl{Type: "ephemeral"},
		}}
	}

	if req.Reasoning != "" && m.Reasoning {
		if adaptive {
			// Adaptive-thinking models (Opus 4.7+): the model decides when
			// and how much to think; depth is steered by output_config.effort.
			// Explicit budgets are rejected with a 400, so none is sent.
			out.Thinking = &anthThinking{Type: "adaptive"}
			if effort := AnthropicAdaptiveEffort(req.Reasoning); effort != "" {
				out.OutputConfig = &anthOutputConfig{Effort: effort}
			}
		} else {
			budget := anthropicReasoningBudget(req.Reasoning)
			if budget > 0 {
				// Reasoning requires max_tokens > budget. Keep at least a small
				// answer budget while respecting the model's advertised output cap.
				const minAnswerTokens = 1024
				if m.MaxOutput > minAnswerTokens && budget >= m.MaxOutput {
					budget = m.MaxOutput - minAnswerTokens
				}
				out.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
				if out.MaxTokens <= budget {
					out.MaxTokens = budget + minAnswerTokens
					if m.MaxOutput > 0 && out.MaxTokens > m.MaxOutput {
						out.MaxTokens = m.MaxOutput
					}
				}
			}
		}
	}

	for _, t := range req.Tools {
		name := t.Name
		if c.oauthTok != "" {
			name = toClaudeCodeToolName(name)
		}
		out.Tools = append(out.Tools, anthTool{
			Name:        name,
			Description: t.Description,
			InputSchema: t.Schema,
		})
	}
	// Cache the last tool definition (applies cache breakpoint to the whole tools array).
	if n := len(out.Tools); n > 0 {
		out.Tools[n-1].CacheControl = &anthCacheCtrl{Type: "ephemeral"}
	}

	// Convert messages. Anthropic's wire format has only "user" and
	// "assistant" roles; tool_result blocks live inside user messages.
	// Some image-capable providers (for example Gemini image generation)
	// can emit assistant image blocks. Anthropic only accepts image blocks
	// in user messages, so strip assistant-side images when switching
	// providers mid-session. The saved-path TextBlock emitted beside the
	// image keeps the artifact reachable in chat.
	//
	// CRITICAL: tool_result blocks go into their OWN new user
	// message, they are NOT merged into the preceding user message.
	// Merging would mutate the prior user message's content array
	// between turn N and turn N+1: turn N caches the prefix ending at
	// [user: "read sample.ts"], turn N+1 sends
	// [user: "read sample.ts" + tool_result=...] which is a
	// different block sequence, busting the cache prefix match.
	// Anthropic's API happily accepts consecutive user messages, and
	// emitting them separately keeps each message bit-stable across
	// turns, so the cache prefix matches for the entire history up
	// to the newest block.
	req.Messages = RepairOrphanedToolResults(req.Messages)
	for _, msg := range req.Messages {
		renameTools := c.oauthTok != ""
		switch msg.Role {
		case RoleUser:
			out.Messages = append(out.Messages, anthMessage{
				Role:    "user",
				Content: convertAnthContent(msg.Content, renameTools),
			})
		case RoleTool:
			out.Messages = append(out.Messages, anthMessage{
				Role:    "user",
				Content: convertAnthContent(msg.Content, renameTools),
			})
		case RoleAssistant:
			out.Messages = append(out.Messages, anthMessage{
				Role:    "assistant",
				Content: convertAnthContent(filterAnthAssistantContent(msg.Content), renameTools),
			})
		}
	}

	// Tag the LAST user message with cache_control. Spends the 4th
	// breakpoint. For prefixes under ~1024 tokens (Anthropic's
	// minimum cacheable block size for Opus), no cache is written.
	tagLastUserCache(out.Messages)

	return out, nil
}

// tagLastUserCache marks the last block of the most recent user
// message. One marker; combined with identity + systemPrompt +
// last-tool, spends Anthropic's 4-breakpoint budget.
func tagLastUserCache(msgs []anthMessage) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			markLastBlockEphemeral(msgs[i].Content)
			return
		}
	}
}

// markLastBlockEphemeral sets CacheControl on the last entry in blocks
// regardless of whether it's a text, image, tool_use, or tool_result.
// Each block type carries its own CacheControl pointer so we type-
// switch + reassign the slice element.
func markLastBlockEphemeral(blocks []interface{}) {
	if len(blocks) == 0 {
		return
	}
	i := len(blocks) - 1
	cc := &anthCacheCtrl{Type: "ephemeral"}
	switch v := blocks[i].(type) {
	case anthTextBlock:
		v.CacheControl = cc
		blocks[i] = v
	case anthImageBlock:
		v.CacheControl = cc
		blocks[i] = v
	case anthToolUseBlock:
		v.CacheControl = cc
		blocks[i] = v
	case anthToolResultBlock:
		v.CacheControl = cc
		blocks[i] = v
	}
}

func anthropicReasoningBudget(level string) int {
	return ReasoningBudget(level)
}

func filterAnthAssistantContent(blocks []Content) []Content {
	filtered := make([]Content, 0, len(blocks))
	for _, b := range blocks {
		if _, ok := b.(ImageBlock); ok {
			continue
		}
		filtered = append(filtered, b)
	}
	return filtered
}

func convertAnthContent(blocks []Content, renameTools bool) []interface{} {
	out := make([]interface{}, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case TextBlock:
			if v.Text == "" {
				continue
			}
			out = append(out, anthTextBlock{Type: "text", Text: v.Text})
		case ImageBlock:
			data, mime := anthShrinkImageBytesIfTooBig(v.Data, v.MimeType)
			out = append(out, anthImageBlock{
				Type: "image",
				Source: anthImageSource{
					Type:      "base64",
					MediaType: mime,
					Data:      base64.StdEncoding.EncodeToString(data),
				},
			})
		case ToolCallBlock:
			args := v.Arguments
			if len(args) == 0 || !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			name := v.Name
			if renameTools {
				name = toClaudeCodeToolName(name)
			}
			out = append(out, anthToolUseBlock{
				Type: "tool_use", ID: v.ID, Name: name, Input: args,
			})
		case ToolResultBlock:
			// Flatten content to a string if all text; else array of blocks.
			content, _ := anthBuildToolResultContent(v.Content)
			out = append(out, anthToolResultBlock{
				Type:      "tool_result",
				ToolUseID: v.CallID,
				Content:   content,
				IsError:   v.IsError,
			})
		}
	}
	return out
}

func anthBuildToolResultContent(blocks []Content) (json.RawMessage, error) {
	onlyText := true
	var sb strings.Builder
	for _, b := range blocks {
		if tb, ok := b.(TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tb.Text)
		} else {
			onlyText = false
			break
		}
	}
	if onlyText {
		if sb.Len() == 0 {
			return json.Marshal("")
		}
		return json.Marshal(sb.String())
	}
	// Array form: text + image blocks.
	arr := make([]interface{}, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case TextBlock:
			arr = append(arr, anthTextBlock{Type: "text", Text: v.Text})
		case ImageBlock:
			data, mime := anthShrinkImageBytesIfTooBig(v.Data, v.MimeType)
			arr = append(arr, anthImageBlock{
				Type: "image",
				Source: anthImageSource{
					Type:      "base64",
					MediaType: mime,
					Data:      base64.StdEncoding.EncodeToString(data),
				},
			})
		}
	}
	return json.Marshal(arr)
}

// ---- streaming ----

func (c *anthropicClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	// Optional debug dump: when $ZOT_DEBUG_ANTHROPIC is a file path
	// we append every outgoing request body to it, one JSON object
	// per line. Useful for diffing turn N vs turn N+1 to understand
	// why the cache prefix isn't matching.
	if dump := os.Getenv("ZOT_DEBUG_ANTHROPIC"); dump != "" {
		if f, derr := os.OpenFile(dump, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); derr == nil {
			_, _ = f.Write(body)
			_, _ = f.Write([]byte{'\n'})
			_ = f.Close()
		}
	}

	newReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("anthropic-version", anthropicAPIVersion)
		if c.oauthTok != "" {
			// Claude-Code-shaped request: identical headers and values as the
			// official CLI. Any drift triggers Anthropic's anti-abuse check and
			// rate-limits (or outright blocks) the request.
			httpReq.Header.Set("accept", "application/json")
			httpReq.Header.Set("authorization", "Bearer "+c.oauthTok)
			httpReq.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,fine-grained-tool-streaming-2025-05-14")
			httpReq.Header.Set("anthropic-dangerous-direct-browser-access", "true")
			httpReq.Header.Set("user-agent", "claude-cli/"+claudeCodeVersion)
			httpReq.Header.Set("x-app", "cli")
			// Remove x-api-key entirely by NOT setting it.
		} else {
			httpReq.Header.Set("accept", "text/event-stream")
			httpReq.Header.Set("x-api-key", c.apiKey)
		}
		// Extra headers (set by anthropic-messages-compatible third parties
		// — kimi-coding's X-Msh-*, copilot's Editor-Plugin-Version, etc.).
		// Applied last so callers can override defaults if they really need to.
		for k, v := range c.headers {
			httpReq.Header.Set(k, v)
		}
		return httpReq, nil
	}

	resp, err := doStreamWithRetry(ctx, c.http, newReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	out := make(chan Event, 16)
	go c.runStream(ctx, resp, req, out)
	return out, nil
}

func (c *anthropicClient) runStream(ctx context.Context, resp *http.Response, req Request, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	// Same lookup-by-actual-provider-id pattern as buildRequest, so cost
	// calculation works for third-party Anthropic-Messages endpoints.
	model, _ := FindModel(c.Name(), req.Model)
	if model.ID == "" {
		model, _ = FindModel("", req.Model)
	}
	out <- EventStart{Model: req.Model, Provider: c.Name()}

	raw := make(chan sseEvent, 16)
	go readSSE(resp.Body, raw)

	// State for assembling the assistant message. Blocks are indexed
	// by their `index` field from Anthropic so we can preserve the
	// interleaved order the model emitted them in (text may come
	// before OR after tool_use; mixing both happens frequently).
	type blockEntry struct {
		kind     string // "text" | "tool_use"
		textBuf  strings.Builder
		toolCall ToolCallBlock
		toolArgs strings.Builder
	}
	var (
		blocks     = map[int]*blockEntry{}
		blockOrder []int // insertion order of indexes
		activeIdx  = -1
		usage      Usage
		stop       StopReason = StopEnd
		finalErr   error
	)
	_ = activeIdx // read-only indicator used for legacy parity

	registerBlock := func(idx int, kind string) *blockEntry {
		if be, ok := blocks[idx]; ok {
			return be
		}
		be := &blockEntry{kind: kind}
		blocks[idx] = be
		blockOrder = append(blockOrder, idx)
		return be
	}

	assembleMsg := func() Message {
		content := []Content{}
		for _, idx := range blockOrder {
			be := blocks[idx]
			switch be.kind {
			case "text":
				if be.textBuf.Len() > 0 {
					content = append(content, TextBlock{Text: be.textBuf.String()})
				}
			case "tool_use":
				tc := be.toolCall
				args := be.toolArgs.String()
				if args == "" {
					args = "{}"
				}
				tc.Arguments = json.RawMessage(args)
				content = append(content, tc)
			}
		}
		return Message{Role: RoleAssistant, Content: content, Time: time.Now()}
	}

	sendDone := func() {
		usage.CostUSD = ComputeCost(model, usage)
		out <- EventUsage{Usage: usage}
		out <- EventDone{Stop: stop, Err: finalErr, Message: assembleMsg()}
	}

	for {
		select {
		case <-ctx.Done():
			stop = StopAborted
			finalErr = ctx.Err()
			sendDone()
			return
		case ev, ok := <-raw:
			if !ok {
				sendDone()
				return
			}
			// Parse event payload based on event: type.
			var payload map[string]json.RawMessage
			if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
				continue
			}
			switch ev.Event {
			case "content_block_start":
				var idx int
				if b, ok := payload["index"]; ok {
					_ = json.Unmarshal(b, &idx)
				}
				var block struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Text  string          `json:"text"`
					Input json.RawMessage `json:"input"`
				}
				if b, ok := payload["content_block"]; ok {
					_ = json.Unmarshal(b, &block)
				}
				activeIdx = idx
				switch block.Type {
				case "tool_use":
					name := block.Name
					if c.oauthTok != "" {
						name = fromClaudeCodeToolName(name, req.Tools)
					}
					be := registerBlock(idx, "tool_use")
					be.toolCall = ToolCallBlock{ID: block.ID, Name: name}
					out <- EventToolStart{ID: block.ID, Name: name}
				case "text":
					registerBlock(idx, "text")
				case "thinking":
					// not surfaced
				}
			case "content_block_delta":
				var idx int
				if b, ok := payload["index"]; ok {
					_ = json.Unmarshal(b, &idx)
				}
				var d struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
					Thinking    string `json:"thinking"`
				}
				if b, ok := payload["delta"]; ok {
					_ = json.Unmarshal(b, &d)
				}
				switch d.Type {
				case "text_delta":
					if be, ok := blocks[idx]; ok && be.kind == "text" {
						be.textBuf.WriteString(d.Text)
					}
					out <- EventTextDelta{Delta: d.Text}
				case "input_json_delta":
					if be, ok := blocks[idx]; ok && be.kind == "tool_use" {
						be.toolArgs.WriteString(d.PartialJSON)
						out <- EventToolArgs{ID: be.toolCall.ID, Delta: d.PartialJSON}
					}
				case "thinking_delta":
					// Not surfaced in v1.
				}
			case "content_block_stop":
				var idx int
				if b, ok := payload["index"]; ok {
					_ = json.Unmarshal(b, &idx)
				}
				if be, ok := blocks[idx]; ok && be.kind == "tool_use" {
					out <- EventToolEnd{ID: be.toolCall.ID}
				}
				activeIdx = -1
			case "message_start":
				var m struct {
					Message struct {
						Usage struct {
							InputTokens              int `json:"input_tokens"`
							OutputTokens             int `json:"output_tokens"`
							CacheReadInputTokens     int `json:"cache_read_input_tokens"`
							CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &m)
				// Anthropic sends cumulative values on message_start and
				// again on message_delta (refreshed), so assign, don't
				// accumulate. Accumulating doubles cache_creation_input
				// which can be 50-70% of cost.
				usage.InputTokens = m.Message.Usage.InputTokens
				usage.OutputTokens = m.Message.Usage.OutputTokens
				usage.CacheReadTokens = m.Message.Usage.CacheReadInputTokens
				usage.CacheWriteTokens = m.Message.Usage.CacheCreationInputTokens
			case "message_delta":
				var m struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						OutputTokens             int `json:"output_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &m)
				// Refresh usage from the latest cumulative totals
				// Anthropic provides. Only apply non-zero fields in case
				// a given delta only carries output tokens.
				if m.Usage.InputTokens > 0 {
					usage.InputTokens = m.Usage.InputTokens
				}
				if m.Usage.OutputTokens > 0 {
					usage.OutputTokens = m.Usage.OutputTokens
				}
				if m.Usage.CacheReadInputTokens > 0 {
					usage.CacheReadTokens = m.Usage.CacheReadInputTokens
				}
				if m.Usage.CacheCreationInputTokens > 0 {
					usage.CacheWriteTokens = m.Usage.CacheCreationInputTokens
				}
				switch m.Delta.StopReason {
				case "end_turn", "stop_sequence":
					stop = StopEnd
				case "max_tokens":
					stop = StopLength
				case "tool_use":
					stop = StopToolUse
				}
			case "message_stop":
				sendDone()
				return
			case "error":
				var e struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &e)
				stop = StopError
				finalErr = fmt.Errorf("anthropic %s: %s", e.Error.Type, e.Error.Message)
				sendDone()
				return
			}
			_ = activeIdx
		}
	}
}
