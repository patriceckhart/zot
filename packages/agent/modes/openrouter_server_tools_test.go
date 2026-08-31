package modes

import (
	"testing"

	agenttools "github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
)

func TestOpenRouterServerToolsDefaultOff(t *testing.T) {
	agent := core.NewAgent(nil, "openai/gpt-4.1", "", core.Registry{})
	NewInteractive(InteractiveConfig{
		Agent:    agent,
		Provider: "openrouter",
		Model:    "openai/gpt-4.1",
	})
	if _, err := agent.Tools.Get("openrouter:web_search"); err == nil {
		t.Fatal("server tools enabled without explicit opt-in")
	}
	if agent.MaxToolCalls != 0 {
		t.Fatalf("MaxToolCalls = %d; want 0", agent.MaxToolCalls)
	}
}

func TestOpenRouterServerToolsOptInSetsSafeLimit(t *testing.T) {
	enabled := true
	agent := core.NewAgent(nil, "openai/gpt-4.1", "", core.Registry{})
	NewInteractive(InteractiveConfig{
		Agent:                        agent,
		Provider:                     "openrouter",
		Model:                        "openai/gpt-4.1",
		OpenRouterServerToolsEnabled: &enabled,
	})
	if _, err := agent.Tools.Get("openrouter:web_search"); err != nil {
		t.Fatalf("enabled server tool missing: %v", err)
	}
	if agent.MaxToolCalls != agenttools.OpenRouterServerToolCallLimit {
		t.Fatalf("MaxToolCalls = %d; want %d", agent.MaxToolCalls, agenttools.OpenRouterServerToolCallLimit)
	}
}

func TestOpenRouterServerToolsRespectNoTools(t *testing.T) {
	enabled := true
	agent := core.NewAgent(nil, "openai/gpt-4.1", "", core.Registry{})
	NewInteractive(InteractiveConfig{
		Agent:                        agent,
		Provider:                     "openrouter",
		Model:                        "openai/gpt-4.1",
		OpenRouterServerToolsEnabled: &enabled,
		NoTools:                      true,
	})
	if len(agent.Tools) != 0 {
		t.Fatalf("--no-tools registry has %d tools; want none", len(agent.Tools))
	}
	if agent.MaxToolCalls != 0 {
		t.Fatalf("MaxToolCalls = %d; want 0", agent.MaxToolCalls)
	}
}

func TestOpenRouterPresetKeepsPresetToolConfiguration(t *testing.T) {
	enabled := true
	agent := core.NewAgent(nil, "@preset/flash", "", core.Registry{})
	NewInteractive(InteractiveConfig{
		Agent:                        agent,
		Provider:                     "openrouter",
		Model:                        "@preset/flash",
		OpenRouterServerToolsEnabled: &enabled,
	})
	if _, err := agent.Tools.Get("openrouter:web_search"); err == nil {
		t.Fatal("request tools would override tools configured by the preset")
	}
	if agent.MaxToolCalls != 0 {
		t.Fatalf("MaxToolCalls = %d; want preset-owned default", agent.MaxToolCalls)
	}
}
