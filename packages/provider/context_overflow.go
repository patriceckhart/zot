package provider

import "strings"

// IsContextOverflowError reports provider rejections caused by oversized input
// or request payloads. Providers do not share a structured error format.
func IsContextOverflowError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 413") ||
		strings.Contains(msg, " 413") ||
		strings.HasPrefix(msg, "413 ") ||
		strings.Contains(msg, "payload too large") ||
		strings.Contains(msg, "request entity too large") ||
		strings.Contains(msg, "input exceeds the context window") ||
		strings.Contains(msg, "context window exceeded") ||
		strings.Contains(msg, "maximum context length") ||
		strings.Contains(msg, "context_length_exceeded") ||
		(strings.Contains(msg, "input") && strings.Contains(msg, "exceeds the maximum number of tokens"))
}
