package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// Codex (ChatGPT subscription) client. Uses OpenAI's Responses API via
// chatgpt.com/backend-api with the chatgpt-account-id handshake.
//
// Wire protocol notes:
//   - Endpoint: POST https://chatgpt.com/backend-api/codex/responses
//   - Headers: Authorization: Bearer <access_token>, chatgpt-account-id: <id>,
//     OpenAI-Beta: responses=experimental, originator: zot
//   - Body: OpenAI Responses API shape (not chat/completions).
//     input: [{role, content: [{type: "input_text" | "input_image" | ... }]}]
//     instructions: <system prompt>
//     tools: [{type:"function", name, description, parameters, strict}]
//     stream: true
//   - SSE events: response.output_item.added, response.output_text.delta,
//     response.function_call_arguments.delta, response.output_item.done,
//     response.completed, response.failed, error
//
// The access_token comes from the OpenAI OAuth flow (auth.openai.com).
// The accountID is parsed from the id_token JWT's `chatgpt_account_id`
// claim; see auth/oauth.go.

const codexDefaultBaseURL = "https://chatgpt.com/backend-api/codex/responses"

type codexClient struct {
	token             string
	accountID         string
	baseURL           string
	errorLabel        string
	providerName      string
	modelName         func(string) string
	disableCLIRouting bool
	cliRoutingAll     bool
	http              *http.Client
}

// NewOpenAICodex creates a client that talks to ChatGPT's Codex endpoint
// using a subscription OAuth access token and the user's ChatGPT
// account id. baseURL may be empty to use the default.
func NewOpenAICodex(token, accountID, baseURL string) Client {
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}
	return &codexClient{
		token:        token,
		accountID:    accountID,
		baseURL:      strings.TrimRight(baseURL, "/"),
		errorLabel:   "codex",
		providerName: "openai-codex",
		// The ChatGPT Codex backend load-sheds requests from client
		// identities it does not recognize: unknown originator/user-agent
		// pairs receive "Our servers are currently overloaded" stream
		// errors near-deterministically while Codex CLI requests succeed.
		// Send the Codex CLI request shape for every model on this
		// backend.
		cliRoutingAll: true,
		http:          &http.Client{Timeout: 0},
	}
}

func (c *codexClient) Name() string { return "openai-codex" }

func (c *codexClient) grammarProvider() string {
	if c.providerName != "" {
		return c.providerName
	}
	return c.Name()
}

// ---- Responses API wire types (subset needed for zot's surface) ----

type codexInputText struct {
	Type string `json:"type"` // "input_text"
	Text string `json:"text"`
}

type codexInputImage struct {
	Type     string `json:"type"` // "input_image"
	Detail   string `json:"detail"`
	ImageURL string `json:"image_url"`
}

type codexOutputText struct {
	Type        string `json:"type"` // "output_text"
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type codexInputMessage struct {
	Type    string `json:"type,omitempty"` // "message" (optional for input)
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type codexOutputMessage struct {
	Type    string            `json:"type"` // "message"
	Role    string            `json:"role"`
	Status  string            `json:"status,omitempty"`
	ID      string            `json:"id,omitempty"`
	Content []codexOutputText `json:"content"`
}

type codexFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

type codexCustomCall struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
}

type codexFunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"` // string (or ResponseFunctionCallOutputItemList for images; v1 only uses string)
}

// codexReasoningItem mirrors the Responses API "reasoning" output item.
// We capture it on incoming streams and replay it verbatim on follow-up
// requests: the API rejects assistant tool-call replays without it when
// thinking is enabled.
type codexReasoningItem struct {
	Type             string `json:"type"` // "reasoning"
	ID               string `json:"id,omitempty"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
	// Summary is required by the Responses API even when no summary text
	// was streamed; encode an empty array rather than omitting the field.
	Summary []codexReasoningSummary `json:"summary"`
}

type codexReasoningSummary struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

type codexTool struct {
	Type        string            `json:"type"` // "function"
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Parameters  json.RawMessage   `json:"parameters,omitempty"`
	Format      *customToolFormat `json:"format,omitempty"`
}

type codexReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}

type codexRequest struct {
	Model             string                `json:"model"`
	Store             bool                  `json:"store"`
	Stream            bool                  `json:"stream"`
	Instructions      string                `json:"instructions,omitempty"`
	Input             []any                 `json:"input"`
	Tools             []codexTool           `json:"tools,omitempty"`
	ToolChoice        string                `json:"tool_choice,omitempty"`
	ParallelToolCalls bool                  `json:"parallel_tool_calls"`
	Include           []string              `json:"include,omitempty"`
	Reasoning         *codexReasoningConfig `json:"reasoning,omitempty"`
	PromptCacheKey    string                `json:"prompt_cache_key,omitempty"`
}

// ---- Request building ----

func (c *codexClient) findModel(id string) (Model, error) {
	if c.providerName != "" && c.providerName != "openai-codex" {
		if m, err := FindModel(c.providerName, id); err == nil {
			return m, nil
		}
	}
	if m, err := FindModel("openai-codex", id); err == nil {
		return m, nil
	}
	return FindModel("openai", id)
}

func (c *codexClient) buildRequest(req Request) (*codexRequest, error) {
	m, err := c.findModel(req.Model)
	if err != nil {
		return nil, err
	}
	reasoning := ClampReasoningForModel(m, req.Reasoning)

	body := &codexRequest{
		Model:             req.Model,
		Store:             false,
		Stream:            true,
		Instructions:      req.System,
		ParallelToolCalls: true,
		Include:           []string{"reasoning.encrypted_content"},
	}
	if m.Reasoning {
		effort := OpenAICodexReasoningEffort(reasoning, req.Model)
		if hasReasoningLevelOverride(m, req.Reasoning) {
			effort = reasoning
		}
		if effort != "" {
			body.Reasoning = &codexReasoningConfig{Effort: effort}
		}
	}
	grammar := grammarTools(req.Tools, supportsGrammarTools(m, c.grammarProvider(), true))
	customCalls := map[string]bool{}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if call, ok := block.(ToolCallBlock); ok {
				if _, native := grammar[call.Name]; native {
					customCalls[call.ID] = true
				}
			}
		}
	}
	activeTools := activeToolDefinitions(req.Tools, req.Messages)
	if len(activeTools) > 0 {
		body.ToolChoice = "auto"
		for _, t := range activeTools {
			if _, native := grammar[t.Name]; native {
				body.Tools = append(body.Tools, codexTool{Type: "custom", Name: t.Name, Description: t.Description, Format: grammarFormat(t)})
				continue
			}
			params := t.Schema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			body.Tools = append(body.Tools, codexTool{
				Type:        "function",
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			})
		}
	}

	msgIdx := 0
	req.Messages = RepairOrphanedToolResults(req.Messages)
	for _, msg := range req.Messages {
		switch msg.Role {
		case RoleUser:
			content := []any{}
			for _, c := range msg.Content {
				switch v := c.(type) {
				case TextBlock:
					if v.Text != "" {
						content = append(content, codexInputText{Type: "input_text", Text: v.Text})
					}
				case ImageBlock:
					url := "data:" + v.MimeType + ";base64," + base64.StdEncoding.EncodeToString(v.Data)
					content = append(content, codexInputImage{Type: "input_image", Detail: "auto", ImageURL: url})
				}
			}
			if len(content) == 0 {
				continue
			}
			body.Input = append(body.Input, codexInputMessage{Role: "user", Content: content})
		case RoleAssistant:
			// Emit one output_message per text block, one function_call per
			// tool call, and one reasoning item per ReasoningBlock,
			// preserving the order so the model sees the same interleaving
			// we captured. The reasoning replay is what keeps OpenAI
			// Codex from rejecting follow-up tool calls with
			// "thinking is enabled but reasoning_content is missing".
			for _, c := range msg.Content {
				switch v := c.(type) {
				case ReasoningBlock:
					item := codexReasoningItem{
						Type:             "reasoning",
						ID:               v.ID,
						EncryptedContent: v.Encrypted,
						Summary:          []codexReasoningSummary{},
					}
					if v.Summary != "" {
						item.Summary = []codexReasoningSummary{{Type: "summary_text", Text: v.Summary}}
					}
					body.Input = append(body.Input, item)
				case TextBlock:
					if v.Text == "" {
						continue
					}
					msgIdx++
					body.Input = append(body.Input, codexOutputMessage{
						Type:   "message",
						Role:   "assistant",
						Status: "completed",
						ID:     fmt.Sprintf("msg_%d", msgIdx),
						Content: []codexOutputText{
							{Type: "output_text", Text: v.Text, Annotations: []any{}},
						},
					})
				case ToolCallBlock:
					if property, native := grammar[v.Name]; native {
						callID, _ := splitCallID(v.ID)
						body.Input = append(body.Input, codexCustomCall{Type: "custom_tool_call", CallID: callID, Name: v.Name, Input: customToolInput(v.Arguments, property)})
						continue
					}
					args := string(v.Arguments)
					if args == "" || !json.Valid([]byte(args)) {
						args = "{}"
					}
					callID, _ := splitCallID(v.ID)
					body.Input = append(body.Input, codexFunctionCall{
						Type:      "function_call",
						CallID:    callID,
						Name:      v.Name,
						Arguments: args,
					})
				}
			}
		case RoleTool:
			for _, c := range msg.Content {
				if tr, ok := c.(ToolResultBlock); ok {
					var text strings.Builder
					imageCount := 0
					for _, inner := range tr.Content {
						switch v := inner.(type) {
						case TextBlock:
							if text.Len() > 0 {
								text.WriteString("\n")
							}
							text.WriteString(v.Text)
						case ImageBlock:
							imageCount++
						}
					}
					// The Responses API function_call_output only carries a
					// string, so image bytes cannot ride along here. The agent
					// loop mirrors any tool-result images into the following
					// user message (which this client does serialize as
					// input_image). Leave a short text note so an image-only
					// result is not an empty output the API may reject, and so
					// the model knows the image arrives next.
					out := text.String()
					if out == "" && imageCount > 0 {
						if imageCount == 1 {
							out = "[image returned; see the following message]"
						} else {
							out = fmt.Sprintf("[%d images returned; see the following message]", imageCount)
						}
					}
					callID, _ := splitCallID(tr.CallID)
					outputType := "function_call_output"
					if customCalls[tr.CallID] {
						outputType = "custom_tool_call_output"
					}
					body.Input = append(body.Input, codexFunctionCallOutput{
						Type:   outputType,
						CallID: callID,
						Output: out,
					})
				}
			}
		}
	}

	return body, nil
}

func splitCallID(id string) (string, string) {
	if i := strings.Index(id, "|"); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

func usesCodexCLIRouting(model string) bool {
	switch model {
	case "gpt-5.6-luna", "gpt-5.6-luna-pro", "gpt-5.6-terra", "gpt-5.6-terra-pro":
		return true
	default:
		return false
	}
}

func newCodexSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("zot-%d", time.Now().UnixNano())
	}
	return strings.Join([]string{
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	}, "-")
}

// ---- Streaming ----

func (c *codexClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	if c.modelName != nil {
		wire.Model = c.modelName(wire.Model)
	}
	useCLIRouting := !c.disableCLIRouting && (c.cliRoutingAll || usesCodexCLIRouting(wire.Model))
	var codexSessionID string
	if useCLIRouting {
		codexSessionID = newCodexSessionID()
		wire.PromptCacheKey = codexSessionID
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	newReq := func(cliRouting bool) func() (*http.Request, error) {
		return func() (*http.Request, error) {
			httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("content-type", "application/json")
			httpReq.Header.Set("accept", "text/event-stream")
			httpReq.Header.Set("authorization", "Bearer "+c.token)
			httpReq.Header.Set("chatgpt-account-id", c.accountID)
			httpReq.Header.Set("openai-beta", "responses=experimental")
			if codexSessionID != "" {
				httpReq.Header.Set("session-id", codexSessionID)
				httpReq.Header.Set("x-client-request-id", codexSessionID)
			}
			if cliRouting {
				// The ChatGPT Codex backend normally requires the Codex CLI
				// request identity. A narrow fallback below uses zot's identity
				// when that compatibility shape selects unsupported cache policy.
				httpReq.Header.Set("originator", "codex_cli_rs")
				httpReq.Header.Set("user-agent", "codex_cli_rs/0.0.0")
			} else {
				httpReq.Header.Set("originator", "zot")
				httpReq.Header.Set("user-agent", fmt.Sprintf("zot (%s %s)", runtime.GOOS, runtime.GOARCH))
			}
			return httpReq, nil
		}
	}

	resp, err := doStreamWithRetry(ctx, c.http, newReq(useCLIRouting))
	if err != nil {
		return nil, fmt.Errorf("openai-codex: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		status := resp.StatusCode
		responseBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !useCLIRouting || !unsupportedPromptCacheRetention(status, responseBody) {
			return nil, fmt.Errorf("openai-codex: http %d: %s", status, strings.TrimSpace(string(responseBody)))
		}
		resp, err = doStreamWithRetry(ctx, c.http, newReq(false))
		if err != nil {
			return nil, fmt.Errorf("openai-codex: cache compatibility retry: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			status = resp.StatusCode
			responseBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("openai-codex: http %d: %s", status, strings.TrimSpace(string(responseBody)))
		}
	}

	out := make(chan Event, 16)
	go c.runStream(ctx, resp, req, out)
	return out, nil
}

func unsupportedPromptCacheRetention(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Error.Param != "prompt_cache_retention" {
		return false
	}
	message := strings.ToLower(payload.Error.Message)
	return payload.Error.Code == "invalid_parameter" || strings.Contains(message, "not supported")
}

func (c *codexClient) runStream(ctx context.Context, resp *http.Response, req Request, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	model, _ := c.findModel(req.Model)
	grammar := grammarTools(req.Tools, supportsGrammarTools(model, c.grammarProvider(), true))
	providerName := c.providerName
	if providerName == "" {
		providerName = "openai-codex"
	}
	out <- EventStart{Model: req.Model, Provider: providerName}

	raw := make(chan sseEvent, 16)
	go readSSE(resp.Body, raw)

	// Accumulators. The Responses API emits output_items in order; each
	// item is either a "message" (text) or a "function_call". We track
	// the in-flight item by its index.
	type itemState struct {
		kind      string // "message" | "function_call" | "reasoning"
		callID    string
		name      string
		argsBuf   strings.Builder
		custom    *customInputBuffer
		textBuf   strings.Builder
		summary   strings.Builder
		rawID     string
		encrypted string
		announced bool
	}
	var (
		items    = map[int]*itemState{}
		order    []int
		usage    Usage
		stop     StopReason = StopEnd
		finalErr error
	)

	assemble := func() Message {
		content := []Content{}
		for _, idx := range order {
			it := items[idx]
			switch it.kind {
			case "message":
				if it.textBuf.Len() > 0 {
					content = append(content, TextBlock{Text: it.textBuf.String()})
				}
			case "function_call", "custom_tool_call":
				args := it.argsBuf.String()
				if it.custom != nil {
					args = string(it.custom.arguments())
				}
				if args == "" || !json.Valid([]byte(args)) {
					args = "{}"
				}
				content = append(content, ToolCallBlock{
					ID: it.callID, Name: it.name, Arguments: json.RawMessage(args),
				})
			case "reasoning":
				if it.encrypted == "" && it.summary.Len() == 0 && it.rawID == "" {
					continue
				}
				content = append(content, ReasoningBlock{
					ID:        it.rawID,
					Summary:   it.summary.String(),
					Encrypted: it.encrypted,
				})
			}
		}
		return Message{Role: RoleAssistant, Content: content, Time: time.Now()}
	}

	sendDone := func() {
		usage.CostUSD = ComputeCost(model, usage)
		out <- EventUsage{Usage: usage}
		out <- EventDone{Stop: stop, Err: finalErr, Message: assemble()}
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
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &head); err != nil {
				continue
			}

			switch head.Type {
			case "response.output_item.added":
				var p struct {
					OutputIndex int `json:"output_index"`
					Item        struct {
						Type             string `json:"type"` // "message" | "function_call" | "reasoning"
						ID               string `json:"id"`
						CallID           string `json:"call_id"`
						Name             string `json:"name"`
						EncryptedContent string `json:"encrypted_content"`
					} `json:"item"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				it := &itemState{}
				switch p.Item.Type {
				case "message":
					it.kind = "message"
				case "function_call", "custom_tool_call":
					it.kind = p.Item.Type
					if it.kind == "custom_tool_call" {
						property := grammar[p.Item.Name]
						if property == "" {
							property = "input"
						}
						it.custom = &customInputBuffer{property: property}
					}
					it.callID = p.Item.CallID
					it.name = p.Item.Name
					if !it.announced {
						it.announced = true
						out <- EventToolStart{ID: it.callID, Name: it.name}
					}
				case "reasoning":
					it.kind = "reasoning"
					it.rawID = p.Item.ID
					it.encrypted = p.Item.EncryptedContent
				default:
					continue
				}
				items[p.OutputIndex] = it
				order = append(order, p.OutputIndex)
			case "response.output_text.delta":
				var p struct {
					OutputIndex int    `json:"output_index"`
					Delta       string `json:"delta"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				if it, ok := items[p.OutputIndex]; ok && it.kind == "message" {
					it.textBuf.WriteString(p.Delta)
					out <- EventTextDelta{Delta: p.Delta}
				}
			case "response.reasoning_summary_text.delta":
				var p struct {
					OutputIndex int    `json:"output_index"`
					Delta       string `json:"delta"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				if it, ok := items[p.OutputIndex]; ok && it.kind == "reasoning" {
					it.summary.WriteString(p.Delta)
				}
			case "response.reasoning_summary_text.done":
				// summary text already accumulated via deltas
			case "response.function_call_arguments.delta":
				var p struct {
					OutputIndex int    `json:"output_index"`
					Delta       string `json:"delta"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				if it, ok := items[p.OutputIndex]; ok && it.kind == "function_call" {
					it.argsBuf.WriteString(p.Delta)
					out <- EventToolArgs{ID: it.callID, Delta: p.Delta}
				}
			case "response.custom_tool_call_input.delta":
				var p struct {
					OutputIndex int    `json:"output_index"`
					Delta       string `json:"delta"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				if it := items[p.OutputIndex]; it != nil && it.custom != nil {
					if delta := it.custom.append(p.Delta, false); delta != "" {
						out <- EventToolArgs{ID: it.callID, Delta: delta}
					}
				}
			case "response.custom_tool_call_input.done":
				var p struct {
					OutputIndex int     `json:"output_index"`
					Input       *string `json:"input"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				if it := items[p.OutputIndex]; it != nil && it.custom != nil {
					if delta := it.custom.finish(p.Input); delta != "" {
						out <- EventToolArgs{ID: it.callID, Delta: delta}
					}
				}
			case "response.output_item.done":
				var p struct {
					OutputIndex int `json:"output_index"`
					Item        struct {
						Type             string  `json:"type"`
						ID               string  `json:"id"`
						Input            *string `json:"input"`
						EncryptedContent string  `json:"encrypted_content"`
						Summary          []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"summary"`
					} `json:"item"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				if it, ok := items[p.OutputIndex]; ok {
					switch it.kind {
					case "function_call", "custom_tool_call":
						if it.custom != nil {
							if delta := it.custom.finish(p.Item.Input); delta != "" {
								out <- EventToolArgs{ID: it.callID, Delta: delta}
							}
						}
						out <- EventToolEnd{ID: it.callID}
					case "reasoning":
						if p.Item.EncryptedContent != "" {
							it.encrypted = p.Item.EncryptedContent
						}
						if it.rawID == "" && p.Item.ID != "" {
							it.rawID = p.Item.ID
						}
						for _, s := range p.Item.Summary {
							if s.Text == "" {
								continue
							}
							if it.summary.Len() > 0 {
								it.summary.WriteString("\n")
							}
							it.summary.WriteString(s.Text)
						}
					}
				}
			case "response.completed", "response.done":
				var p struct {
					Response struct {
						Usage struct {
							InputTokens        int `json:"input_tokens"`
							OutputTokens       int `json:"output_tokens"`
							InputTokensDetails struct {
								CachedTokens int `json:"cached_tokens"`
							} `json:"input_tokens_details"`
							OutputTokensDetails *struct {
								ReasoningTokens int `json:"reasoning_tokens"`
							} `json:"output_tokens_details"`
						} `json:"usage"`
						Status string `json:"status"`
					} `json:"response"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				usage.InputTokens = p.Response.Usage.InputTokens - p.Response.Usage.InputTokensDetails.CachedTokens
				if usage.InputTokens < 0 {
					usage.InputTokens = p.Response.Usage.InputTokens
				}
				usage.OutputTokens = p.Response.Usage.OutputTokens
				usage.CacheReadTokens = p.Response.Usage.InputTokensDetails.CachedTokens
				if details := p.Response.Usage.OutputTokensDetails; details != nil {
					usage.ReasoningTokens = details.ReasoningTokens
					usage.ReasoningTokensKnown = true
				}

				hadTool := false
				for _, it := range items {
					if it.kind == "function_call" || it.kind == "custom_tool_call" {
						hadTool = true
						break
					}
				}
				if hadTool {
					stop = StopToolUse
				} else {
					stop = StopEnd
				}
				sendDone()
				return
			case "response.failed":
				var p struct {
					Response struct {
						Error struct {
							Message string `json:"message"`
						} `json:"error"`
					} `json:"response"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				stop = StopError
				finalErr = fmt.Errorf("codex: %s", p.Response.Error.Message)
				sendDone()
				return
			case "error":
				var p struct {
					Message string `json:"message"`
					Code    string `json:"code"`
					Error   struct {
						Message string `json:"message"`
						Code    string `json:"code"`
					} `json:"error"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				msg := p.Message
				if msg == "" {
					msg = p.Error.Message
				}
				if msg == "" {
					msg = p.Code
				}
				if msg == "" {
					msg = p.Error.Code
				}
				if msg == "" {
					msg = strings.TrimSpace(ev.Data)
				}
				stop = StopError
				label := c.errorLabel
				if label == "" {
					label = "codex"
				}
				finalErr = fmt.Errorf("%s error: %s", label, msg)
				sendDone()
				return
			}
		}
	}
}
