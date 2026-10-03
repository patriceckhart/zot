package provider

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var grammarModelVersion = regexp.MustCompile(`^gpt-(\d+)`)

func supportsGrammarTools(model Model, provider string, responses bool) bool {
	if model.SupportsOpenAIGrammarTools != nil {
		return *model.SupportsOpenAIGrammarTools
	}
	if !responses {
		return false
	}
	switch provider {
	case "openai", "openai-responses", "openai-codex", "azure-openai", "azure-openai-responses", "github-copilot", "opencode", "cloudflare-ai-gateway":
	default:
		return false
	}
	match := grammarModelVersion.FindStringSubmatch(model.ID)
	if len(match) != 2 {
		return false
	}
	version, _ := strconv.Atoi(match[1])
	return version >= 5
}

func grammarTools(tools []Tool, supported bool) map[string]string {
	out := map[string]string{}
	if !supported {
		return out
	}
	for _, tool := range tools {
		sampling := tool.ConstrainedSampling
		if sampling == nil || sampling.Type != "grammar" || sampling.Variants["openai_lark"] == "" {
			continue
		}
		property := sampling.InputProperty
		if property == "" {
			var schema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			}
			if json.Unmarshal(tool.Schema, &schema) != nil || len(schema.Properties) != 1 {
				continue
			}
			for name, field := range schema.Properties {
				if field.Type == "string" {
					property = name
				}
			}
		}
		if property != "" {
			out[tool.Name] = property
		}
	}
	return out
}

type customToolFormat struct {
	Type       string `json:"type"`
	Syntax     string `json:"syntax"`
	Definition string `json:"definition"`
}

func grammarFormat(tool Tool) *customToolFormat {
	return &customToolFormat{Type: "grammar", Syntax: "lark", Definition: tool.ConstrainedSampling.Variants["openai_lark"]}
}

func customToolInput(args json.RawMessage, property string) string {
	var object map[string]json.RawMessage
	var input string
	if json.Unmarshal(args, &object) == nil && json.Unmarshal(object[property], &input) == nil {
		return input
	}
	if json.Unmarshal(args, &input) == nil {
		return input
	}
	return string(args)
}

// customInputBuffer translates raw input deltas into the established JSON
// argument stream. Stored transcripts and non-native providers stay compatible.
type customInputBuffer struct {
	property string
	source   strings.Builder
	started  bool
	closed   bool
}

func (b *customInputBuffer) arguments() json.RawMessage {
	raw, _ := json.Marshal(map[string]string{b.property: b.source.String()})
	return raw
}

func (b *customInputBuffer) append(delta string, close bool) string {
	if b.closed {
		return ""
	}
	var out strings.Builder
	if !b.started {
		key, _ := json.Marshal(b.property)
		out.WriteByte('{')
		out.Write(key)
		out.WriteString(`:"`)
		b.started = true
	}
	b.source.WriteString(delta)
	escaped, _ := json.Marshal(delta)
	out.Write(escaped[1 : len(escaped)-1])
	if close {
		out.WriteString(`"}`)
		b.closed = true
	}
	return out.String()
}

func (b *customInputBuffer) finish(input *string) string {
	delta := ""
	if input != nil && strings.HasPrefix(*input, b.source.String()) {
		delta = (*input)[b.source.Len():]
	}
	out := b.append(delta, true)
	if input != nil && b.source.String() != *input {
		b.source.Reset()
		b.source.WriteString(*input)
	}
	return out
}
