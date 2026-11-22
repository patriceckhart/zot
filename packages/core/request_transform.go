package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/patriceckhart/zot/packages/provider"
)

const (
	MaxRequestTransformBytes    = 16 << 20
	MaxRequestTransformSegments = 10000
)

// RequestSegment identifies a rewritable logical request value. Skipped values
// carry no content. IDs are positional and stable only within this request.
type RequestSegment struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Skipped string          `json:"skipped,omitempty"`
}

// RequestTransformError is terminal, even when the underlying failure resembles
// a retryable provider error. It never triggers compaction fallback.
type RequestTransformError struct{ Err error }

func (e *RequestTransformError) Error() string { return "before_request transform failed" }
func (e *RequestTransformError) Unwrap() error { return e.Err }

func (a *Agent) transformRequest(ctx context.Context, purpose string, req provider.Request) (provider.Request, error) {
	if err := ctx.Err(); err != nil {
		return req, err
	}
	if a.BeforeRequest == nil {
		return req, nil
	}
	out, err := a.BeforeRequest(ctx, purpose, req)
	if err != nil {
		return req, &RequestTransformError{Err: err}
	}
	if err := ctx.Err(); err != nil {
		return req, err
	}
	return out, nil
}

// RequestSegments extracts logical content without serializing opaque provider
// blocks. Replacement and extraction share a traversal to prevent ID drift.
func RequestSegments(req provider.Request) ([]RequestSegment, error) {
	_, segments, err := rewriteRequest(req, nil)
	return segments, err
}

// ReplaceRequestSegments validates every known replacement before returning a
// detached request. The input and its transcript-owned slices remain unchanged.
// Unknown IDs are ignored. JSON object keys, array lengths and value types must
// remain identical. Signed blocks and encrypted reasoning cannot be replaced.
func ReplaceRequestSegments(req provider.Request, replacements map[string]json.RawMessage) (provider.Request, error) {
	out, _, err := rewriteRequest(req, replacements)
	if err != nil {
		return req, err
	}
	return out, nil
}

// RequestTransformHash hashes canonical JSON of provider, model, purpose and
// ordered segments. Session identity is a separate cache-key component.
func RequestTransformHash(providerID, model, purpose string, segments []RequestSegment) string {
	b, _ := json.Marshal(struct {
		Provider string           `json:"provider"`
		Model    string           `json:"model"`
		Purpose  string           `json:"purpose"`
		Segments []RequestSegment `json:"segments"`
	}{providerID, model, purpose, segments})
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

func rewriteRequest(req provider.Request, replacements map[string]json.RawMessage) (provider.Request, []RequestSegment, error) {
	var segments []RequestSegment
	budget := 0
	var failure error
	add := func(s RequestSegment) RequestSegment {
		if failure != nil {
			return s
		}
		if raw, ok := replacements[s.ID]; ok && s.Skipped == "" {
			if len(raw) > 6*MaxRequestTransformBytes {
				failure = errors.New("replacement content limit exceeded")
				return s
			}
			switch s.Kind {
			case "text":
				if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s.Text) != nil {
					failure = errors.New("text replacement must be a string")
				}
			case "json":
				old, err := decodeTransformJSON(s.Value)
				if err != nil {
					failure = errors.New("invalid source JSON")
					break
				}
				value, err := decodeTransformJSON(raw)
				if err != nil || !sameJSONShape(old, value, 0) {
					failure = errors.New("JSON replacement changes structure or value type")
					break
				}
				s.Value, _ = json.Marshal(value)
			}
		}
		budget += len(s.Text) + len(s.Value)
		if budget > MaxRequestTransformBytes || len(segments) >= MaxRequestTransformSegments {
			failure = errors.New("request transform content limit exceeded")
			return s
		}
		segments = append(segments, s)
		return s
	}
	text := func(id, value string) string { return add(RequestSegment{ID: id, Kind: "text", Text: value}).Text }
	jsonValue := func(id string, value json.RawMessage) json.RawMessage {
		if len(value) > MaxRequestTransformBytes {
			failure = errors.New("request transform content limit exceeded")
			return value
		}
		v, err := decodeTransformJSON(value)
		if err != nil {
			failure = errors.New("invalid source JSON")
			return value
		}
		if !sameJSONShape(v, v, 0) {
			failure = errors.New("request transform JSON nesting limit exceeded")
			return value
		}
		canonical, _ := json.Marshal(v)
		segment := add(RequestSegment{ID: id, Kind: "json", Value: canonical})
		if _, changed := replacements[id]; !changed {
			return value
		}
		return segment.Value
	}
	var content func([]provider.Content, string, int) []provider.Content
	content = func(blocks []provider.Content, prefix string, depth int) []provider.Content {
		if depth > 64 {
			failure = errors.New("request transform nesting limit exceeded")
			return blocks
		}
		out := slices.Clone(blocks)
		for i, block := range blocks {
			if failure != nil {
				break
			}
			id := fmt.Sprintf("%s.c%d", prefix, i)
			switch b := block.(type) {
			case provider.TextBlock:
				if b.ThoughtSignature != "" {
					add(RequestSegment{ID: id, Kind: "text", Skipped: "signed"})
					continue
				}
				b.Text = text(id, b.Text)
				out[i] = b
			case provider.ToolCallBlock:
				if b.ThoughtSignature != "" {
					add(RequestSegment{ID: id, Kind: "json", Skipped: "signed"})
					continue
				}
				b.Arguments = jsonValue(id, b.Arguments)
				out[i] = b
			case provider.ToolResultBlock:
				b.Content = content(b.Content, id+".r", depth+1)
				out[i] = b
			case provider.ImageBlock:
				add(RequestSegment{ID: id, Kind: "opaque", Skipped: "image"})
			case provider.ReasoningBlock:
				add(RequestSegment{ID: id, Kind: "opaque", Skipped: "reasoning"})
			default:
				add(RequestSegment{ID: id, Kind: "opaque", Skipped: "unsupported"})
			}
		}
		return out
	}
	req.System = text("system", req.System)
	req.Messages = slices.Clone(req.Messages)
	for i := range req.Messages {
		if failure != nil {
			break
		}
		req.Messages[i].Content = content(req.Messages[i].Content, fmt.Sprintf("m%d", i), 0)
	}
	req.Tools = slices.Clone(req.Tools)
	for i := range req.Tools {
		if failure != nil {
			break
		}
		t := &req.Tools[i]
		prefix := fmt.Sprintf("t%d", i)
		t.Description = text(prefix+".description", t.Description)
		t.NamespaceDescription = text(prefix+".namespace_description", t.NamespaceDescription)
		t.NamespaceInstructions = text(prefix+".namespace_instructions", t.NamespaceInstructions)
		if len(t.Schema) > 0 {
			t.Schema = jsonValue(prefix+".schema", t.Schema)
		}
		if t.ConstrainedSampling != nil {
			add(RequestSegment{ID: prefix + ".grammar", Kind: "opaque", Skipped: "grammar"})
		}
	}
	return req, segments, failure
}

func decodeTransformJSON(raw json.RawMessage) (any, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	err := d.Decode(&value)
	return value, err
}

func sameJSONShape(a, b any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameJSONShape(v, w, depth+1) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i, v := range x {
			if !sameJSONShape(v, y[i], depth+1) {
				return false
			}
		}
		return true
	case string:
		_, ok := b.(string)
		return ok
	case json.Number:
		_, ok := b.(json.Number)
		return ok
	case bool:
		_, ok := b.(bool)
		return ok
	case nil:
		return b == nil
	}
	return false
}
