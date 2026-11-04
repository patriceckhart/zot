package provider

import "testing"

func TestCloudGPUOfflineRequestMetadata(t *testing.T) {
	SetLiveModels(nil)
	t.Cleanup(func() { SetLiveModels(nil) })
	// Use the shared constructor so this regression also runs independently
	// of CloudGPU's provider registration.
	client := NewOpenAICompat("cloudgpu", "test-key", "https://example.invalid/v1", "").(*openaiClient)
	for _, id := range []string{"kimi-k2.6", "glm-5.2", "unknown-cloudgpu-model"} {
		t.Run(id, func(t *testing.T) {
			for _, maxTokens := range []int{0, 2048} {
				wire, err := client.buildRequest(Request{Model: id, MaxTokens: maxTokens, Reasoning: "high"})
				if err != nil {
					t.Fatal(err)
				}
				want := maxTokens
				if want == 0 {
					want = 4096
				}
				if wire.MaxTokens == nil || *wire.MaxTokens != want {
					t.Errorf("max_tokens = %v, want %d", wire.MaxTokens, want)
				}
				if wire.MaxCompletionTok != nil || wire.ReasoningEffort != "" {
					t.Errorf("borrowed reasoning metadata: max_completion_tokens=%v, reasoning_effort=%q", wire.MaxCompletionTok, wire.ReasoningEffort)
				}
			}
		})
	}
}

func TestCloudGPURequestUsesOwnCatalogMetadata(t *testing.T) {
	SetLiveModels([]Model{{
		Provider: "cloudgpu", ID: "kimi-k2.6", ContextWindow: 65536,
		MaxOutput: 16384, Reasoning: true,
	}})
	t.Cleanup(func() { SetLiveModels(nil) })
	client := NewOpenAICompat("cloudgpu", "test-key", "https://example.invalid/v1", "").(*openaiClient)
	wire, err := client.buildRequest(Request{Model: "kimi-k2.6", Reasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.MaxCompletionTok == nil || *wire.MaxCompletionTok != 16384 || wire.ReasoningEffort != "high" || wire.MaxTokens != nil {
		t.Errorf("own catalog metadata not respected: %+v", wire)
	}
}
