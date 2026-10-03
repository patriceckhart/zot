package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type ClassifierQuestion struct {
	Type         string          `json:"type"`
	Instructions *string         `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

type ClassifierContext struct {
	State         map[string]json.RawMessage    `json:"state"`
	Questions     map[string]ClassifierQuestion `json:"questions"`
	questionOrder []string
	stateJSON     json.RawMessage
}

func ParseClassifierContext(raw json.RawMessage) (ClassifierContext, error) {
	var context ClassifierContext
	if err := json.Unmarshal(raw, &context); err != nil || context.State == nil || len(context.Questions) == 0 {
		return context, fmt.Errorf("classify expects {state: object, questions: {id: question}}")
	}
	for _, q := range context.Questions {
		if q.Instructions == nil {
			return context, fmt.Errorf("classifier instructions must be a string")
		}
		switch q.Type {
		case "choice", "bool":
			var criteria map[string]*string
			if err := json.Unmarshal(q.Criteria, &criteria); err != nil || len(criteria) == 0 {
				return context, fmt.Errorf("classifier criteria must map labels to strings")
			}
			for _, description := range criteria {
				if description == nil {
					return context, fmt.Errorf("classifier criteria must map labels to strings")
				}
			}
			if q.Type == "bool" {
				if _, ok := criteria["true"]; !ok {
					return context, fmt.Errorf("bool criteria must contain true and false")
				}
				if _, ok := criteria["false"]; !ok {
					return context, fmt.Errorf("bool criteria must contain true and false")
				}
			}
		case "score":
			var criteria []*string
			if err := json.Unmarshal(q.Criteria, &criteria); err != nil || len(criteria) == 0 {
				return context, fmt.Errorf("score criteria must list levels as strings")
			}
			for _, description := range criteria {
				if description == nil {
					return context, fmt.Errorf("score criteria must list levels as strings")
				}
			}
		default:
			return context, fmt.Errorf("classifier question type must be choice, score or bool")
		}
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	context.questionOrder = jsonObjectKeys(fields["questions"])
	context.stateJSON = fields["state"]
	return context, nil
}

type ModelBlock struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	Data     *string `json:"data,omitempty"`
	MimeType *string `json:"mimeType,omitempty"`
}

type ImagesContext struct {
	Input []ModelBlock `json:"input"`
}

func ParseImagesContext(raw json.RawMessage) (ImagesContext, error) {
	var context ImagesContext
	if err := json.Unmarshal(raw, &context); err != nil || len(context.Input) == 0 {
		return context, fmt.Errorf("generateImages expects {input: [text or image blocks]}")
	}
	for _, block := range context.Input {
		if block.Type == "text" && block.Text != nil {
			continue
		}
		if block.Type == "image" && block.Data != nil && block.MimeType != nil && strings.HasPrefix(*block.MimeType, "image/") {
			if data, err := base64.StdEncoding.DecodeString(*block.Data); err == nil && len(data) > 0 {
				continue
			}
		}
		return context, fmt.Errorf("image input must contain valid text or base64 image blocks")
	}
	return context, nil
}

// NonChatClient handles inference wire protocols, never agent or VM state.
type NonChatClient struct{ HTTP *http.Client }

func (c NonChatClient) post(ctx context.Context, endpoint, key string, payload any) (map[string]json.RawMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("invalid model request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid model endpoint")
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" && key != "local" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := doStreamWithRetry(ctx, client, func() (*http.Request, error) {
		retry := request.Clone(ctx)
		retry.Body = io.NopCloser(bytes.NewReader(data))
		return retry, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("model request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("model service returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024+1))
	if err != nil || len(body) > 64*1024*1024 {
		return nil, fmt.Errorf("invalid or oversized model response")
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(body, &result); err != nil || result == nil {
		return nil, fmt.Errorf("model service returned invalid JSON")
	}
	return result, nil
}

func inferenceResult(model NonChatModel, field string) map[string]any {
	var initial any = []any{}
	if field == "answers" {
		initial = map[string]any{}
	}
	return map[string]any{"api": model.API, "provider": model.Provider, "model": model.ID, "stopReason": "stop", "timestamp": time.Now().UnixMilli(), field: initial}
}

func inferenceError(ctx context.Context, result map[string]any, err error) {
	result["stopReason"] = "error"
	if ctx.Err() != nil {
		result["stopReason"] = "aborted"
	}
	result["errorMessage"] = err.Error()
}

func decodeModelNumber(raw json.RawMessage, target *float64) bool {
	var value *float64
	if json.Unmarshal(raw, &value) != nil || value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return false
	}
	*target = *value
	return true
}

func scriptUsage(usage Usage, model Model) map[string]any {
	cost := map[string]any{
		"input":      float64(usage.InputTokens) * model.PriceInput / 1e6,
		"output":     float64(usage.OutputTokens) * model.PriceOutput / 1e6,
		"cacheRead":  float64(usage.CacheReadTokens) * model.PriceCacheRead / 1e6,
		"cacheWrite": float64(usage.CacheWriteTokens) * model.PriceCacheWrite / 1e6,
		"total":      usage.CostUSD,
	}
	return map[string]any{"input": usage.InputTokens, "output": usage.OutputTokens, "cacheRead": usage.CacheReadTokens, "cacheWrite": usage.CacheWriteTokens, "totalTokens": usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens, "cost": cost}
}

func reportedUsage(raw json.RawMessage, model Model) *Usage {
	var fields struct {
		Input      int      `json:"input_tokens"`
		Output     int      `json:"output_tokens"`
		Prompt     int      `json:"prompt_tokens"`
		Completion int      `json:"completion_tokens"`
		Cost       *float64 `json:"cost"`
		Details    struct {
			Cache int `json:"cached_tokens"`
			Write int `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	usage := Usage{InputTokens: max(0, fields.Input) + max(0, fields.Prompt), OutputTokens: max(0, fields.Output) + max(0, fields.Completion), CacheReadTokens: max(0, fields.Details.Cache-fields.Details.Write), CacheWriteTokens: max(0, fields.Details.Write)}
	usage.InputTokens = max(0, usage.InputTokens-usage.CacheReadTokens-usage.CacheWriteTokens)
	usage.CostUSD = (float64(usage.InputTokens)*model.PriceInput + float64(usage.OutputTokens)*model.PriceOutput + float64(usage.CacheReadTokens)*model.PriceCacheRead + float64(usage.CacheWriteTokens)*model.PriceCacheWrite) / 1e6
	if fields.Cost != nil && *fields.Cost >= 0 && !math.IsInf(*fields.Cost, 0) && !math.IsNaN(*fields.Cost) {
		usage.CostUSD = *fields.Cost
	}
	return &usage
}

func (c NonChatClient) Classify(ctx context.Context, model NonChatModel, context ClassifierContext, key string) (map[string]any, *Usage) {
	result := inferenceResult(model, "answers")
	if model.API == "llama-cpp-classify" {
		answers, err := c.classifyLocal(ctx, model, context, key)
		if err != nil {
			inferenceError(ctx, result, err)
		} else {
			result["answers"] = answers
		}
		return result, nil
	}
	if model.API != "typesafe-system-one" && model.API != "cloudflare-workers-ai-system-one" {
		inferenceError(ctx, result, fmt.Errorf("unsupported classifier API"))
		return result, nil
	}
	if key == "" {
		inferenceError(ctx, result, fmt.Errorf("no API key for classifier provider"))
		return result, nil
	}
	questions := map[string]ClassifierQuestion{}
	for id, question := range context.Questions {
		if question.Type == "bool" {
			question.Type = "noul"
		}
		questions[id] = question
	}
	endpoint := strings.TrimRight(model.BaseURL, "/") + "/systemone"
	payload := map[string]any{"model": model.ID, "state": context.State, "questions": questions}
	if model.API == "cloudflare-workers-ai-system-one" {
		base, err := resolveCloudflareURL(model.BaseURL)
		if err != nil {
			inferenceError(ctx, result, err)
			return result, nil
		}
		endpoint = strings.TrimRight(base, "/") + "/run"
		payload = map[string]any{"model": model.ID, "input": map[string]any{"state": context.State, "questions": questions}}
	}
	body, err := c.post(ctx, endpoint, key, payload)
	if err == nil && model.API == "cloudflare-workers-ai-system-one" {
		var envelope struct {
			Success bool `json:"success"`
			Result  struct {
				State  string                     `json:"state"`
				Result map[string]json.RawMessage `json:"result"`
			} `json:"result"`
		}
		encoded, _ := json.Marshal(body)
		if json.Unmarshal(encoded, &envelope) != nil || !envelope.Success || envelope.Result.State != "Completed" || envelope.Result.Result == nil {
			err = fmt.Errorf("classifier service returned an incomplete run")
		} else {
			body = envelope.Result.Result
		}
	}
	if err != nil {
		inferenceError(ctx, result, err)
		return result, nil
	}
	usage := reportedUsage(body["usage"], model.Model)
	if usage != nil {
		result["usage"] = scriptUsage(*usage, model.Model)
	}
	var answers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body["answers"], &answers); err != nil {
		inferenceError(ctx, result, fmt.Errorf("classifier returned invalid answers"))
		return result, usage
	}
	parsed := map[string]any{}
	for id, question := range context.Questions {
		answer := answers[id]
		var kind string
		_ = json.Unmarshal(answer["type"], &kind)
		var value map[string]any
		if question.Type == "bool" {
			var probability float64
			if kind != "noul" || len(answer["noul"]) == 0 || !decodeModelNumber(answer["noul"], &probability) {
				inferenceError(ctx, result, fmt.Errorf("classifier returned invalid bool answer"))
				return result, usage
			}
			value = map[string]any{"type": "bool", "probability": probability}
		} else {
			if kind != question.Type || len(answer["confidence"]) == 0 {
				inferenceError(ctx, result, fmt.Errorf("classifier returned invalid answer type"))
				return result, usage
			}
			var confidence float64
			if !decodeModelNumber(answer["confidence"], &confidence) {
				inferenceError(ctx, result, fmt.Errorf("classifier returned invalid confidence"))
				return result, usage
			}
			value = map[string]any{"type": kind, "confidence": confidence}
			if kind == "score" {
				var score float64
				if len(answer["score"]) == 0 || !decodeModelNumber(answer["score"], &score) {
					inferenceError(ctx, result, fmt.Errorf("classifier returned invalid score"))
					return result, usage
				}
				value["score"] = score
			} else {
				var choice *string
				var rawProbabilities map[string]json.RawMessage
				if json.Unmarshal(answer["choice"], &choice) != nil || choice == nil || json.Unmarshal(answer["probabilities"], &rawProbabilities) != nil || rawProbabilities == nil {
					inferenceError(ctx, result, fmt.Errorf("classifier returned invalid choice"))
					return result, usage
				}
				probabilities := map[string]float64{}
				for label, raw := range rawProbabilities {
					var probability float64
					if !decodeModelNumber(raw, &probability) {
						inferenceError(ctx, result, fmt.Errorf("classifier returned invalid probability"))
						return result, usage
					}
					probabilities[label] = probability
				}
				value["choice"], value["probabilities"] = *choice, probabilities
			}
		}
		parsed[id] = value
	}
	result["answers"] = parsed
	return result, usage
}

func (c NonChatClient) GenerateImages(ctx context.Context, model NonChatModel, context ImagesContext, key string) (map[string]any, *Usage) {
	result := inferenceResult(model, "output")
	if model.API != "openrouter-images" {
		inferenceError(ctx, result, fmt.Errorf("unsupported image API"))
		return result, nil
	}
	if key == "" {
		inferenceError(ctx, result, fmt.Errorf("no API key for image provider"))
		return result, nil
	}
	input := make([]map[string]any, 0, len(context.Input))
	for _, block := range context.Input {
		if block.Type == "text" {
			input = append(input, map[string]any{"type": "text", "text": *block.Text})
		} else {
			input = append(input, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + *block.MimeType + ";base64," + *block.Data}})
		}
	}
	modalities := []string{"image"}
	for _, kind := range model.Output {
		if kind == "text" {
			modalities = append(modalities, "text")
			break
		}
	}
	body, err := c.post(ctx, strings.TrimRight(model.BaseURL, "/")+"/chat/completions", key, map[string]any{"model": model.ID, "stream": false, "modalities": modalities, "messages": []map[string]any{{"role": "user", "content": input}}})
	if err != nil {
		inferenceError(ctx, result, err)
		return result, nil
	}
	usage := reportedUsage(body["usage"], model.Model)
	if usage != nil {
		result["usage"] = scriptUsage(*usage, model.Model)
	}
	var choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
			Images  []struct {
				URL json.RawMessage `json:"image_url"`
			} `json:"images"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body["choices"], &choices); err != nil || len(choices) == 0 {
		inferenceError(ctx, result, fmt.Errorf("image service returned no choice"))
		return result, usage
	}
	output := make([]map[string]any, 0)
	var text string
	if json.Unmarshal(choices[0].Message.Content, &text) == nil && text != "" {
		output = append(output, map[string]any{"type": "text", "text": text})
	}
	for _, image := range choices[0].Message.Images {
		var url string
		if json.Unmarshal(image.URL, &url) != nil {
			var object struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(image.URL, &object)
			url = object.URL
		}
		header, data, ok := strings.Cut(url, ",")
		if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			continue
		}
		output = append(output, map[string]any{"type": "image", "mimeType": strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"), "data": data})
	}
	result["output"] = output
	return result, usage
}
