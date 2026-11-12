package extproto

import "encoding/json"

// RequestTransformFrameLimit includes JSON escaping and a trailing newline.
// It applies only after request_transform capability negotiation.
const RequestTransformFrameLimit = 128 << 20

// RequestTransformOptions applies to all transform failures, not only timeouts.
// OnTimeout is retained as the wire name for the failure policy. Empty means
// fail. TimeoutMS defaults to 30000 and must be between 1 and 300000.
type RequestTransformOptions struct {
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	OnTimeout string `json:"on_timeout,omitempty"`
}

type RequestSegment struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Skipped string          `json:"skipped,omitempty"`
}

type RequestTransformFromHost struct {
	Type        string           `json:"type"`
	ID          string           `json:"id"`
	SessionID   string           `json:"session_id"`
	Provider    string           `json:"provider"`
	Model       string           `json:"model"`
	Purpose     string           `json:"purpose"`
	RequestHash string           `json:"request_hash"`
	DeadlineMS  int64            `json:"deadline_ms,omitempty"`
	Segments    []RequestSegment `json:"segments"`
}

type RequestTransformResponseFromExt struct {
	Type         string                     `json:"type"`
	ID           string                     `json:"id"`
	Replacements map[string]json.RawMessage `json:"replacements,omitempty"`
	Error        string                     `json:"error,omitempty"`
}
