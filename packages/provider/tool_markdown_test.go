package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolDisplayFormatNotSentToProviders(t *testing.T) {
	text := "# Findings\n\n**Important**"
	blocks := []Content{TextBlock{Text: text, Format: "markdown"}}
	anthropic, err := anthBuildToolResultContent(blocks)
	if err != nil {
		t.Fatal(err)
	}
	var decoded string
	if err := json.Unmarshal(anthropic, &decoded); err != nil || decoded != text {
		t.Fatalf("Anthropic text changed: %s, %v", anthropic, err)
	}
	if got := buildOAIToolContent(blocks, false, true); got != text {
		t.Fatalf("OpenAI text changed: %q", got)
	}
	gemini, err := json.Marshal(convertGemToolResultParts([]Content{ToolResultBlock{CallID: "call", Content: blocks}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gemini), `"format"`) || !strings.Contains(string(gemini), "**Important**") {
		t.Fatalf("unexpected Gemini content: %s", gemini)
	}
}
