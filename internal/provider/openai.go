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

const openaiDefaultBaseURL = "https://api.openai.com"

type openaiClient struct {
	apiKey  string
	baseURL string
	oauth   bool // when true, apiKey actually holds an OAuth access token
	http    *http.Client
}

// NewOpenAI creates an OpenAI client using an API key. baseURL may be empty.
func NewOpenAI(apiKey, baseURL string) Client {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	return &openaiClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 0},
	}
}

// NewOpenAIOAuth creates an OpenAI client using a subscription OAuth access token.
// The token is sent as an HTTP Bearer credential on the standard chat/completions endpoint.
func NewOpenAIOAuth(accessToken, baseURL string) Client {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	return &openaiClient{
		apiKey:  accessToken,
		baseURL: strings.TrimRight(baseURL, "/"),
		oauth:   true,
		http:    &http.Client{Timeout: 0},
	}
}

func (c *openaiClient) Name() string { return "openai" }

// ---- wire types ----

type oaiContentText struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type oaiContentImage struct {
	Type     string `json:"type"` // "image_url"
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type oaiToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

type oaiToolCall struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"` // "function"
	Function oaiToolCallFn `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    interface{}   `json:"content,omitempty"` // string or []block
	Name       string        `json:"name,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiRequest struct {
	Model            string            `json:"model"`
	Messages         []oaiMessage      `json:"messages"`
	Tools            []oaiTool         `json:"tools,omitempty"`
	ToolChoice       string            `json:"tool_choice,omitempty"`
	Stream           bool              `json:"stream"`
	StreamOptions    *oaiStreamOptions `json:"stream_options,omitempty"`
	Temperature      *float32          `json:"temperature,omitempty"`
	MaxTokens        *int              `json:"max_tokens,omitempty"`
	MaxCompletionTok *int              `json:"max_completion_tokens,omitempty"`
	ReasoningEffort  string            `json:"reasoning_effort,omitempty"`
}

// ---- request building ----

func (c *openaiClient) buildRequest(req Request) (*oaiRequest, error) {
	m, err := FindModel("openai", req.Model)
	if err != nil {
		return nil, err
	}
	out := &oaiRequest{
		Model:         req.Model,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
		Temperature:   req.Temperature,
	}

	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = m.MaxOutput
	}
	if m.Reasoning {
		out.MaxCompletionTok = &maxTok
		if req.Reasoning != "" {
			out.ReasoningEffort = strings.ToLower(req.Reasoning)
		}
	} else {
		out.MaxTokens = &maxTok
	}

	if req.System != "" {
		out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: req.System})
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case RoleUser:
			content := buildOAIUserContent(msg.Content)
			out.Messages = append(out.Messages, oaiMessage{Role: "user", Content: content})
		case RoleAssistant:
			am := oaiMessage{Role: "assistant"}
			var text strings.Builder
			for _, b := range msg.Content {
				switch v := b.(type) {
				case TextBlock:
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(v.Text)
				case ToolCallBlock:
					args := v.Arguments
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					am.ToolCalls = append(am.ToolCalls, oaiToolCall{
						ID:   v.ID,
						Type: "function",
						Function: oaiToolCallFn{
							Name:      v.Name,
							Arguments: string(args),
						},
					})
				}
			}
			if text.Len() > 0 {
				am.Content = text.String()
			}
			out.Messages = append(out.Messages, am)
		case RoleTool:
			// Each ToolResultBlock becomes its own tool message.
			for _, b := range msg.Content {
				if tr, ok := b.(ToolResultBlock); ok {
					var text strings.Builder
					for _, inner := range tr.Content {
						if tb, ok := inner.(TextBlock); ok {
							if text.Len() > 0 {
								text.WriteString("\n")
							}
							text.WriteString(tb.Text)
						}
					}
					if tr.IsError && text.Len() > 0 {
						text.WriteString(" [error]")
					}
					out.Messages = append(out.Messages, oaiMessage{
						Role:       "tool",
						ToolCallID: tr.CallID,
						Content:    text.String(),
					})
				}
			}
		}
	}

	for _, t := range req.Tools {
		var tool oaiTool
		tool.Type = "function"
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = t.Schema
		out.Tools = append(out.Tools, tool)
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = "auto"
	}

	return out, nil
}

func buildOAIUserContent(blocks []Content) interface{} {
	hasImage := false
	for _, b := range blocks {
		if _, ok := b.(ImageBlock); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		var sb strings.Builder
		for _, b := range blocks {
			if tb, ok := b.(TextBlock); ok {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(tb.Text)
			}
		}
		return sb.String()
	}
	var arr []interface{}
	for _, b := range blocks {
		switch v := b.(type) {
		case TextBlock:
			arr = append(arr, oaiContentText{Type: "text", Text: v.Text})
		case ImageBlock:
			var img oaiContentImage
			img.Type = "image_url"
			img.ImageURL.URL = "data:" + v.MimeType + ";base64," + base64.StdEncoding.EncodeToString(v.Data)
			arr = append(arr, img)
		}
	}
	return arr
}

// ---- streaming ----

func (c *openaiClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	httpReq.Header.Set("authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	out := make(chan Event, 16)
	go c.runStream(ctx, resp, req, out)
	return out, nil
}

func (c *openaiClient) runStream(ctx context.Context, resp *http.Response, req Request, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	model, _ := FindModel("openai", req.Model)
	out <- EventStart{Model: req.Model, Provider: "openai"}

	raw := make(chan sseEvent, 16)
	go readSSE(resp.Body, raw)

	// Interleaved block tracking: text and tool_calls preserve their
	// emission order so the assistant message renders in the same order
	// the model produced it. The builder fires one kind of block at a
	// time — incoming text deltas after a tool_call split into a fresh
	// text block; subsequent tool_calls each get their own slot.
	type blockEntry struct {
		kind      string // "text" | "tool_use"
		textBuf   strings.Builder
		toolID    string
		toolName  string
		toolArgs  strings.Builder
		announced bool
	}
	var (
		blocks      []*blockEntry
		currentText *blockEntry             // most-recent text block, nil if none
		toolByIdx   = map[int]*blockEntry{} // openai tool_call index -> block
		usage       Usage
		stop        StopReason = StopEnd
		finalErr    error
	)

	appendText := func(delta string) {
		if currentText == nil {
			currentText = &blockEntry{kind: "text"}
			blocks = append(blocks, currentText)
		}
		currentText.textBuf.WriteString(delta)
	}

	getOrCreateTool := func(idx int) *blockEntry {
		if t, ok := toolByIdx[idx]; ok {
			return t
		}
		t := &blockEntry{kind: "tool_use"}
		toolByIdx[idx] = t
		blocks = append(blocks, t)
		// A new tool block breaks the current text block. Subsequent text
		// deltas will start a fresh text block after this tool.
		currentText = nil
		return t
	}

	assembleMsg := func() Message {
		content := []Content{}
		for _, b := range blocks {
			switch b.kind {
			case "text":
				if b.textBuf.Len() > 0 {
					content = append(content, TextBlock{Text: b.textBuf.String()})
				}
			case "tool_use":
				args := b.toolArgs.String()
				if args == "" {
					args = "{}"
				}
				content = append(content, ToolCallBlock{
					ID: b.toolID, Name: b.toolName, Arguments: json.RawMessage(args),
				})
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
			if ev.Data == "[DONE]" {
				sendDone()
				return
			}
			var chunk struct {
				Choices []struct {
					Index int `json:"index"`
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens        int `json:"prompt_tokens"`
					CompletionTokens    int `json:"completion_tokens"`
					PromptTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"prompt_tokens_details"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				continue
			}
			if chunk.Error != nil {
				stop = StopError
				finalErr = fmt.Errorf("openai: %s", chunk.Error.Message)
				sendDone()
				return
			}
			if chunk.Usage != nil {
				usage.InputTokens = chunk.Usage.PromptTokens - chunk.Usage.PromptTokensDetails.CachedTokens
				if usage.InputTokens < 0 {
					usage.InputTokens = chunk.Usage.PromptTokens
				}
				usage.OutputTokens = chunk.Usage.CompletionTokens
				usage.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.Content != "" {
					appendText(ch.Delta.Content)
					out <- EventTextDelta{Delta: ch.Delta.Content}
				}
				for _, tc := range ch.Delta.ToolCalls {
					t := getOrCreateTool(tc.Index)
					if tc.ID != "" {
						t.toolID = tc.ID
					}
					if tc.Function.Name != "" {
						t.toolName = tc.Function.Name
					}
					if !t.announced && t.toolID != "" && t.toolName != "" {
						t.announced = true
						out <- EventToolStart{ID: t.toolID, Name: t.toolName}
					}
					if tc.Function.Arguments != "" {
						t.toolArgs.WriteString(tc.Function.Arguments)
						if t.announced {
							out <- EventToolArgs{ID: t.toolID, Delta: tc.Function.Arguments}
						}
					}
				}
				switch ch.FinishReason {
				case "stop":
					stop = StopEnd
				case "length":
					stop = StopLength
				case "tool_calls", "function_call":
					stop = StopToolUse
					for _, b := range blocks {
						if b.kind == "tool_use" && b.announced {
							out <- EventToolEnd{ID: b.toolID}
						}
					}
				}
			}
		}
	}
}
