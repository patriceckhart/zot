package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

func (c NonChatClient) classifyLocal(ctx context.Context, model NonChatModel, input ClassifierContext, key string) (map[string]any, error) {
	root := strings.TrimSuffix(strings.TrimRight(model.BaseURL, "/"), "/v1")
	ids := localQuestionIDs(input)
	answers := map[string]any{}
	for _, id := range ids {
		question := input.Questions[id]
		labels, keys, _, err := localLabels(question)
		if err != nil {
			return nil, err
		}
		tokens := make([]int, len(labels))
		newline, err := c.labelTokens(ctx, root, model.ID, key, "\n")
		if err != nil {
			return nil, err
		}
		seen := map[int]bool{}
		for i, label := range labels {
			withLabel, err := c.labelTokens(ctx, root, model.ID, key, "\n"+label)
			if err != nil {
				return nil, err
			}
			var token int
			if len(withLabel) == len(newline)+1 && equalTokens(newline, withLabel[:len(newline)]) {
				token = withLabel[len(newline)]
			} else {
				alone, err := c.labelTokens(ctx, root, model.ID, key, label)
				if err != nil {
					return nil, err
				}
				if len(alone) != 1 {
					return nil, fmt.Errorf("classifier answer labels must be single tokens")
				}
				token = alone[0]
			}
			if seen[token] {
				return nil, fmt.Errorf("classifier labels share a token")
			}
			seen[token] = true
			tokens[i] = token
		}
		prompt, err := localQuestionPrompt(input, id, labels)
		if err != nil {
			return answers, err
		}
		template, err := c.post(ctx, root+"/apply-template", key, map[string]any{"model": model.ID, "messages": []map[string]string{{"role": "system", "content": localClassifierSystem}, {"role": "user", "content": prompt}}, "chat_template_kwargs": map[string]bool{"enable_thinking": false}})
		if err != nil {
			return nil, err
		}
		var rendered string
		if json.Unmarshal(template["prompt"], &rendered) != nil || rendered == "" {
			return nil, fmt.Errorf("local classifier returned no chat template")
		}
		if strings.HasSuffix(rendered, "<think>") {
			rendered += "</think>"
		}
		var logits []float64
		for _, depth := range []int{max(256, 16*len(labels)), 4096, 32768} {
			completion, err := c.post(ctx, root+"/completion", key, map[string]any{"model": model.ID, "prompt": rendered, "n_predict": 1, "n_probs": depth, "post_sampling_probs": false, "cache_prompt": true, "temperature": 0})
			if err != nil {
				return nil, err
			}
			var positions []struct {
				Probabilities []struct {
					ID    int      `json:"id"`
					Value *float64 `json:"logprob"`
				} `json:"top_logprobs"`
			}
			if json.Unmarshal(completion["completion_probabilities"], &positions) != nil || len(positions) == 0 {
				return nil, fmt.Errorf("local classifier returned no token probabilities")
			}
			byToken := map[int]float64{}
			for _, probability := range positions[0].Probabilities {
				if probability.Value != nil {
					byToken[probability.ID] = *probability.Value
				}
			}
			logits = nil
			for _, token := range tokens {
				value, ok := byToken[token]
				if !ok {
					logits = nil
					break
				}
				logits = append(logits, value)
			}
			if len(logits) == len(tokens) {
				break
			}
		}
		if len(logits) != len(tokens) {
			return nil, fmt.Errorf("local classifier did not return every answer label")
		}
		probabilities, err := labelSoftmax(logits)
		if err != nil {
			return nil, err
		}
		answers[id] = labelAnswer(question.Type, keys, probabilities)
	}
	return answers, nil
}

func (c NonChatClient) labelTokens(ctx context.Context, root, model, key, content string) ([]int, error) {
	body, err := c.post(ctx, root+"/tokenize", key, map[string]any{"model": model, "content": content, "add_special": false, "parse_special": false})
	if err != nil {
		return nil, err
	}
	var rawTokens []json.RawMessage
	if json.Unmarshal(body["tokens"], &rawTokens) != nil || rawTokens == nil {
		return nil, fmt.Errorf("local classifier returned invalid tokens")
	}
	tokens := make([]int, 0, len(rawTokens))
	for _, raw := range rawTokens {
		var token int
		if json.Unmarshal(raw, &token) != nil {
			var object struct {
				ID *int `json:"id"`
			}
			if json.Unmarshal(raw, &object) != nil || object.ID == nil {
				return nil, fmt.Errorf("local classifier returned invalid tokens")
			}
			token = *object.ID
		}
		tokens = append(tokens, token)
	}
	return tokens, nil
}

func equalTokens(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func localLabels(question ClassifierQuestion) (labels, keys, meanings []string, err error) {
	switch question.Type {
	case "choice":
		var criteria map[string]string
		_ = json.Unmarshal(question.Criteria, &criteria)
		keys = jsonObjectKeys(question.Criteria)
		alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		if len(keys) < 2 || len(keys) > len(alphabet) {
			return nil, nil, nil, fmt.Errorf("local choice needs 2 to 62 labels")
		}
		for i, key := range keys {
			labels = append(labels, string(alphabet[i]))
			meanings = append(meanings, key+": "+criteria[key])
		}
	case "score":
		_ = json.Unmarshal(question.Criteria, &meanings)
		if len(meanings) < 2 || len(meanings) > 10 {
			return nil, nil, nil, fmt.Errorf("local score needs 2 to 10 levels")
		}
		for i := range meanings {
			label := fmt.Sprint(i)
			labels = append(labels, label)
			keys = append(keys, label)
		}
	case "bool":
		var criteria map[string]string
		_ = json.Unmarshal(question.Criteria, &criteria)
		labels = []string{"Yes", "No"}
		keys = []string{"true", "false"}
		meanings = []string{criteria["true"], criteria["false"]}
	default:
		return nil, nil, nil, fmt.Errorf("unknown classifier question type")
	}
	return labels, keys, meanings, nil
}

func labelSoftmax(logits []float64) ([]float64, error) {
	peak := math.Inf(-1)
	for _, value := range logits {
		if value > peak {
			peak = value
		}
	}
	if peak <= -1e30 || math.IsInf(peak, 0) || math.IsNaN(peak) {
		return nil, fmt.Errorf("classifier gave no probability to any label")
	}
	out := make([]float64, len(logits))
	sum := 0.0
	for i, value := range logits {
		out[i] = math.Exp(value - peak)
		sum += out[i]
	}
	if sum == 0 || math.IsNaN(sum) {
		return nil, fmt.Errorf("invalid label probabilities")
	}
	for i := range out {
		out[i] /= sum
	}
	return out, nil
}

func labelAnswer(kind string, keys []string, probabilities []float64) map[string]any {
	best := 0
	for i := range probabilities {
		if probabilities[i] > probabilities[best] {
			best = i
		}
	}
	if kind == "bool" {
		return map[string]any{"type": "bool", "probability": probabilities[0]}
	}
	confidence := min(1, max(0, (float64(len(probabilities))*probabilities[best]-1)/(float64(len(probabilities))-1)))
	if kind == "score" {
		score := 0.0
		for i, p := range probabilities {
			score += float64(i) * p
		}
		return map[string]any{"type": "score", "score": score, "confidence": confidence}
	}
	values := map[string]float64{}
	for i, key := range keys {
		values[key] = probabilities[i]
	}
	return map[string]any{"type": "choice", "choice": keys[best], "probabilities": values, "confidence": confidence}
}
