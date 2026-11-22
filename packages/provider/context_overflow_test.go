package provider

import (
	"errors"
	"testing"
)

func TestIsContextOverflowError(t *testing.T) {
	if IsContextOverflowError(nil) {
		t.Fatal("nil classified as overflow")
	}
	for _, message := range []string{
		"HTTP 413", "status 413", "413 request rejected", "Payload Too Large",
		"request entity too large", "input exceeds the context window",
		"context window exceeded", "maximum context length is 1000",
		"context_length_exceeded", "input exceeds the maximum number of tokens",
	} {
		if !IsContextOverflowError(errors.New(message)) {
			t.Errorf("missed overflow: %q", message)
		}
	}
	for _, message := range []string{"HTTP 401", "HTTP 429", "connection reset", "context canceled", "invalid tool schema"} {
		if IsContextOverflowError(errors.New(message)) {
			t.Errorf("incorrect overflow: %q", message)
		}
	}
}
