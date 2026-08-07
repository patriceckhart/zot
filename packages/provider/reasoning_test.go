package provider

import (
	"slices"
	"testing"
)

func TestAvailableReasoningLevels(t *testing.T) {
	tests := []struct {
		name  string
		model Model
		want  []string
	}{
		{name: "unsupported", model: Model{}, want: []string{""}},
		{name: "generic openai", model: Model{Reasoning: true}, want: []string{"", "low", "medium", "high"}},
		{name: "responses", model: Model{Reasoning: true, API: APIResponses, ID: "gpt-5.5"}, want: []string{"", "low", "medium", "high", "xhigh"}},
		{name: "responses native max", model: Model{Reasoning: true, API: APIResponses, ID: "gpt-5.6-sol"}, want: []string{"", "low", "medium", "high", "xhigh", "max"}},
		{name: "adaptive", model: Model{Reasoning: true, AdaptiveThinking: true}, want: []string{"", "low", "medium", "high", "xhigh", "max"}},
		{name: "adaptive compatible", model: Model{Reasoning: true, AdaptiveThinkingCompat: true}, want: []string{"", "high"}},
		{name: "gemini pro", model: Model{Provider: "google", ID: "gemini-3-pro-preview", Reasoning: true}, want: []string{"", "low", "high"}},
		{name: "gemini flash", model: Model{Provider: "google", ID: "gemini-3-flash-preview", Reasoning: true}, want: []string{"", "minimum", "low", "medium", "high"}},
		{name: "gemini 2.5 budget", model: Model{Provider: "google", ID: "gemini-2.5-flash", Reasoning: true}, want: []string{"", "minimum", "low", "medium", "high", "xhigh"}},
		{name: "gemini alias without control", model: Model{Provider: "google", ID: "gemini-flash-latest", Reasoning: true}, want: []string{""}},
		{name: "anthropic budget", model: Model{Provider: "anthropic", Reasoning: true}, want: []string{"", "minimum", "low", "medium", "high", "xhigh"}},
		{name: "bedrock without effort control", model: Model{Provider: "amazon-bedrock", Reasoning: true}, want: []string{""}},
		{
			name: "per-model overrides",
			model: Model{
				Provider: "anthropic", Reasoning: true,
				ReasoningLevelMap: map[string]string{"minimum": "low", "high": "", "max": "max"},
			},
			want: []string{"", "low", "medium", "xhigh", "max"},
		},
		{
			name: "custom provider expands above high",
			model: Model{
				Reasoning: true,
				ReasoningLevelMap: map[string]string{
					"minimum": "",
					"low":     "",
					"medium":  "",
					"high":    "high",
					"xhigh":   "xhigh",
					"max":     "max",
				},
			},
			want: []string{"", "high", "xhigh", "max"},
		},
		{
			name: "override adds max without removing defaults",
			model: Model{
				Reasoning:         true,
				ReasoningLevelMap: map[string]string{"max": "max"},
			},
			want: []string{"", "low", "medium", "high", "max"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AvailableReasoningLevels(tt.model); !slices.Equal(got, tt.want) {
				t.Fatalf("levels = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClampReasoningForModel(t *testing.T) {
	tests := []struct {
		name  string
		model Model
		level string
		want  string
	}{
		{name: "unsupported", model: Model{}, level: "high", want: ""},
		{name: "generic max", model: Model{Reasoning: true}, level: "max", want: "high"},
		{name: "adaptive minimum", model: Model{Reasoning: true, AdaptiveThinking: true}, level: "minimum", want: "low"},
		{name: "gemini pro medium", model: Model{Provider: "google", ID: "gemini-3-pro", Reasoning: true}, level: "medium", want: "high"},
		{name: "native max", model: Model{Reasoning: true, API: APIResponses, ID: "gpt-5.6-sol"}, level: "max", want: "max"},
		{
			name:  "explicit equivalent",
			model: Model{Reasoning: true, ReasoningLevelMap: map[string]string{"minimum": "high"}},
			level: "minimum",
			want:  "high",
		},
		{
			name: "custom map keeps max",
			model: Model{
				Reasoning: true,
				ReasoningLevelMap: map[string]string{
					"minimum": "", "low": "", "medium": "",
					"high": "high", "xhigh": "xhigh", "max": "max",
				},
			},
			level: "max",
			want:  "max",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClampReasoningForModel(tt.model, tt.level); got != tt.want {
				t.Fatalf("level = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenAIRequestUsesReasoningLevelMap(t *testing.T) {
	SetLiveModels([]Model{{
		Provider:          "openai",
		ID:                "mapped-reasoning-model",
		Reasoning:         true,
		ReasoningLevelMap: map[string]string{"high": "low"},
	}})
	t.Cleanup(func() { SetLiveModels(nil) })

	client := NewOpenAI("test", "").(*openaiClient)
	request, err := client.buildRequest(Request{Model: "mapped-reasoning-model", Reasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if request.ReasoningEffort != "low" {
		t.Fatalf("reasoning effort = %q, want low", request.ReasoningEffort)
	}
}

func TestOpenAIRequestSendsMappedMaxEffort(t *testing.T) {
	SetLiveModels([]Model{{
		Provider:  "custom",
		ID:        "deepseek-reasoner",
		Reasoning: true,
		ReasoningLevelMap: map[string]string{
			"minimum": "", "low": "", "medium": "",
			"high": "high", "xhigh": "xhigh", "max": "max",
		},
	}})
	t.Cleanup(func() { SetLiveModels(nil) })

	client := NewOpenAI("test", "").(*openaiClient)
	request, err := client.buildRequest(Request{Model: "deepseek-reasoner", Reasoning: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if request.ReasoningEffort != "max" {
		t.Fatalf("reasoning effort = %q, want max", request.ReasoningEffort)
	}
}

func TestReasoningEffortMappings(t *testing.T) {
	cases := []struct {
		level      string
		model      string
		openai     string
		anthCompat string
		codex      string
		budget     int
		normalized string
	}{
		{"off", "gpt-5.6-sol", "", "", "", 0, ""},
		{"minimum", "gpt-5.6-sol", "low", "low", "low", 1024, "minimum"},
		{"minimal", "gpt-5.6-sol", "low", "low", "low", 1024, "minimum"},
		{"low", "gpt-5.6-sol", "low", "low", "low", 2048, "low"},
		{"medium", "gpt-5.6-sol", "medium", "medium", "medium", 8192, "medium"},
		{"high", "gpt-5.6-sol", "high", "high", "high", 16384, "high"},
		{"maximum", "gpt-5.6-sol", "xhigh", "xhigh", "xhigh", 32768, "xhigh"},
		{"xhigh", "gpt-5.6-sol", "xhigh", "xhigh", "xhigh", 32768, "xhigh"},
		{"max", "gpt-5.6-sol", "max", "max", "max", 32768, "max"},
		{"max", "gpt-5.5", "max", "max", "xhigh", 32768, "max"},
	}
	for _, tc := range cases {
		if got := NormalizeReasoning(tc.level); got != tc.normalized {
			t.Errorf("NormalizeReasoning(%q)=%q want %q", tc.level, got, tc.normalized)
		}
		if got := OpenAIReasoningEffort(tc.level); got != tc.openai {
			t.Errorf("OpenAIReasoningEffort(%q)=%q want %q", tc.level, got, tc.openai)
		}
		if got := OpenAICompatAnthropicEffort(tc.level); got != tc.anthCompat {
			t.Errorf("OpenAICompatAnthropicEffort(%q)=%q want %q", tc.level, got, tc.anthCompat)
		}
		if got := OpenAICodexReasoningEffort(tc.level, tc.model); got != tc.codex {
			t.Errorf("OpenAICodexReasoningEffort(%q, %q)=%q want %q", tc.level, tc.model, got, tc.codex)
		}
		if got := ReasoningBudget(tc.level); got != tc.budget {
			t.Errorf("ReasoningBudget(%q)=%d want %d", tc.level, got, tc.budget)
		}
	}
}
