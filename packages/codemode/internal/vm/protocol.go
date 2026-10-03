// Package codemodevm is the isolated JavaScript worker and its wire contract.
// It deliberately has no dependency on agent configuration, credentials or tools.
package codemodevm

import "encoding/json"

type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Namespace    string          `json:"namespace,omitempty"`
	Schema       json.RawMessage `json:"schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

type Start struct {
	Code   string                     `json:"code"`
	Tools  []Tool                     `json:"tools"`
	Store  map[string]json.RawMessage `json:"store"`
	Models bool                       `json:"models"`
}

type Item struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Frame carries output and capability requests out, and JSON replies back in.
type Frame struct {
	Type         string                     `json:"type"`
	ID           int                        `json:"id,omitempty"`
	Name         string                     `json:"name,omitempty"`
	Args         json.RawMessage            `json:"args,omitempty"`
	Value        json.RawMessage            `json:"value,omitempty"`
	Item         *Item                      `json:"item,omitempty"`
	OK           bool                       `json:"ok,omitempty"`
	Error        string                     `json:"error,omitempty"`
	Store        map[string]json.RawMessage `json:"store,omitempty"`
	StoreWritten bool                       `json:"store_written,omitempty"`
}

// Identifier preserves ASCII identifier characters, replacing everything else.
func Identifier(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '$' || len(out) > 0 && r >= '0' && r <= '9'
		if !valid {
			r = '_'
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
