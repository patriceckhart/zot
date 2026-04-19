package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func (c *anthropicClient) Name() string { return "anthropic" }

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
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

type anthRequest struct {
	Model       string            `json:"model"`
	MaxTokens   int               `json:"max_tokens"`
	System      []anthSystemBlock `json:"system,omitempty"`
	Messages    []anthMessage     `json:"messages"`
	Tools       []anthTool        `json:"tools,omitempty"`
	Temperature *float32          `json:"temperature,omitempty"`
	Thinking    *anthThinking     `json:"thinking,omitempty"`
	Stream      bool              `json:"stream"`
}

// ---- request building ----

func (c *anthropicClient) buildRequest(req Request) (*anthRequest, error) {
	m, err := FindModel("anthropic", req.Model)
	if err != nil {
		return nil, err
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = m.MaxOutput
	}

	out := &anthRequest{
		Model:       req.Model,
		MaxTokens:   maxTok,
		Temperature: req.Temperature,
		Stream:      true,
	}

	// System prompt assembly differs between api-key and OAuth modes.
	// OAuth requests MUST begin with the Claude Code identity line or
	// Anthropic rejects them (429 rate_limit_error with zero tokens used).
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
		budget := anthropicReasoningBudget(req.Reasoning)
		if budget > 0 {
			out.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
			// Reasoning requires max_tokens > budget.
			if out.MaxTokens <= budget {
				out.MaxTokens = budget + 4096
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

	// Group messages: consecutive user/tool roles into one "user" message.
	// Anthropic only has roles "user" and "assistant"; tool_result blocks live in user messages.
	for _, msg := range req.Messages {
		renameTools := c.oauthTok != ""
		switch msg.Role {
		case RoleUser:
			out.Messages = append(out.Messages, anthMessage{
				Role:    "user",
				Content: convertAnthContent(msg.Content, renameTools),
			})
		case RoleTool:
			// Attach tool_result blocks to a user message; merge with prior user msg if last.
			blocks := convertAnthContent(msg.Content, renameTools)
			if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == "user" {
				out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			} else {
				out.Messages = append(out.Messages, anthMessage{Role: "user", Content: blocks})
			}
		case RoleAssistant:
			out.Messages = append(out.Messages, anthMessage{
				Role:    "assistant",
				Content: convertAnthContent(msg.Content, renameTools),
			})
		}
	}

	// Mark the last two user messages with cache_control so anthropic
	// caches the running conversation prefix. Combined with the system
	// + tools breakpoints above this is the recommended layout for
	// multi-turn caching: turn N writes a cache that turn N+1 reads,
	// dropping per-turn input tokens from "system + history" down to
	// just the new user message. Anthropic allows up to 4 breakpoints
	// per request; we use system + tools + 2 conversation = 4.
	tagUserCache(out.Messages)

	return out, nil
}

// tagUserCache attaches a cache_control marker to the last block of
// the most recent (and second-most-recent, if any) user message in
// msgs. The marker tells the api to checkpoint the prefix at that
// point so subsequent requests can replay everything up to and
// including that block as a cache hit.
func tagUserCache(msgs []anthMessage) {
	indexes := make([]int, 0, 2)
	for i := len(msgs) - 1; i >= 0 && len(indexes) < 2; i-- {
		if msgs[i].Role == "user" {
			indexes = append(indexes, i)
		}
	}
	for _, idx := range indexes {
		markLastBlockEphemeral(msgs[idx].Content)
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
	switch strings.ToLower(level) {
	case "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 16384
	default:
		return 0
	}
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
			out = append(out, anthImageBlock{
				Type: "image",
				Source: anthImageSource{
					Type:      "base64",
					MediaType: v.MimeType,
					Data:      base64.StdEncoding.EncodeToString(v.Data),
				},
			})
		case ToolCallBlock:
			args := v.Arguments
			if len(args) == 0 {
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
			arr = append(arr, anthImageBlock{
				Type: "image",
				Source: anthImageSource{
					Type:      "base64",
					MediaType: v.MimeType,
					Data:      base64.StdEncoding.EncodeToString(v.Data),
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
		httpReq.Header.Set("user-agent", "claude-cli/"+claudeCodeVersion+" (external, cli)")
		httpReq.Header.Set("x-app", "cli")
		// Remove x-api-key entirely by NOT setting it.
	} else {
		httpReq.Header.Set("accept", "text/event-stream")
		httpReq.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
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

	model, _ := FindModel("anthropic", req.Model)
	out <- EventStart{Model: req.Model, Provider: "anthropic"}

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
				usage.InputTokens += m.Message.Usage.InputTokens
				usage.OutputTokens += m.Message.Usage.OutputTokens
				usage.CacheReadTokens += m.Message.Usage.CacheReadInputTokens
				usage.CacheWriteTokens += m.Message.Usage.CacheCreationInputTokens
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
				usage.InputTokens += m.Usage.InputTokens
				usage.OutputTokens += m.Usage.OutputTokens
				usage.CacheReadTokens += m.Usage.CacheReadInputTokens
				usage.CacheWriteTokens += m.Usage.CacheCreationInputTokens
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
