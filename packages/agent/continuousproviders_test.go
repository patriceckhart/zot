package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
)

func TestContinuousModelPolicyBlocksHostAndKeylessProviders(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	loadTestModelsJSON(t, `{"providers":{"blocked-routing":{"baseUrl":"http://localhost:1/v1","auth":"none","models":[{"id":"pinned"}]},"ollama":{"models":[{"id":"local-model"}]}}}`)
	disabled := append(append([]string(nil), knownProviders...), "blocked-routing")
	if err := SaveConfig(Config{DisabledProviders: disabled, HideSpeculativeModels: true}); err != nil {
		t.Fatal(err)
	}
	host := Resolved{Provider: "openai", Model: "gpt-4o-mini", Credential: "test-key"}
	for _, config := range []continuous.AgentConfig{{}, {Provider: "blocked-routing", Model: "pinned"}} {
		if _, err := continuousInference(context.Background(), host, config); err == nil {
			t.Fatal("disabled host or keyless provider bypassed policy")
		}
	}
	models, err := continuousAvailableModels(context.Background(), host)
	if err != nil || len(models) != 0 {
		t.Fatalf("available disabled models = %+v, error = %v", models, err)
	}
}

func TestContinuousInferenceScopesCredentialsAndEndpoint(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-other")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	host := Resolved{Provider: "openai", Model: "gpt-4o-mini", Credential: "synthetic-host", AuthMethod: "apikey", BaseURL: "http://127.0.0.1:1", InsecureTLS: true}
	other, err := continuousInference(context.Background(), host, continuous.AgentConfig{Provider: "anthropic", Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Credential != "synthetic-other" || other.BaseURL == host.BaseURL || other.InsecureTLS {
		t.Fatal("host credential or endpoint settings leaked to another provider")
	}
	same, err := continuousInference(context.Background(), host, continuous.AgentConfig{Model: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if same.Credential != host.Credential || same.BaseURL != host.BaseURL || !same.InsecureTLS {
		t.Fatal("same-provider override lost host inference settings")
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := continuousInference(context.Background(), host, continuous.AgentConfig{Provider: "anthropic", Model: "claude-sonnet-4-5"}); err == nil {
		t.Fatal("missing credentials accepted")
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := continuousInference(context.Background(), host, continuous.AgentConfig{Provider: "deepseek", Model: "deepseek-v4-pro"}); err == nil {
		t.Fatal("catalog endpoint allowed a remote provider without credentials")
	}
	if _, err := continuousInference(context.Background(), host, continuous.AgentConfig{Provider: "unknown"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := continuousInference(ctx, host, continuous.AgentConfig{}); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestContinuousAvailableModelsDoesNotExecuteCredentialCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ZOT_AGENT_API_KEY_COMMAND_HELPER", "1")
	t.Setenv("DEEPSEEK_API_KEY", "")
	marker := filepath.Join(home, "command-ran")
	credentials := auth.Credentials{DeepSeek: auth.ProviderCreds{APIKeyCommand: &auth.APIKeyCommand{
		Program: os.Args[0], Args: []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker},
	}}}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	previous := provider.ModelsForProvider("ollama")
	provider.SetManagedModelsForProvider("ollama", []provider.Model{{Provider: "ollama", ID: "routing-local"}})
	t.Cleanup(func() { provider.SetManagedModelsForProvider("ollama", previous) })
	models, err := continuousAvailableModels(context.Background(), Resolved{Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, m := range models {
		found[m.Provider] = true
	}
	if !found["openai"] || !found["deepseek"] || !found["ollama"] {
		t.Fatal("host, local, or command-backed provider omitted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("credential command ran during model discovery")
	}
}

func TestContinuousInferenceDoesNotDowngradeFailedCredentialCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	const name = "routing-command"
	loadTestModelsJSON(t, `{"providers":{"routing-command":{"api":"openai","baseUrl":"http://127.0.0.1:1"}}}`)
	creds := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{name: {
		APIKeyCommand: &auth.APIKeyCommand{Program: filepath.Join(home, "missing-command")},
	}}}
	encoded, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := continuousInference(context.Background(), Resolved{Provider: "openai", Model: "gpt-4o-mini", Credential: "synthetic"}, continuous.AgentConfig{Provider: name, Model: "custom-model"}); err == nil {
		t.Fatal("failed credential command silently downgraded to keyless access")
	}
}

func TestContinuousCompactionUsesSelectedModel(t *testing.T) {
	host := Resolved{Provider: "openai", Model: "gpt-4o-mini"}
	config := continuous.AgentConfig{Provider: "anthropic", Model: "claude-sonnet-4-5"}
	policy := continuousCompactionFor(host)(config)
	model, err := provider.FindModel(config.Provider, config.Model)
	if err != nil {
		t.Fatal(err)
	}
	if policy.ContextWindow != model.ContextWindow || policy.ReserveTokens == 0 {
		t.Fatalf("incorrect model policy: %+v", policy)
	}
}
