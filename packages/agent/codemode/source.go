package codemode

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

type sourceOptions struct {
	Code            string
	MaxOutputTokens int64
	Timeout         time.Duration
}

func parseSource(raw json.RawMessage) (sourceOptions, error) {
	input := string(raw)
	var legacyTimeout *int64
	if strings.HasPrefix(strings.TrimSpace(input), "\"") {
		if err := json.Unmarshal(raw, &input); err != nil {
			return sourceOptions{}, err
		}
	} else if strings.HasPrefix(strings.TrimSpace(input), "{") {
		var args struct {
			Code    *string `json:"code"`
			Timeout *int64  `json:"timeout_ms"`
		}
		if err := json.Unmarshal(raw, &args); err == nil {
			if args.Code == nil {
				return sourceOptions{}, fmt.Errorf("codemode requires JavaScript source")
			}
			input = *args.Code
			legacyTimeout = args.Timeout
		}
	}
	out := sourceOptions{Code: input, MaxOutputTokens: 10000}
	if strings.TrimSpace(input) == "" {
		return out, fmt.Errorf("expected non-empty JavaScript source")
	}
	if legacyTimeout != nil {
		if *legacyTimeout <= 0 || *legacyTimeout > 2147483647 {
			return out, fmt.Errorf("timeout_ms must be a positive integer up to 2147483647")
		}
		out.Timeout = time.Duration(*legacyTimeout) * time.Millisecond
	}
	first, rest, found := strings.Cut(input, "\n")
	first = strings.TrimLeft(first, " \t")
	if !strings.HasPrefix(first, "// @options:") {
		return out, nil
	}
	if !found || strings.TrimSpace(rest) == "" {
		return out, fmt.Errorf("the @options line must be followed by JavaScript source")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(first, "// @options:"))), &fields); err != nil || fields == nil {
		return out, fmt.Errorf("@options must be a JSON object")
	}
	for key, value := range fields {
		n, ok := value.(float64)
		if !ok || n < 0 || n > 9007199254740991 || math.Trunc(n) != n {
			return out, fmt.Errorf("@options %s must be a non-negative safe integer", key)
		}
		switch key {
		case "max_output_tokens":
			out.MaxOutputTokens = int64(n)
		case "timeout_ms":
			if n == 0 || n > 2147483647 {
				return out, fmt.Errorf("@options timeout_ms must be a positive integer up to 2147483647")
			}
			out.Timeout = time.Duration(n) * time.Millisecond
		default:
			return out, fmt.Errorf("@options only supports max_output_tokens and timeout_ms")
		}
	}
	out.Code = "\n" + rest
	return out, nil
}
