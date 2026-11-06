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
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Google Gemini provider, talking directly to the Generative Language
// REST API at generativelanguage.googleapis.com. We hand-roll the wire
// format instead of pulling in @google/genai (the SDK is large and we
// only need streamGenerateContent + the models list).
//
// Auth model: API key only. Google does NOT issue OAuth tokens for
// consumer Gemini Advanced / Google One AI subscriptions; programmatic
// access requires either an AI Studio API key (this client) or Vertex
// AI / GCP service-account credentials (separate provider, not yet
// implemented in zot).

const geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"

// geminiClient implements Client against the Gemini Generative Language API.
type geminiClient struct {
	providerName string
	apiKey       string
	baseURL      string
	http         *http.Client
}

// NewGemini creates a Gemini client using an AI Studio API key.
// baseURL may be empty; defaults to https://generativelanguage.googleapis.com.
func NewGemini(apiKey, baseURL string) Client {
	return NewGeminiNamed("google", apiKey, baseURL)
}

// NewGeminiNamed creates a Gemini-compatible client with its own provider identity.
func NewGeminiNamed(providerName, apiKey, baseURL string) Client {
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	return &geminiClient{
		providerName: providerName,
		apiKey:       apiKey,
		baseURL:      strings.TrimRight(baseURL, "/"),
		http:         &http.Client{Timeout: 0},
	}
}

func (c *geminiClient) Name() string {
	if c.providerName == "" {
		return "google"
	}
	return c.providerName
}

// ---- wire types ----
//
// Subset of Gemini's Content / Part / GenerateContentRequest schema.
// Only the fields zot actually emits or consumes are declared here.

type gemInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type gemFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gemFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// gemPart is one element of a Content. Exactly one of the optional
// fields is populated per part. We use pointers so empty values are
// omitted from the wire, which matters for tool responses (Gemini
// rejects parts with both `text` and `functionResponse` set).
type gemPart struct {
	Text             string               `json:"text,omitempty"`
	InlineData       *gemInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *gemFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *gemFunctionResponse `json:"functionResponse,omitempty"`
	// Thought marks a thought-summary part. ThoughtSignature is opaque
	// provider state that must be replayed on the same part.
	Thought          bool   `json:"thought,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type gemContent struct {
	Role  string    `json:"role"` // "user" | "model"
	Parts []gemPart `json:"parts"`
}

type gemFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type gemTool struct {
	FunctionDeclarations []gemFunctionDecl `json:"functionDeclarations,omitempty"`
}

type gemThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

type gemGenerationConfig struct {
	Temperature     *float32           `json:"temperature,omitempty"`
	MaxOutputTokens *int               `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *gemThinkingConfig `json:"thinkingConfig,omitempty"`
}

type gemSystemInstruction struct {
	Parts []gemPart `json:"parts"`
}

type gemRequest struct {
	Contents          []gemContent          `json:"contents"`
	SystemInstruction *gemSystemInstruction `json:"systemInstruction,omitempty"`
	Tools             []gemTool             `json:"tools,omitempty"`
	GenerationConfig  *gemGenerationConfig  `json:"generationConfig,omitempty"`
}

// ---- request building ----

func (c *geminiClient) buildRequest(req Request) (*gemRequest, string, error) {
	m, err := FindModel(c.Name(), req.Model)
	if err != nil {
		// Not in the catalog — still allow custom ids by falling back
		// to defaults so users can point at unreleased models or
		// alternate base URLs.
		m = Model{
			Provider:      c.Name(),
			API:           "google",
			ID:            req.Model,
			ContextWindow: 1_000_000,
			MaxOutput:     8192,
			Reasoning:     strings.Contains(req.Model, "2.5") || strings.Contains(req.Model, "3"),
		}
	}

	reasoning := ClampReasoningForModel(m, req.Reasoning)
	out := &gemRequest{}

	// System prompt → systemInstruction.parts[0].text.
	if strings.TrimSpace(req.System) != "" {
		out.SystemInstruction = &gemSystemInstruction{
			Parts: []gemPart{{Text: req.System}},
		}
	}

	functionsEnabled := geminiSupportsFunctionCalling(m.ID)
	activeTools := activeToolDefinitions(req.Tools, req.Messages)

	// Convert tool defs. Gemini image-generation models reject function
	// declarations with "Function calling is not enabled for this model";
	// for those models, send a direct multimodal prompt instead.
	if functionsEnabled && len(activeTools) > 0 {
		decls := make([]gemFunctionDecl, 0, len(activeTools))
		for _, t := range activeTools {
			schema := sanitizeGeminiToolSchema(t.Schema)
			decls = append(decls, gemFunctionDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			})
		}
		out.Tools = []gemTool{{FunctionDeclarations: decls}}
	}

	// Generation config.
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = m.MaxOutput
	}
	gc := &gemGenerationConfig{Temperature: req.Temperature}
	if maxTok > 0 {
		gc.MaxOutputTokens = &maxTok
	}
	if reasoning != "" && m.Reasoning {
		tc := geminiThinkingConfig(m.ID, reasoning)
		if tc != nil {
			gc.ThinkingConfig = tc
		}
	}
	out.GenerationConfig = gc

	// Convert messages. When function calling is disabled for the target
	// model, also remove historical functionCall/functionResponse parts;
	// image models should receive only text/image content.
	msgs := req.Messages
	if functionsEnabled {
		msgs = RepairOrphanedToolResults(req.Messages)
	}
	for _, msg := range msgs {
		switch msg.Role {
		case RoleUser:
			parts := convertGemUserParts(msg.Content)
			if len(parts) == 0 {
				continue
			}
			out.Contents = append(out.Contents, gemContent{Role: "user", Parts: parts})
		case RoleAssistant:
			parts := convertGemAssistantParts(msg.Content, functionsEnabled)
			// Gemini 3 requires a thought signature on client-injected function
			// calls. Google's documented placeholder opts out of validation
			// for calls that were not generated by Gemini. Do not alter genuine
			// model responses or replace signatures obtained from the API.
			if strings.HasPrefix(strings.ToLower(m.ID), "gemini-3") && msg.Meta["synthetic_tool_call"] == "true" {
				for i := range parts {
					if parts[i].FunctionCall != nil && parts[i].ThoughtSignature == "" {
						parts[i].ThoughtSignature = "skip_thought_signature_validator"
					}
				}
			}
			if len(parts) == 0 {
				continue
			}
			out.Contents = append(out.Contents, gemContent{Role: "model", Parts: parts})
		case RoleTool:
			if !functionsEnabled {
				continue
			}
			// Each tool_result becomes a user-role message with a
			// functionResponse part. Gemini's protocol uses
			// "user" role for tool replies.
			parts := convertGemToolResultParts(msg.Content)
			if len(parts) == 0 {
				continue
			}
			out.Contents = append(out.Contents, gemContent{Role: "user", Parts: parts})
		}
	}

	return out, m.ID, nil
}

func sanitizeGeminiToolSchema(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || !json.Valid(schema) {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	v = stripGeminiUnsupportedSchemaFields(v)
	out, err := json.Marshal(v)
	if err != nil || len(out) == 0 || !json.Valid(out) {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return out
}

func stripGeminiUnsupportedSchemaFields(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			switch k {
			case "additionalProperties", "$schema":
				continue
			default:
				out[k] = stripGeminiUnsupportedSchemaFields(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = stripGeminiUnsupportedSchemaFields(val)
		}
		return out
	default:
		return v
	}
}

func convertGemUserParts(blocks []Content) []gemPart {
	var parts []gemPart
	for _, b := range blocks {
		switch v := b.(type) {
		case TextBlock:
			if v.Text == "" {
				continue
			}
			parts = append(parts, gemPart{Text: v.Text})
		case ImageBlock:
			parts = append(parts, gemPart{
				InlineData: &gemInlineData{
					MimeType: v.MimeType,
					Data:     base64.StdEncoding.EncodeToString(v.Data),
				},
			})
		}
	}
	return parts
}

func convertGemAssistantParts(blocks []Content, functionsEnabled bool) []gemPart {
	var parts []gemPart
	for _, b := range blocks {
		switch v := b.(type) {
		case ReasoningBlock:
			// Foreign protocol signatures cannot be replayed as Gemini thoughts.
			if v.ID == miniMaxThinkingID || v.ID == bedrockReasoningID || v.ID == bedrockRedactedReasoningID {
				continue
			}
			if v.Encrypted != "" || v.Summary != "" {
				parts = append(parts, gemPart{
					Text:             v.Summary,
					Thought:          true,
					ThoughtSignature: v.Encrypted,
				})
			}
		case TextBlock:
			if strings.TrimSpace(v.Text) == "" {
				continue
			}
			parts = append(parts, gemPart{Text: v.Text, ThoughtSignature: v.ThoughtSignature})
		case ImageBlock:
			parts = append(parts, gemPart{
				InlineData: &gemInlineData{
					MimeType: v.MimeType,
					Data:     base64.StdEncoding.EncodeToString(v.Data),
				},
				ThoughtSignature: v.ThoughtSignature,
			})
		case ToolCallBlock:
			if !functionsEnabled {
				continue
			}
			args := v.Arguments
			if len(args) == 0 || !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			parts = append(parts, gemPart{
				FunctionCall: &gemFunctionCall{
					Name: v.Name,
					Args: args,
				},
				ThoughtSignature: v.ThoughtSignature,
			})
		}
	}
	return parts
}

func geminiSupportsFunctionCalling(modelID string) bool {
	id := strings.ToLower(modelID)
	// Gemini image generation/editing models accept direct multimodal
	// prompts but reject tools/function declarations.
	if strings.Contains(id, "flash-image") || strings.Contains(id, "image-generation") {
		return false
	}
	return true
}

func saveGeminiImageToWorkingDir(mimeType string, data []byte) (string, error) {
	ext := ".png"
	switch strings.ToLower(mimeType) {
	case "image/jpeg", "image/jpg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	name := "zot-gemini-image-" + uuid.NewString() + ext
	path := filepath.Join(".", name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func convertGemToolResultParts(blocks []Content) []gemPart {
	var parts []gemPart
	for _, b := range blocks {
		tr, ok := b.(ToolResultBlock)
		if !ok {
			continue
		}
		// Flatten text content. Image content in tool results is dropped
		// for Gemini < 3 (multimodal function responses are 3+); for the
		// common path (text output) we wrap as {"output": "..."} or
		// {"error": "..."} per the SDK's convention.
		var sb strings.Builder
		for _, c := range tr.Content {
			if tb, ok := c.(TextBlock); ok {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(tb.Text)
			}
		}
		key := "output"
		if tr.IsError {
			key = "error"
		}
		// Gemini wants response to be an object, not a string.
		respObj := map[string]string{key: sb.String()}
		respBytes, err := json.Marshal(respObj)
		if err != nil {
			respBytes = []byte(`{"output":""}`)
		}
		// Tool name is required on functionResponse; the original
		// ToolCallBlock has it but ToolResultBlock only carries the
		// call id. We thread the name back via the call id by using
		// the id as the name fallback — Gemini ignores the name on
		// the response side as long as it's non-empty.
		name := tr.CallID
		parts = append(parts, gemPart{
			FunctionResponse: &gemFunctionResponse{
				Name:     name,
				Response: respBytes,
			},
		})
	}
	return parts
}

// geminiThinkingConfig maps zot's reasoning level ("low"/"medium"/"high")
// to Gemini's thinkingConfig. The right knob depends on the model
// generation: 2.5 family uses thinkingBudget (tokens), 3.x uses
// thinkingLevel (enum). Returns nil when the level is unrecognised.
func geminiThinkingConfig(modelID, level string) *gemThinkingConfig {
	level = NormalizeReasoning(level)
	id := strings.ToLower(modelID)

	// Gemini 3.x: enum-based thinkingLevel. Pro can't go below LOW.
	if strings.Contains(id, "gemini-3") {
		isPro := strings.Contains(id, "-pro")
		var lvl string
		switch level {
		case "minimum":
			if isPro {
				lvl = "LOW"
			} else {
				lvl = "MINIMAL"
			}
		case "low":
			lvl = "LOW"
		case "medium":
			if isPro {
				lvl = "HIGH"
			} else {
				lvl = "MEDIUM"
			}
		case "high", "xhigh", "max":
			lvl = "HIGH"
		default:
			return nil
		}
		return &gemThinkingConfig{IncludeThoughts: true, ThinkingLevel: lvl}
	}

	// Gemini 2.5 family: token-budget per-model.
	budget := ReasoningBudget(level)
	switch {
	case strings.Contains(id, "2.5-pro"):
		if budget > 32768 {
			budget = 32768
		}
	case strings.Contains(id, "2.5-flash-lite"):
		if budget > 24576 {
			budget = 24576
		}
	case strings.Contains(id, "2.5-flash"):
		if budget > 24576 {
			budget = 24576
		}
	default:
		return nil
	}
	if budget <= 0 {
		return nil
	}
	return &gemThinkingConfig{IncludeThoughts: true, ThinkingBudget: &budget}
}

// ---- streaming ----

func (c *geminiClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	wire, modelID, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	// Gemini's streaming endpoint is a POST with ?alt=sse to get
	// an EventSource-compatible response. Without alt=sse the
	// server returns a JSON array (one element per chunk), which
	// we'd need a different parser for.
	apiPath := fmt.Sprintf("/v1beta/models/%s:streamGenerateContent", modelID)
	url := c.baseURL + apiPath + "?alt=sse"

	newReq := func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("accept", "text/event-stream")
		// Two ways to authenticate against the Generative Language
		// API: x-goog-api-key header or ?key= query param. We use
		// the header so the key never lands in proxy access logs.
		if c.apiKey != "" {
			httpReq.Header.Set("x-goog-api-key", c.apiKey)
		}
		return httpReq, nil
	}

	resp, err := doStreamWithRetry(ctx, c.http, newReq)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.Name(), err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("%s: http %d: %s", c.Name(), resp.StatusCode, strings.TrimSpace(string(b)))
	}

	out := make(chan Event, 16)
	go c.runStream(ctx, resp, req, out)
	return out, nil
}

func (c *geminiClient) runStream(ctx context.Context, resp *http.Response, req Request, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	model, _ := FindModel(c.Name(), req.Model)
	out <- EventStart{Model: req.Model, Provider: c.Name()}

	raw := make(chan sseEvent, 16)
	go readSSE(resp.Body, raw)

	// Gemini's SSE stream is a sequence of complete JSON
	// GenerateContentResponse objects, one per data: line. Each
	// candidate carries a list of parts (text or functionCall),
	// possibly accumulating across chunks.
	type blockEntry struct {
		kind             string // "text" | "image" | "tool_use" | "reasoning"
		textBuf          strings.Builder
		thoughtSignature string
		image            *ImageBlock
		imagePath        string
		toolID           string
		toolName         string
		toolArgs         strings.Builder
	}
	var (
		blocks           []*blockEntry
		currentText      *blockEntry
		currentReasoning *blockEntry
		usage            Usage
		stop             StopReason = StopEnd
		finalErr         error
		toolCounter      int
	)

	appendPartText := func(kind, delta, signature string) {
		current := &currentText
		if kind == "reasoning" {
			current = &currentReasoning
		}
		if *current == nil {
			*current = &blockEntry{kind: kind}
			blocks = append(blocks, *current)
		}
		(*current).textBuf.WriteString(delta)
		if signature != "" {
			(*current).thoughtSignature = signature
			// A signature terminates this wire part. A later delta of the
			// same kind belongs to a new part and must not be merged into it.
			*current = nil
		}
	}

	appendImage := func(mimeType, dataB64, signature string) {
		data, err := base64.StdEncoding.DecodeString(dataB64)
		if err != nil || len(data) == 0 {
			return
		}
		img := ImageBlock{MimeType: mimeType, Data: data, ThoughtSignature: signature}
		path, _ := saveGeminiImageToWorkingDir(mimeType, data)
		blocks = append(blocks, &blockEntry{kind: "image", image: &img, imagePath: path})
		// Non-text parts break both streamed text runs.
		currentText = nil
		currentReasoning = nil
	}

	startTool := func(name string, providedID string, args json.RawMessage) *blockEntry {
		toolCounter++
		id := providedID
		if id == "" {
			id = fmt.Sprintf("%s_%d_%d", name, time.Now().UnixNano(), toolCounter)
		}
		t := &blockEntry{
			kind:     "tool_use",
			toolID:   id,
			toolName: name,
		}
		if len(args) > 0 && json.Valid(args) {
			t.toolArgs.Write(args)
		}
		blocks = append(blocks, t)
		// New tool blocks break both streamed text runs.
		currentText = nil
		currentReasoning = nil
		return t
	}

	assembleMsg := func() Message {
		content := []Content{}
		for _, b := range blocks {
			switch b.kind {
			case "text":
				if b.textBuf.Len() > 0 {
					content = append(content, TextBlock{
						Text:             b.textBuf.String(),
						ThoughtSignature: b.thoughtSignature,
					})
				}
			case "reasoning":
				if b.textBuf.Len() > 0 || b.thoughtSignature != "" {
					content = append(content, ReasoningBlock{
						Summary:   b.textBuf.String(),
						Encrypted: b.thoughtSignature,
					})
				}
			case "image":
				if b.image != nil && len(b.image.Data) > 0 {
					content = append(content, *b.image)
					if b.imagePath != "" {
						content = append(content, TextBlock{Text: fmt.Sprintf("Saved image: `%s`", b.imagePath)})
					}
				}
			case "tool_use":
				args := b.toolArgs.String()
				if args == "" || !json.Valid([]byte(args)) {
					args = "{}"
				}
				content = append(content, ToolCallBlock{
					ID:               b.toolID,
					Name:             b.toolName,
					Arguments:        json.RawMessage(args),
					ThoughtSignature: b.thoughtSignature,
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
			if strings.TrimSpace(ev.Data) == "" {
				continue
			}
			var chunk struct {
				Candidates []struct {
					Content struct {
						Role  string `json:"role"`
						Parts []struct {
							Text             string           `json:"text"`
							InlineData       *gemInlineData   `json:"inlineData"`
							Thought          bool             `json:"thought"`
							ThoughtSignature string           `json:"thoughtSignature"`
							FunctionCall     *gemFunctionCall `json:"functionCall"`
							FunctionCallID   string           `json:"id"`
							FunctionCallName string           `json:"name"`
						} `json:"parts"`
					} `json:"content"`
					FinishReason string `json:"finishReason"`
				} `json:"candidates"`
				UsageMetadata *struct {
					PromptTokenCount        int  `json:"promptTokenCount"`
					CandidatesTokenCount    int  `json:"candidatesTokenCount"`
					ThoughtsTokenCount      *int `json:"thoughtsTokenCount"`
					CachedContentTokenCount int  `json:"cachedContentTokenCount"`
					TotalTokenCount         int  `json:"totalTokenCount"`
				} `json:"usageMetadata"`
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Status  string `json:"status"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				continue
			}
			if chunk.Error != nil {
				stop = StopError
				finalErr = fmt.Errorf("%s: %s", c.Name(), chunk.Error.Message)
				sendDone()
				return
			}
			for _, cand := range chunk.Candidates {
				for _, part := range cand.Content.Parts {
					if part.InlineData != nil {
						appendImage(part.InlineData.MimeType, part.InlineData.Data, part.ThoughtSignature)
						continue
					}
					if part.FunctionCall != nil {
						var args json.RawMessage
						if len(part.FunctionCall.Args) > 0 {
							args = part.FunctionCall.Args
						} else {
							args = json.RawMessage("{}")
						}
						t := startTool(part.FunctionCall.Name, part.FunctionCallID, args)
						t.thoughtSignature = part.ThoughtSignature
						out <- EventToolStart{ID: t.toolID, Name: t.toolName}
						out <- EventToolArgs{ID: t.toolID, Delta: t.toolArgs.String()}
						out <- EventToolEnd{ID: t.toolID}
						continue
					}
					if part.Thought {
						appendPartText("reasoning", part.Text, part.ThoughtSignature)
						if part.Text != "" {
							out <- EventReasoningDelta{Delta: part.Text}
						}
						currentText = nil
						continue
					}
					if part.Text == "" {
						continue
					}
					appendPartText("text", part.Text, part.ThoughtSignature)
					currentReasoning = nil
					out <- EventTextDelta{Delta: part.Text}
				}
				switch cand.FinishReason {
				case "STOP", "":
					// "" arrives on intermediate chunks; only
					// promote the explicit terminal value.
					if cand.FinishReason == "STOP" {
						stop = StopEnd
					}
				case "MAX_TOKENS":
					stop = StopLength
				case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
					stop = StopError
					finalErr = fmt.Errorf("%s: response blocked (%s)", c.Name(), cand.FinishReason)
				}
			}
			if chunk.UsageMetadata != nil {
				// Gemini reports cumulative totals on every chunk
				// (or close to it), so assign rather than sum.
				um := chunk.UsageMetadata
				input := um.PromptTokenCount - um.CachedContentTokenCount
				if input < 0 {
					input = um.PromptTokenCount
				}
				usage.InputTokens = input
				usage.OutputTokens = um.CandidatesTokenCount
				if um.ThoughtsTokenCount != nil {
					usage.ReasoningTokens = *um.ThoughtsTokenCount
					usage.ReasoningTokensKnown = true
					usage.OutputTokens += *um.ThoughtsTokenCount
				}
				usage.CacheReadTokens = um.CachedContentTokenCount
			}
			// Promote ToolUse stop when tool calls are present and
			// the candidate finished cleanly.
			if stop == StopEnd {
				for _, b := range blocks {
					if b.kind == "tool_use" {
						stop = StopToolUse
						break
					}
				}
			}
		}
	}
}
