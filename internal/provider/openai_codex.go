package provider

import (
	"bytes"
	"context"
	"encoding/base64"
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
	token     string
	accountID string
	baseURL   string
	http      *http.Client
}

// NewOpenAICodex creates a client that talks to ChatGPT's Codex endpoint
// using a subscription OAuth access token and the user's ChatGPT
// account id. baseURL may be empty to use the default.
func NewOpenAICodex(token, accountID, baseURL string) Client {
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}
	return &codexClient{
		token:     token,
		accountID: accountID,
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      &http.Client{Timeout: 0},
	}
}

func (c *codexClient) Name() string { return "openai" }

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
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type codexRequest struct {
	Model             string      `json:"model"`
	Store             bool        `json:"store"`
	Stream            bool        `json:"stream"`
	Instructions      string      `json:"instructions,omitempty"`
	Input             []any       `json:"input"`
	Tools             []codexTool `json:"tools,omitempty"`
	ToolChoice        string      `json:"tool_choice,omitempty"`
	ParallelToolCalls bool        `json:"parallel_tool_calls"`
	Include           []string    `json:"include,omitempty"`
}

// ---- Request building ----

func (c *codexClient) buildRequest(req Request) (*codexRequest, error) {
	m, err := FindModel("openai", req.Model)
	if err != nil {
		return nil, err
	}
	_ = m

	body := &codexRequest{
		Model:             req.Model,
		Store:             false,
		Stream:            true,
		Instructions:      req.System,
		ParallelToolCalls: true,
		Include:           []string{"reasoning.encrypted_content"},
	}
	if len(req.Tools) > 0 {
		body.ToolChoice = "auto"
		for _, t := range req.Tools {
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
					for _, inner := range tr.Content {
						if tb, ok := inner.(TextBlock); ok {
							if text.Len() > 0 {
								text.WriteString("\n")
							}
							text.WriteString(tb.Text)
						}
					}
					callID, _ := splitCallID(tr.CallID)
					body.Input = append(body.Input, codexFunctionCallOutput{
						Type:   "function_call_output",
						CallID: callID,
						Output: text.String(),
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

// ---- Streaming ----

func (c *codexClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	httpReq.Header.Set("authorization", "Bearer "+c.token)
	httpReq.Header.Set("chatgpt-account-id", c.accountID)
	httpReq.Header.Set("openai-beta", "responses=experimental")
	httpReq.Header.Set("originator", "zot")
	httpReq.Header.Set("user-agent", fmt.Sprintf("zot (%s %s)", runtime.GOOS, runtime.GOARCH))

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-codex: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai-codex: http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	out := make(chan Event, 16)
	go c.runStream(ctx, resp, req, out)
	return out, nil
}

func (c *codexClient) runStream(ctx context.Context, resp *http.Response, req Request, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	model, _ := FindModel("openai", req.Model)
	out <- EventStart{Model: req.Model, Provider: "openai"}

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
			case "function_call":
				args := it.argsBuf.String()
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
				case "function_call":
					it.kind = "function_call"
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
			case "response.output_item.done":
				var p struct {
					OutputIndex int `json:"output_index"`
					Item        struct {
						Type             string `json:"type"`
						ID               string `json:"id"`
						EncryptedContent string `json:"encrypted_content"`
						Summary          []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"summary"`
					} `json:"item"`
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				if it, ok := items[p.OutputIndex]; ok {
					switch it.kind {
					case "function_call":
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

				hadTool := false
				for _, it := range items {
					if it.kind == "function_call" {
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
				}
				_ = json.Unmarshal([]byte(ev.Data), &p)
				msg := p.Message
				if msg == "" {
					msg = p.Code
				}
				stop = StopError
				finalErr = fmt.Errorf("codex error: %s", msg)
				sendDone()
				return
			}
		}
	}
}
