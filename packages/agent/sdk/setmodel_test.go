package sdk

import (
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestRuntimeSetModelRejectsBusyAgent(t *testing.T) {
	const from, to = "claude-3-5-haiku-20241022", "claude-opus-4-1-20250805"
	r := &Runtime{
		agent:        &core.Agent{Model: from, MaxTokens: 4096},
		provider:     "anthropic",
		model:        from,
		activeCancel: func() {}, // Prompt and Compact both hold this until complete.
	}
	if err := r.SetModel(to); !errors.Is(err, ErrBusy) {
		t.Fatalf("SetModel during active turn = %v, want ErrBusy", err)
	}
	if r.model != from || r.agent.Model != from || r.agent.MaxTokens != 4096 {
		t.Fatalf("busy SetModel changed model or limit: runtime=%q agent=%q max=%d", r.model, r.agent.Model, r.agent.MaxTokens)
	}

	r.activeCancel = nil
	if err := r.SetModel(to); err != nil {
		t.Fatalf("SetModel after turn: %v", err)
	}
	if r.model != to || r.agent.Model != to {
		t.Fatalf("SetModel after turn: runtime=%q agent=%q, want %q", r.model, r.agent.Model, to)
	}
}

func TestRuntimeSetModelRejectsDisabledProvider(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := agent.SaveConfig(agent.Config{DisabledProviders: []string{"anthropic"}}); err != nil {
		t.Fatal(err)
	}
	const from, to = "claude-3-5-haiku-20241022", "claude-opus-4-1-20250805"
	r := &Runtime{agent: &core.Agent{Model: from, MaxTokens: 4096}, provider: "anthropic", model: from}
	if err := r.SetModel(to); err == nil || !strings.Contains(err.Error(), "disabled_providers") {
		t.Fatalf("SetModel: %v", err)
	}
	if r.model != from || r.agent.Model != from || r.agent.MaxTokens != 4096 {
		t.Fatal("disabled SetModel changed the active model")
	}
}

func TestRuntimeSetModelUpdatesMaxTokens(t *testing.T) {
	const to = "claude-opus-4-1-20250805"
	want, err := provider.FindModel("anthropic", to)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{agent: &core.Agent{MaxTokens: 4096}, provider: "anthropic"}
	if err := r.SetModel(to); err != nil {
		t.Fatal(err)
	}
	if r.agent.MaxTokens != want.MaxOutput {
		t.Fatalf("MaxTokens = %d, want %d", r.agent.MaxTokens, want.MaxOutput)
	}
}
