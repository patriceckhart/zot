package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
)

func TestModelVisibilityPolicy(t *testing.T) {
	models := []provider.Model{
		{Provider: "google-vertex", ID: "vertex"},
		{Provider: "openai", ID: "confirmed"},
		{Provider: "openai", ID: "guess", Speculative: true},
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		want int
	}{
		{"defaults", Config{}, 3},
		{"provider alias", Config{DisabledProviders: []string{" VERTEX "}}, 2},
		{"speculative", Config{HideSpeculativeModels: true}, 2},
		{"both", Config{DisabledProviders: []string{"google-vertex"}, HideSpeculativeModels: true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.visibleModels(models)
			if len(got) != tc.want {
				t.Fatalf("visible = %+v, want %d models", got, tc.want)
			}
			if len(models) != 3 || !models[2].Speculative {
				t.Fatal("visibility filtering mutated catalog")
			}
		})
	}
}

func TestDisabledProviderSuppressesADC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", home)
	for _, key := range []string{"GOOGLE_CLOUD_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_VERTEX_API_KEY"} {
		t.Setenv(key, "")
	}
	path, err := auth.GoogleApplicationDefaultCredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !CredentialAvailable("google-vertex") {
		t.Fatal("test ADC credential is not available")
	}
	if err := SaveConfig(Config{DisabledProviders: []string{"vertex", "ollama"}}); err != nil {
		t.Fatal(err)
	}
	if CredentialAvailable("google-vertex") {
		t.Fatal("disabled provider still has an available credential")
	}
	providers := modelPickerProviders()
	if slices.Contains(providers, "google-vertex") || slices.Contains(providers, "ollama") {
		t.Fatalf("disabled providers in picker: %v", providers)
	}
	for _, name := range []string{"google-vertex", "vertex", "gcp-vertex"} {
		_, _, err := ResolveCredential(name, "explicit-test-key")
		if err == nil || !strings.Contains(err.Error(), "disabled_providers") {
			t.Fatalf("explicit credential for %s: %v", name, err)
		}
	}
}

func TestDisabledProviderRejectsAllResolutionPaths(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	cfg := Config{Provider: "google-vertex", Model: "saved-selection", DisabledProviders: []string{"google-vertex", "ollama", "removed-provider"}}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	ValidateAndRepairConfig()
	for _, args := range []Args{
		{},
		{Provider: "vertex", APIKey: "explicit-test-key"},
		{Provider: "removed-provider"},
		{Provider: "google-vertex", inheritedCredential: "inherited-test-key"},
		{Provider: "ollama", Model: "local-model", BaseURL: "http://localhost:11434"},
	} {
		for _, requireCred := range []bool{false, true} {
			if _, err := Resolve(args, requireCred); err == nil || !strings.Contains(err.Error(), "disabled_providers") {
				t.Fatalf("Resolve(%+v, %v): %v", args, requireCred, err)
			}
		}
	}
	after, err := os.ReadFile(ConfigPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("disabled selection was rewritten: %v", err)
	}
}

func TestDisabledProvidersSkipCredentialCommandsAndDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	t.Setenv("BLOCKED_CUSTOM_API_KEY", "")
	t.Setenv("ZOT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(home, "command-ran")
	var requests atomic.Int32
	server := newModelsServer(t, &requests, "", "discovered")
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"blocked-custom":{"baseUrl":%q,"discover":true,"auth":"none","models":[{"id":"pinned"}]}}}`, server.URL+"/v1"))
	command := &auth.APIKeyCommand{Program: os.Args[0], Args: []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker}}
	creds := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{}}
	for _, name := range []string{"blocked-custom", provider.LMStudioProviderID, provider.LlamaCPPProviderID} {
		creds.AdditionalAPIKeyCreds[name] = auth.ProviderCreds{APIKeyCommand: command, BaseURL: server.URL}
	}
	encoded, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	disabled := append(append([]string(nil), knownProviders...), "blocked-custom")
	if err := SaveConfig(Config{DisabledProviders: disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Args{Provider: "blocked-custom", Model: "pinned"}, true); err == nil {
		t.Fatal("keyless custom provider bypassed disabling")
	}
	if _, _, err := ResolveCredential("blocked-custom", ""); err == nil {
		t.Fatal("disabled provider resolved command credential")
	}
	if _, _, err := ResolveLlamaCPPConfig(); err == nil {
		t.Fatal("disabled llama.cpp resolved configuration")
	}
	if err := RefreshLMStudioModels(context.Background()); err == nil {
		t.Fatal("disabled LM Studio refreshed models")
	}
	if err := RefreshCustomProviderModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, fmt.Errorf("unexpected discovery request")
	})
	refreshModels()
	refreshCopilotModelAvailability()
	t.Cleanup(func() { provider.SetModelAvailability("github-copilot", nil) })
	if requests.Load() != 0 {
		t.Fatalf("disabled discovery made %d requests", requests.Load())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("disabled credential command ran: %v", err)
	}
}

func TestHiddenSpeculativeModelsRemainResolvableAndPromoteLive(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	var model provider.Model
	for _, m := range provider.Catalog {
		if m.Speculative && m.Provider == "anthropic" {
			model = m
			break
		}
	}
	if model.ID == "" {
		t.Fatal("test requires an Anthropic speculative catalog entry")
	}
	cfg := Config{Provider: model.Provider, Model: model.ID, HideSpeculativeModels: true}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	for _, m := range visibleModelCatalog() {
		if m.Speculative {
			t.Fatalf("speculative model still visible: %s/%s", m.Provider, m.ID)
		}
	}
	ValidateAndRepairConfig()
	resolved, err := Resolve(Args{APIKey: "test-key"}, true)
	if err != nil || resolved.Model != model.ID {
		t.Fatalf("saved speculative selection: model = %q, error = %v", resolved.Model, err)
	}
	resolved, err = Resolve(Args{Provider: model.Provider, Model: model.ID, APIKey: "test-key"}, true)
	if err != nil || resolved.Model != model.ID {
		t.Fatalf("explicit speculative selection: model = %q, error = %v", resolved.Model, err)
	}
	provider.SetLiveModels([]provider.Model{{Provider: model.Provider, ID: model.ID}})
	if !slices.ContainsFunc(visibleModelCatalog(), func(m provider.Model) bool { return m.Provider == model.Provider && m.ID == model.ID && !m.Speculative }) {
		t.Fatal("live discovery did not make the model visible")
	}
}

func TestJSONCatalogHidesSpeculativeModels(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "")
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	var disabled []string
	for _, name := range knownProviders {
		if name != "anthropic" {
			disabled = append(disabled, name)
		}
	}
	if err := SaveConfig(Config{DisabledProviders: disabled, HideSpeculativeModels: true}); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if err := printModelCatalogJSONContext(context.Background(), &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	var entries []catalogEntry
	if err := json.Unmarshal(output.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || diagnostics.Len() != 0 {
		t.Fatalf("missing enabled models or unexpected diagnostics: %s", &diagnostics)
	}
	for _, entry := range entries {
		if entry.Provider != "anthropic" || entry.Source == "speculative" {
			t.Fatalf("unfiltered entry: %+v", entry)
		}
	}
}

func TestModelListingCLIRespectsVisibility(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	if err := SaveConfig(Config{DisabledProviders: append([]string(nil), knownProviders...), HideSpeculativeModels: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(UserModelsPath(), []byte(`{"providers":{"listing-visible":{"baseUrl":"http://localhost:1/v1","auth":"none","models":[{"id":"visible-model"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--list-models", "--list-models-json"} {
		t.Run(flag, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunHelpHelperProcess$")
			cmd.Env = append(os.Environ(), "ZOT_HELP_HELPER="+flag)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("listing failed: %v, stderr: %s", err, &stderr)
			}
			if !strings.Contains(stdout.String(), "visible-model") {
				t.Fatalf("listing omitted enabled model: %s", &stdout)
			}
			if strings.Contains(stdout.String(), "anthropic") || strings.Contains(stdout.String(), "speculative") {
				t.Fatalf("listing contains excluded entries: %s", &stdout)
			}
			if flag == "--list-models-json" {
				var entries []catalogEntry
				if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
