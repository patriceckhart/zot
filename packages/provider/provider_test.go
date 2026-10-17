package provider

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEParse(t *testing.T) {
	r := strings.NewReader("event: foo\ndata: {\"a\":1}\n\ndata: hello\ndata: world\n\n")
	ch := make(chan sseEvent, 4)
	go readSSE(r, ch)

	e := <-ch
	if e.Event != "foo" || e.Data != `{"a":1}` {
		t.Fatalf("event 1: %+v", e)
	}
	e = <-ch
	if e.Event != "" || e.Data != "hello\nworld" {
		t.Fatalf("event 2: %+v", e)
	}
	if _, ok := <-ch; ok {
		t.Fatalf("channel not closed")
	}
}

func TestModelCatalog(t *testing.T) {
	if len(Catalog) == 0 {
		t.Fatal("empty catalog")
	}
	if _, err := FindModel("anthropic", "claude-sonnet-4-5"); err != nil {
		t.Fatal(err)
	}
	if _, err := FindModel("openai", "gpt-5"); err != nil {
		t.Fatal(err)
	}
	if _, err := FindModel("", "nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestVertexGeminiFlashCatalogLimits(t *testing.T) {
	for _, id := range []string{"gemini-3.1-flash-lite", "gemini-3.5-flash"} {
		m, err := FindModel("google-vertex", id)
		if err != nil {
			t.Fatal(err)
		}
		if m.ContextWindow != 1048576 || m.MaxOutput != 65535 || !m.Reasoning {
			t.Errorf("%s metadata = %+v", id, m)
		}
	}
}

func TestComputeCost(t *testing.T) {
	m, _ := FindModel("anthropic", "claude-sonnet-4-5")
	cost := ComputeCost(m, Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	want := m.PriceInput + m.PriceOutput
	if cost != want {
		t.Fatalf("cost=%v want=%v", cost, want)
	}
}

func TestComputeCostInputTier(t *testing.T) {
	m := Model{
		PriceInput: 1, PriceOutput: 2, PriceCacheRead: 0.1,
		PriceTierInputTokens: 100,
		PriceInputAbove:      3, PriceOutputAbove: 4, PriceCacheReadAbove: 0.2,
	}
	below := ComputeCost(m, Usage{InputTokens: 50, OutputTokens: 10, CacheReadTokens: 50})
	wantBelow := (50.0*1 + 10*2 + 50*0.1) / 1_000_000
	if math.Abs(below-wantBelow) > 1e-12 {
		t.Fatalf("below-tier cost=%v want=%v", below, wantBelow)
	}

	above := ComputeCost(m, Usage{InputTokens: 51, OutputTokens: 10, CacheReadTokens: 50})
	wantAbove := (51.0*3 + 10*4 + 50*0.2) / 1_000_000
	if math.Abs(above-wantAbove) > 1e-12 {
		t.Fatalf("above-tier cost=%v want=%v", above, wantAbove)
	}
}

func TestAnthropicErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"auth","message":"bad key"}}`))
	}))
	defer srv.Close()

	c := NewAnthropic("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "claude-sonnet-4-5"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 err, got %v", err)
	}
}

func TestAnthropicOAuthUsesCurrentClaudeCodeVersion(t *testing.T) {
	var userAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("user-agent")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewAnthropicOAuth("test-token", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "claude-opus-5-5"})
	if err == nil {
		t.Fatal("expected test server error")
	}
	if want := "claude-cli/2.1.280"; userAgent != want {
		t.Fatalf("user-agent = %q, want %q", userAgent, want)
	}
}

func TestOpenAIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want 400 err, got %v", err)
	}
}

func TestMiniMaxM3CatalogAndRequest(t *testing.T) {
	for _, provider := range []string{"minimax", "minimax-cn"} {
		m, err := FindModel(provider, "MiniMax-M3")
		if err != nil {
			t.Fatal(err)
		}
		if m.ContextWindow != 1000000 || m.MaxOutput != 131072 || !m.Reasoning || !m.AdaptiveThinkingCompat {
			t.Errorf("%s metadata = %+v", provider, m)
		}
		if m.PriceInput != 0.3 || m.PriceOutput != 1.2 || m.PriceCacheRead != 0.06 || m.PriceTierInputTokens != 512000 || m.PriceInputAbove != 0.6 || m.PriceOutputAbove != 2.4 || m.PriceCacheReadAbove != 0.12 {
			t.Errorf("%s pricing = %+v", provider, m)
		}
	}

	c := NewAnthropicCompat("minimax", "x", "https://api.minimax.io/anthropic").(*anthropicClient)
	temp := float32(0.7)
	wire, err := c.buildRequest(Request{
		Model:       "MiniMax-M3",
		Reasoning:   "high",
		Temperature: &temp,
		Messages:    []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Thinking == nil || wire.Thinking.Type != "adaptive" || wire.Thinking.BudgetTokens != 0 {
		t.Fatalf("want adaptive thinking without a budget, got %+v", wire.Thinking)
	}
	if wire.OutputConfig != nil {
		t.Fatalf("MiniMax-M3 must not send output_config, got %+v", wire.OutputConfig)
	}
	if wire.Temperature == nil || *wire.Temperature != temp {
		t.Fatalf("MiniMax-M3 should keep temperature, got %v", wire.Temperature)
	}
}

func TestAnthropicAdaptiveThinking(t *testing.T) {
	c := NewAnthropic("x", "").(*anthropicClient)
	temp := float32(0.7)

	// Opus 5 -> adaptive thinking, effort set, no budget, no temperature.
	wire, err := c.buildRequest(Request{
		Model:       "claude-opus-5",
		Reasoning:   "high",
		Temperature: &temp,
		Messages:    []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Thinking == nil || wire.Thinking.Type != "adaptive" {
		t.Fatalf("want adaptive thinking, got %+v", wire.Thinking)
	}
	if wire.Thinking.BudgetTokens != 0 {
		t.Fatalf("adaptive must not send budget_tokens, got %d", wire.Thinking.BudgetTokens)
	}
	if wire.OutputConfig == nil || wire.OutputConfig.Effort != "high" {
		t.Fatalf("want effort=high, got %+v", wire.OutputConfig)
	}
	if wire.Temperature != nil {
		t.Fatalf("adaptive must drop temperature, got %v", *wire.Temperature)
	}

	// maximum -> xhigh effort.
	wire, err = c.buildRequest(Request{
		Model:     "claude-opus-5",
		Reasoning: "maximum",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.OutputConfig == nil || wire.OutputConfig.Effort != "xhigh" {
		t.Fatalf("want effort=xhigh, got %+v", wire.OutputConfig)
	}

	// max is a separate native tier above xhigh on adaptive models.
	wire, err = c.buildRequest(Request{
		Model:     "claude-opus-5",
		Reasoning: "max",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.OutputConfig == nil || wire.OutputConfig.Effort != "max" {
		t.Fatalf("want effort=max, got %+v", wire.OutputConfig)
	}

	// Sonnet 5 -> adaptive thinking, effort set, no budget, no temperature.
	wire, err = c.buildRequest(Request{
		Model:       "claude-sonnet-5",
		Reasoning:   "high",
		Temperature: &temp,
		Messages:    []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Thinking == nil || wire.Thinking.Type != "adaptive" {
		t.Fatalf("want adaptive thinking for Sonnet 5, got %+v", wire.Thinking)
	}
	if wire.Thinking.BudgetTokens != 0 {
		t.Fatalf("Sonnet 5 adaptive must not send budget_tokens, got %d", wire.Thinking.BudgetTokens)
	}
	if wire.OutputConfig == nil || wire.OutputConfig.Effort != "high" {
		t.Fatalf("want Sonnet 5 effort=high, got %+v", wire.OutputConfig)
	}
	if wire.Temperature != nil {
		t.Fatalf("Sonnet 5 adaptive must drop temperature, got %v", *wire.Temperature)
	}

	// Opus 4.5 -> budget-based thinking, no output_config, temperature kept.
	wire, err = c.buildRequest(Request{
		Model:       "claude-opus-4-5",
		Reasoning:   "high",
		Temperature: &temp,
		Messages:    []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Thinking == nil || wire.Thinking.Type != "enabled" || wire.Thinking.BudgetTokens <= 0 {
		t.Fatalf("want budget thinking, got %+v", wire.Thinking)
	}
	if wire.OutputConfig != nil {
		t.Fatalf("budget models must not send output_config, got %+v", wire.OutputConfig)
	}
	if wire.Temperature == nil || *wire.Temperature != temp {
		t.Fatalf("budget model should keep temperature, got %v", wire.Temperature)
	}
}

func TestAnthropicBuildRequestStripsAssistantImages(t *testing.T) {
	c := NewAnthropic("x", "").(*anthropicClient)
	wire, err := c.buildRequest(Request{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "make an image"}}},
			{Role: RoleAssistant, Content: []Content{
				TextBlock{Text: "done"},
				ImageBlock{MimeType: "image/png", Data: []byte("png")},
				TextBlock{Text: "Saved image: `zot-gemini-image-x.png`"},
			}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("messages=%d", len(wire.Messages))
	}
	assistant := wire.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("role=%q", assistant.Role)
	}
	if len(assistant.Content) != 2 {
		t.Fatalf("assistant content=%+v", assistant.Content)
	}
	for _, b := range assistant.Content {
		if _, ok := b.(anthImageBlock); ok {
			t.Fatalf("assistant image block was not stripped: %+v", assistant.Content)
		}
	}
}

func TestRepairOrphanedToolResultsPreservesRepeatedToolCallPairs(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "reused", Name: "read"}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "reused", Content: []Content{TextBlock{Text: "first"}}}}},
		{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "reused", Name: "read"}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "reused", Content: []Content{TextBlock{Text: "second"}}}}},
	}

	got := RepairOrphanedToolResults(msgs)
	if len(got) != len(msgs) {
		t.Fatalf("messages=%d, want %d valid repeated-ID pairs", len(got), len(msgs))
	}
	for _, index := range []int{1, 3} {
		if len(got[index].Content) != 1 {
			t.Fatalf("message %d content=%d, want 1 tool result", index, len(got[index].Content))
		}
		if _, ok := got[index].Content[0].(ToolResultBlock); !ok {
			t.Fatalf("message %d content type=%T, want ToolResultBlock", index, got[index].Content[0])
		}
	}
}

func TestAnthropicBuildRequestDeduplicatesToolResults(t *testing.T) {
	c := NewAnthropic("x", "").(*anthropicClient)
	wire, err := c.buildRequest(Request{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "read the file"}}},
			{Role: RoleAssistant, Content: []Content{
				ToolCallBlock{ID: "tool-1", Name: "read"},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "tool-1", Content: []Content{TextBlock{Text: "first result"}}},
			}},
			{Role: RoleTool, Content: []Content{
				ToolResultBlock{CallID: "tool-1", Content: []Content{TextBlock{Text: "duplicate result"}}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("messages=%d, want 3 after dropping duplicate result message", len(wire.Messages))
	}
	blocks := wire.Messages[2].Content
	if len(blocks) != 1 {
		t.Fatalf("tool result blocks=%d, want 1", len(blocks))
	}
	result, ok := blocks[0].(anthToolResultBlock)
	if !ok {
		t.Fatalf("tool result block type=%T", blocks[0])
	}
	if string(result.Content) != `"first result"` {
		t.Fatalf("tool result content=%s", result.Content)
	}
}

func TestAnthropicBuildRequestPreservesRepeatedToolCallPairs(t *testing.T) {
	c := NewAnthropic("x", "").(*anthropicClient)
	wire, err := c.buildRequest(Request{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "reused", Name: "read"}}},
			{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "reused", Content: []Content{TextBlock{Text: "first"}}}}},
			{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "reused", Name: "read"}}},
			{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "reused", Content: []Content{TextBlock{Text: "second"}}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 4 {
		t.Fatalf("messages=%d, want both repeated-ID tool result pairs", len(wire.Messages))
	}
	for _, index := range []int{1, 3} {
		if len(wire.Messages[index].Content) != 1 {
			t.Fatalf("message %d content=%d, want 1 tool result", index, len(wire.Messages[index].Content))
		}
		if _, ok := wire.Messages[index].Content[0].(anthToolResultBlock); !ok {
			t.Fatalf("message %d content type=%T, want anthToolResultBlock", index, wire.Messages[index].Content[0])
		}
	}
}

func TestAnthropicStreamHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if fl != nil {
				fl.Flush()
			}
		}
		write("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		write("event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		write("event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		write("event: content_block_stop\ndata: {\"index\":0}\n\n")
		write("event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		write("event: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	c := NewAnthropic("x", srv.URL)
	evs, err := c.Stream(context.Background(), Request{Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	var gotText string
	var done EventDone
	for ev := range evs {
		switch e := ev.(type) {
		case EventTextDelta:
			gotText += e.Delta
		case EventDone:
			done = e
		}
	}
	if gotText != "hi" {
		t.Fatalf("text=%q", gotText)
	}
	if done.Stop != StopEnd {
		t.Fatalf("stop=%v", done.Stop)
	}
}

func TestClaudeOpus5Catalog(t *testing.T) {
	m, err := FindModel("anthropic", "claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "Claude Opus 5" || m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
		t.Fatalf("unexpected Opus 5 model: %+v", m)
	}
	if m.PriceInput != 5 || m.PriceOutput != 25 || m.PriceCacheRead != 0.5 || m.PriceCacheWrite != 6.25 {
		t.Fatalf("unexpected Opus 5 pricing: %+v", m)
	}
}

func TestClaudeSonnet5Catalog(t *testing.T) {
	m, err := FindModel("anthropic", "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "Claude Sonnet 5" || m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking {
		t.Fatalf("unexpected Sonnet 5 model: %+v", m)
	}
	if m.PriceInput != 2 || m.PriceOutput != 10 || m.PriceCacheRead != 0.2 || m.PriceCacheWrite != 2.5 {
		t.Fatalf("unexpected Sonnet 5 pricing: %+v", m)
	}
}

func TestClaudeFable5Catalog(t *testing.T) {
	m, err := FindModel("anthropic", "claude-fable-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "Claude Fable 5" || m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
		t.Fatalf("unexpected Fable 5 model: %+v", m)
	}
	if m.PriceInput != 10 || m.PriceOutput != 50 || m.PriceCacheRead != 1 || m.PriceCacheWrite != 12.5 {
		t.Fatalf("unexpected Fable 5 pricing: %+v", m)
	}
}

func TestClaudeFable51Catalog(t *testing.T) {
	m, err := FindModel("anthropic", "claude-fable-5-1")
	if err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "Claude Fable 5.1" || m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
		t.Fatalf("unexpected Fable 5.1 model: %+v", m)
	}
	if m.PriceInput != 10 || m.PriceOutput != 50 || m.PriceCacheRead != 0.25 || m.PriceCacheWrite != 12.5 {
		t.Fatalf("unexpected Fable 5.1 pricing: %+v", m)
	}

	wire, err := NewAnthropic("x", "").(*anthropicClient).buildRequest(Request{
		Model:     m.ID,
		Reasoning: "max",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Thinking == nil || wire.Thinking.Type != "adaptive" || wire.Thinking.BudgetTokens != 0 {
		t.Fatalf("unexpected Fable 5.1 thinking config: %+v", wire.Thinking)
	}
	if wire.OutputConfig == nil || wire.OutputConfig.Effort != "max" {
		t.Fatalf("unexpected Fable 5.1 output config: %+v", wire.OutputConfig)
	}
}

func TestGondolaSeedCatalog(t *testing.T) {
	kimi, err := FindModel("gondola", "kimi-k3")
	if err != nil {
		t.Fatal(err)
	}
	if kimi.ContextWindow != 1000000 || kimi.MaxOutput != 131072 || !kimi.Reasoning || kimi.AdaptiveThinking {
		t.Fatalf("unexpected Kimi K3 model: %+v", kimi)
	}
	if kimi.PriceInput != 1.1475 || kimi.PriceOutput != 5.7375 || kimi.PriceCacheRead != 0.11475 {
		t.Fatalf("unexpected Kimi K3 pricing: %+v", kimi)
	}

	opus, err := FindModel("gondola", "claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	if !opus.AdaptiveThinking || opus.PriceInput != 1.836 || opus.PriceOutput != 9.18 || opus.PriceCacheRead != 0.1836 || opus.PriceCacheWrite != 2.295 {
		t.Fatalf("unexpected Claude Opus 5 model: %+v", opus)
	}
}

func TestYoloAutoSeedCatalog(t *testing.T) {
	// The free-plan default must resolve without a live refresh.
	flash, err := FindModel("yolo-auto", "qwen3.8-flash")
	if err != nil {
		t.Fatal(err)
	}
	if flash.ContextWindow != yoloAutoContextWindow || flash.MaxOutput != yoloAutoMaxOutput ||
		!flash.Reasoning || flash.BaseURL != yoloAutoDefaultBaseURL {
		t.Fatalf("unexpected Qwen model: %+v", flash)
	}
	yolo, err := FindModel("yolo-auto", "yolo")
	if err != nil {
		t.Fatal(err)
	}
	if yolo.ContextWindow != yoloAutoContextWindow || yolo.MaxOutput != yoloAutoMaxOutput ||
		!yolo.Reasoning || yolo.BaseURL != yoloAutoDefaultBaseURL {
		t.Fatalf("unexpected Yolo model: %+v", yolo)
	}

	small, err := FindModel("yolo-auto", "yolo-small")
	if err != nil {
		t.Fatal(err)
	}
	if small.BaseURL != yoloAutoDefaultBaseURL || small.Reasoning {
		t.Fatalf("unexpected Yolo Small model: %+v", small)
	}
}

func TestOpenAICompatAnthropicReasoningEffort(t *testing.T) {
	c := NewOpenRouter("token", "").(*openaiClient)
	wire, err := c.buildRequest(Request{
		Model:     "anthropic/claude-opus-4.8",
		Reasoning: "maximum",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ReasoningEffort != "xhigh" {
		t.Fatalf("want xhigh for adaptive Anthropic model over OpenAI-compatible wire, got %q", wire.ReasoningEffort)
	}

	wire, err = c.buildRequest(Request{
		Model:     "anthropic/claude-sonnet-5",
		Reasoning: "maximum",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ReasoningEffort != "xhigh" {
		t.Fatalf("want xhigh for Claude Sonnet 5 over OpenAI-compatible wire, got %q", wire.ReasoningEffort)
	}

	wire, err = c.buildRequest(Request{
		Model:     "gpt-5.1",
		Reasoning: "maximum",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ReasoningEffort != "high" {
		t.Fatalf("want generic OpenAI-compatible maximum clamped to high, got %q", wire.ReasoningEffort)
	}
}

func TestOpenAIBuildRequestSkipsReasoningOnlyAssistantMessages(t *testing.T) {
	c := NewKimi("token", "").(*openaiClient)
	wire, err := c.buildRequest(Request{
		Model: "kimi-for-coding",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "first"}}},
			{Role: RoleAssistant, Content: []Content{ReasoningBlock{Summary: "thinking only"}}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "second"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, msg := range wire.Messages {
		if msg.Role == "assistant" && msg.Content == nil && len(msg.ToolCalls) == 0 {
			t.Fatalf("message %d is empty assistant: %+v", i, msg)
		}
	}
	if got := len(wire.Messages); got != 2 {
		t.Fatalf("messages=%d want 2 after skipping reasoning-only assistant", got)
	}
}

func TestOpenAIStreamHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	evs, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	var gotText string
	var done EventDone
	for ev := range evs {
		switch e := ev.(type) {
		case EventTextDelta:
			gotText += e.Delta
		case EventDone:
			done = e
		}
	}
	if gotText != "hello" {
		t.Fatalf("text=%q", gotText)
	}
	if done.Stop != StopEnd {
		t.Fatalf("stop=%v", done.Stop)
	}
}

func TestDiscoverOpenRouter(t *testing.T) {
	const body = `{"data":[
		{"id":"x/full","pricing":{"prompt":"0.000003","completion":"0.000015"},
		 "context_length":200000,"top_provider":{"max_completion_tokens":64000},
		 "supported_parameters":["reasoning"]},
		{"id":"x/fallback","top_provider":{"context_length":128000}},
		{"id":""}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	models, err := DiscoverOpenRouter(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 { // empty id dropped
		t.Fatalf("want 2 models, got %d", len(models))
	}
	// per-token USD -> per-1M; reasoning from supported_parameters.
	if m := models[0]; m.Provider != "openrouter" || m.ContextWindow != 200000 ||
		m.MaxOutput != 64000 || !m.Reasoning || m.PriceInput != 3 || m.PriceOutput != 15 {
		t.Errorf("model[0]: %+v", m)
	}
	// context falls back to top_provider; no reasoning.
	if m := models[1]; m.ContextWindow != 128000 || m.MaxOutput != 0 || m.Reasoning {
		t.Errorf("model[1]: %+v", m)
	}
}

func TestGPT56CatalogEntries(t *testing.T) {
	cases := []struct {
		provider   string
		id         string
		context    int
		priceInput float64
		priceOut   float64
		cacheRead  float64
		cacheWrite float64
	}{
		{"openai", "gpt-5.6-luna", 272000, 1, 6, 0.1, 1.25},
		{"openai", "gpt-5.6-sol", 272000, 5, 30, 0.5, 6.25},
		{"openai", "gpt-5.6-terra", 272000, 2.5, 15, 0.25, 3.125},
		{"openai-codex", "gpt-5.6-luna", 272000, 1, 6, 0.1, 1.25},
		{"openai-codex", "gpt-5.6-sol", 272000, 5, 30, 0.5, 6.25},
		{"openai-codex", "gpt-5.6-terra", 272000, 2.5, 15, 0.25, 3.125},
		{"azure-openai-responses", "gpt-5.6-luna", 1050000, 1, 6, 0.1, 1.25},
		{"azure-openai-responses", "gpt-5.6-sol", 1050000, 5, 30, 0.5, 6.25},
		{"azure-openai-responses", "gpt-5.6-terra", 1050000, 2.5, 15, 0.25, 3.125},
		{"vercel-ai-gateway", "openai/gpt-5.6-luna", 1050000, 1, 6, 0.1, 1.25},
		{"vercel-ai-gateway", "openai/gpt-5.6-sol", 1050000, 5, 30, 0.5, 6.25},
		{"vercel-ai-gateway", "openai/gpt-5.6-terra", 1050000, 2.5, 15, 0.25, 3.125},
	}
	for _, tc := range cases {
		m, err := FindModel(tc.provider, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if m.ContextWindow != tc.context || m.MaxOutput != 128000 || !m.Reasoning {
			t.Fatalf("%s/%s limits: %+v", tc.provider, tc.id, m)
		}
		if m.PriceInput != tc.priceInput || m.PriceOutput != tc.priceOut || m.PriceCacheRead != tc.cacheRead || m.PriceCacheWrite != tc.cacheWrite {
			t.Fatalf("%s/%s prices: %+v", tc.provider, tc.id, m)
		}
	}
}

// TestOpenCodeGoCatalog pins entries added from the vendor's current Go
// model lineup (https://opencode.ai/docs/go).
func TestOpenCodeGoCatalog(t *testing.T) {
	cases := []struct {
		id        string
		context   int
		maxOut    int
		priceIn   float64
		priceOut  float64
		cacheRead float64
	}{
		{"gpt-5.6-luna", 1050000, 128000, 0.2, 1.2, 0.02},
		{"grok-4.6", 500000, 500000, 2, 6, 0.5},
		{"kimi-k3", 1048576, 131072, 3, 15, 0.3},
	}
	for _, tc := range cases {
		m, err := FindModel("opencode-go", tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if m.ContextWindow != tc.context || m.MaxOutput != tc.maxOut || !m.Reasoning {
			t.Fatalf("opencode-go/%s limits: %+v", tc.id, m)
		}
		if m.PriceInput != tc.priceIn || m.PriceOutput != tc.priceOut || m.PriceCacheRead != tc.cacheRead {
			t.Fatalf("opencode-go/%s prices: %+v", tc.id, m)
		}
		if m.BaseURL != "https://opencode.ai/zen/go/v1" {
			t.Fatalf("opencode-go/%s baseURL: %s", tc.id, m.BaseURL)
		}
	}

	luna, err := FindModel("opencode-go", "gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	if luna.API != APIResponses || luna.PriceCacheWrite != 0.25 {
		t.Fatalf("opencode-go/gpt-5.6-luna API or cache-write price: %+v", luna)
	}
	if luna.PriceTierInputTokens != 272000 || luna.PriceInputAbove != 0.4 || luna.PriceOutputAbove != 1.8 || luna.PriceCacheReadAbove != 0.04 || luna.PriceCacheWriteAbove != 0.5 {
		t.Fatalf("opencode-go/gpt-5.6-luna long-context prices: %+v", luna)
	}
}

// TestOpenAIOmitsZeroMaxTokens guards against sending max_tokens: 0 for
// discovered models that don't advertise an output cap (MaxOutput == 0).
func TestOpenAIOmitsZeroMaxTokens(t *testing.T) {
	SetLiveModels([]Model{
		{Provider: "openrouter", ID: "vendor/no-cap", DisplayName: "No Cap"},
		{Provider: "openrouter", ID: "vendor/reason-no-cap", DisplayName: "Reason No Cap", Reasoning: true},
		{Provider: "openrouter", ID: "vendor/capped", DisplayName: "Capped", MaxOutput: 4096},
	})
	defer SetLiveModels(nil)

	c := NewOpenAI("x", "").(*openaiClient)

	got, err := c.buildRequest(Request{Model: "vendor/no-cap"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens != nil {
		t.Errorf("expected max_tokens omitted, got %d", *got.MaxTokens)
	}

	got, err = c.buildRequest(Request{Model: "vendor/reason-no-cap"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxCompletionTok != nil {
		t.Errorf("expected max_completion_tokens omitted, got %d", *got.MaxCompletionTok)
	}

	got, err = c.buildRequest(Request{Model: "vendor/capped"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 4096 {
		t.Errorf("expected max_tokens 4096, got %v", got.MaxTokens)
	}
}
