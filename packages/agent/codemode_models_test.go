package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
)

func TestCodemodeLocalClassifierCredentialCancellation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("LLAMA_API_KEY", "")
	t.Setenv("ZOT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(home, "command-ran")
	credentials := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		provider.LlamaCPPProviderID: {
			BaseURL: "http://127.0.0.1:1",
			APIKeyCommand: &auth.APIKeyCommand{
				Program: os.Args[0],
				Args:    []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker},
			},
		},
	}}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	previous := provider.ModelsForProvider(provider.LlamaCPPProviderID)
	provider.SetManagedModelsForProvider(provider.LlamaCPPProviderID, []provider.Model{{Provider: provider.LlamaCPPProviderID, ID: "cancel-test"}})
	t.Cleanup(func() { provider.SetManagedModelsForProvider(provider.LlamaCPPProviderID, previous) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	caller := newCodemodeModelCaller("openrouter", "synthetic-key", "")
	_, _, err = caller(ctx, "classify", json.RawMessage(`[{"provider":"llama.cpp","id":"cancel-test"},{"state":{},"questions":{"q":{"type":"bool","instructions":"test","criteria":{"true":"yes","false":"no"}}}}]`))
	if err == nil {
		t.Fatal("cancelled local classifier setup succeeded")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("credential command ran despite cancellation: %v", err)
	}
}
