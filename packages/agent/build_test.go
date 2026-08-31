package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestReadAgentsContextLoadsGlobalAndAncestors(t *testing.T) {
	root := t.TempDir()
	zotHome := filepath.Join(root, "zot-home")
	project := filepath.Join(root, "repo")
	nested := filepath.Join(project, "packages", "app")
	if err := os.MkdirAll(zotHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zotHome, "AGENTS.md"), []byte("global rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte("repo rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "AGENTS.md"), []byte("app rule"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := loadAgentsContext(nested, zotHome)
	if len(files) != 3 {
		t.Fatalf("loaded %d context files, want 3: %#v", len(files), files)
	}
	wantPaths := []string{
		filepath.Join(zotHome, "AGENTS.md"),
		filepath.Join(project, "AGENTS.md"),
		filepath.Join(nested, "AGENTS.md"),
	}
	for idx, want := range wantPaths {
		if files[idx].Path != want {
			t.Fatalf("context file %d path = %q, want %q", idx, files[idx].Path, want)
		}
	}

	got := formatAgentsContext(files)
	for _, want := range []string{"global rule", "repo rule", "app rule"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatAgentsContext missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "global rule") > strings.Index(got, "repo rule") || strings.Index(got, "repo rule") > strings.Index(got, "app rule") {
		t.Fatalf("AGENTS.md files loaded in wrong order:\n%s", got)
	}
}

func TestReadAgentsContextMissingFilesIsEmpty(t *testing.T) {
	files := loadAgentsContext(t.TempDir(), t.TempDir())
	if len(files) != 0 {
		t.Fatalf("expected no context files, got %#v", files)
	}
	if got := formatAgentsContext(files); got != "" {
		t.Fatalf("expected no context, got %q", got)
	}
}

func TestResolveNoContextFilesSkipsAgentsInstructions(t *testing.T) {
	zotHome := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZOT_HOME", zotHome)
	t.Setenv("OPENAI_API_KEY", "test-key")
	if err := os.WriteFile(filepath.Join(zotHome, "AGENTS.md"), []byte("global rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("project rule"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{
		Provider:       "openai",
		Model:          "gpt-5",
		CWD:            cwd,
		NoContextFiles: true,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ContextFiles) != 0 {
		t.Fatalf("ContextFiles = %#v, want empty", r.ContextFiles)
	}
	if strings.Contains(r.SystemPrompt, "global rule") || strings.Contains(r.SystemPrompt, "project rule") {
		t.Fatalf("system prompt contains disabled context files:\n%s", r.SystemPrompt)
	}
}

func TestResolveExplicitEmptySystemPromptOverridesPersistentPrompt(t *testing.T) {
	zotHome := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZOT_HOME", zotHome)
	t.Setenv("OPENAI_API_KEY", "test-key")
	if err := os.WriteFile(filepath.Join(zotHome, "SYSTEM.md"), []byte("persistent instructions"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{
		Provider:        "openai",
		Model:           "gpt-5",
		CWD:             cwd,
		SystemPromptSet: true,
		SystemPrompt:    "",
		NoContextFiles:  true,
		NoSkill:         true,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"persistent instructions", defaultIdentity, "Zot's own docs are installed under"} {
		if strings.Contains(r.SystemPrompt, unwanted) {
			t.Fatalf("explicit empty system prompt unexpectedly contains %q:\n%s", unwanted, r.SystemPrompt)
		}
	}
}

// TestResolveFallsBackWhenConfiguredModelIsGone reproduces the
// startup failure caught by the user's screenshot: the persisted
// config.json points at a model id that's no longer in the active
// catalogue (because they edited models.json or zot's bundled
// catalogue changed). Resolve must NOT error — strands the user
// with no way to fix it from the TUI — and should repair the config
// so the next launch is silent.
func TestResolveFallsBackWhenConfiguredModelIsGone(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	// Persist a stale model id.
	stale := "gpt-5.5-pro-not-real"
	if err := SaveConfig(Config{Provider: "openai", Model: stale}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, false)
	if err != nil {
		t.Fatalf("Resolve refused to launch with stale model: %v", err)
	}
	if r.Model == stale {
		t.Fatalf("Resolve kept stale model %q", r.Model)
	}
	if r.Provider != "openai" {
		t.Errorf("provider drifted: got %q; want openai", r.Provider)
	}

	// Config on disk should now hold the fallback so subsequent
	// launches don't repeat the warning.
	cfg, _ := LoadConfig()
	if cfg.Model == stale {
		t.Errorf("config.json still pins the stale model %q", cfg.Model)
	}
	if cfg.Model == "" {
		t.Errorf("config.json was emptied; expected the fallback model id")
	}
}

func TestResolveAppliesJailByDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config Config
		want   bool
	}{
		{name: "missing defaults to unlocked", config: Config{Provider: "openai", Model: "gpt-5"}},
		{name: "enabled starts locked", config: Config{Provider: "openai", Model: "gpt-5", JailByDefault: boolPtr(true)}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			t.Setenv("OPENAI_API_KEY", "test-key")
			if err := SaveConfig(tc.config); err != nil {
				t.Fatal(err)
			}

			r, err := Resolve(Args{}, false)
			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}
			if got := r.Sandbox.Locked(); got != tc.want {
				t.Fatalf("sandbox locked = %v, want %v", got, tc.want)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

// TestResolveExplicitFlagStaleDoesNotRepairConfig confirms the
// repair-on-disk happens ONLY when the stale id came from the
// persisted config. If the user passed --model X explicitly and X is
// unknown, we still fall back, but we don't touch their config.
func TestResolveExplicitFlagStaleDoesNotRepairConfig(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	good := "gpt-5"
	if err := SaveConfig(Config{Provider: "openai", Model: good}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{Model: "gpt-totally-fake"}, false)
	if err != nil {
		t.Fatalf("Resolve errored on unknown --model: %v", err)
	}
	if r.Model == "gpt-totally-fake" {
		t.Errorf("Resolve kept the bogus --model value")
	}
	cfg, _ := LoadConfig()
	if cfg.Model != good {
		t.Errorf("config.json was clobbered (was %q; now %q)", good, cfg.Model)
	}
}

func TestNewAgentInjectsOpenRouterServerToolsWhenSettingOn(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	if err := SaveConfig(Config{Provider: "openrouter", Model: "openai/gpt-4.1"}); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(Args{NoSkill: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	ag := r.NewAgent()
	for _, name := range []string{
		"openrouter:web_search",
		"openrouter:web_fetch",
		"openrouter:advisor",
		"openrouter:subagent",
		"openrouter:fusion",
		"openrouter:image_generation",
		"openrouter:datetime",
		"openrouter:experimental__search_models",
	} {
		if _, err := ag.Tools.Get(name); err != nil {
			t.Errorf("missing advertised server tool %q: %v", name, err)
		}
	}
	for _, name := range []string{"openrouter:shell", "openrouter:bash", "openrouter:apply_patch", "openrouter:tool_search"} {
		if _, err := ag.Tools.Get(name); err == nil {
			t.Errorf("chat-completions-incompatible tool %q should not be advertised", name)
		}
	}
}

func TestNewAgentOmitsOpenRouterServerToolsWhenSettingOff(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	off := false
	if err := SaveConfig(Config{Provider: "openrouter", Model: "openai/gpt-4.1", OpenRouterServerToolsEnabled: &off}); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(Args{NoSkill: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	ag := r.NewAgent()
	if _, err := ag.Tools.Get("openrouter:web_search"); err == nil {
		t.Fatal("web_search advertised while setting is off")
	}
}

func TestResolveOpenRouterPreservesSavedRoutedModelID(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	want := "deepseek/deepseek-v4-flash"
	if err := SaveConfig(Config{Provider: "openrouter", Model: want}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.Provider != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", r.Provider)
	}
	if r.Model != want {
		t.Fatalf("model = %q, want %q", r.Model, want)
	}
	if r.MaxOutput != 64000 {
		t.Fatalf("MaxOutput = %d, want synthetic gateway default 64000", r.MaxOutput)
	}
}

func TestResolveGatewayPlainUnknownModelFallsBack(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	stale := "not-a-routed-model"
	if err := SaveConfig(Config{Provider: "openrouter", Model: stale}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.Provider != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", r.Provider)
	}
	if r.Model == stale || r.Model == "" {
		t.Fatalf("model = %q, want repaired gateway default", r.Model)
	}
}

// TestResolveEnvOnlyBedrockDiscoveredWithoutConfig reproduces issue
// #15: pointing ZOT_HOME at a fresh dir drops the persisted
// config.json (which pinned provider=amazon-bedrock). Resolve must
// still discover bedrock from the AWS env vars instead of falling back
// to anthropic and reporting "not logged in".
func TestResolveEnvOnlyBedrockDiscoveredWithoutConfig(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir()) // fresh home: no config.json
	// Disable the Kimi CLI token fallback so a developer machine with a
	// real Kimi CLI login doesn't pre-empt bedrock in the scan.
	if err := SetKimiCLIFallbackDisabled(true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-token")
	t.Setenv("AWS_REGION", "us-east-1")
	// Make sure no other provider's env credential pre-empts bedrock.
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "DEEPSEEK_API_KEY", "KIMI_API_KEY", "MOONSHOT_API_KEY"} {
		t.Setenv(k, "")
	}

	r, err := Resolve(Args{}, true)
	if err != nil {
		t.Fatalf("Resolve errored with env-only bedrock: %v", err)
	}
	if r.Provider != "amazon-bedrock" {
		t.Fatalf("provider = %q, want amazon-bedrock", r.Provider)
	}
	if !r.HasCredential() {
		t.Fatalf("bedrock credential not resolved from env")
	}
}

func TestResolveOllamaUsesModelBaseURLBeforeDefault(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	provider.SetLiveModels(nil)
	defer provider.SetLiveModels(nil)
	provider.SetUserModels([]provider.Model{{
		Provider:      "ollama",
		ID:            "qwen-local",
		DisplayName:   "Qwen Local",
		ContextWindow: 32768,
		MaxOutput:     8192,
		BaseURL:       "http://localhost:8000/v1",
	}})

	r, err := Resolve(Args{Provider: "ollama", Model: "qwen-local"}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://localhost:8000/v1" {
		t.Fatalf("BaseURL = %q, want models.json baseUrl", r.BaseURL)
	}
}

func TestResolveUsesInheritedSwarmCredential(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")

	r, err := Resolve(Args{
		Provider:            "openai",
		Model:               "gpt-5",
		inheritedCredential: "inherited-key",
		inheritedAuthMethod: "apikey",
		inheritedAccountID:  "account-id",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Credential != "inherited-key" || r.AuthMethod != "apikey" || r.AccountID != "account-id" {
		t.Fatalf("inherited credential was not preserved: %+v", r)
	}
}

func TestResolveLlamaCPPUsesRouterInferenceURL(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "http://127.0.0.1:8080/v1/")
	t.Setenv("LLAMA_API_KEY", "")
	provider.SetManagedModels(nil)
	t.Cleanup(func() { provider.SetManagedModels(nil) })

	r, err := Resolve(Args{Provider: "llama.cpp", Model: "local-model"}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://127.0.0.1:8080/v1" {
		t.Fatalf("BaseURL = %q", r.BaseURL)
	}
	if r.Credential != "local" || !r.HasCredential() {
		t.Fatalf("credential = %q", r.Credential)
	}
	if got := r.NewClient().Name(); got != "openai" {
		t.Fatalf("client name = %q", got)
	}
}

func TestResolveLlamaCPPUsesStoredLogin(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("LLAMA_API_KEY", "")
	if err := AuthStoreFor().SetEndpointCredential("llama.cpp", "http://localhost:9090", "stored-key"); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{Provider: "llama.cpp", Model: "stored-model"}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://localhost:9090/v1" || r.Credential != "stored-key" {
		t.Fatalf("resolved = base %q credential %q", r.BaseURL, r.Credential)
	}
}

func TestResolveCustomProviderModelBaseURLBeatsProviderBaseURL(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("MY_COMPANY_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"my-company": {
				"baseUrl": "https://provider.example.com/v1",
				"api": "openai",
				"models": [
					{"id": "fast", "baseUrl": "https://model.example.com/v1"}
				]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	models, warnings := provider.LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	provider.SetLiveModels(nil)
	provider.SetUserModels(models)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	r, err := Resolve(Args{Provider: "my-company", Model: "fast"}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "https://model.example.com/v1" {
		t.Fatalf("BaseURL = %q, want model-level baseUrl", r.BaseURL)
	}
}

func TestCustomProviderUsesOpenAIResponsesAPI(t *testing.T) {
	requestPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath <- r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"company-responses": {
				"baseUrl": "`+srv.URL+`/v1",
				"api": "openai-responses",
				"models": [{"id": "reasoning-model", "reasoning": true}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	models, warnings := provider.LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	provider.SetLiveModels(nil)
	provider.SetUserModels(models)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	r := Resolved{
		Provider:   "company-responses",
		Credential: "test-key",
		BaseURL:    srv.URL + "/v1",
	}
	events, err := r.NewClient().Stream(context.Background(), provider.Request{
		Model:    "reasoning-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if got := <-requestPath; got != "/v1/responses" {
		t.Fatalf("request path = %q, want /v1/responses", got)
	}
}

func TestResolveCustomProviderInsecureFromModelsJSONBaseURL(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LOCAL_PROXY_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"local-proxy": {
				"baseUrl": "https://proxy.example.com/v1",
				"api": "openai",
				"models": [{"id": "default"}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	models, warnings := provider.LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	provider.SetLiveModels(nil)
	provider.SetUserModels(models)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	r, err := Resolve(Args{Provider: "local-proxy", Model: "default", InsecureTLS: true}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if !r.InsecureTLS {
		t.Fatal("InsecureTLS must be set for --insecure with models.json custom baseUrl")
	}
}

func TestResolveOllamaFallsBackToDefaultBaseURL(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	provider.SetLiveModels(nil)
	defer provider.SetLiveModels(nil)

	r, err := Resolve(Args{Provider: "ollama", Model: "any-local-model"}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://localhost:11434" {
		t.Fatalf("BaseURL = %q, want ollama default", r.BaseURL)
	}
}

func TestCanonicalProviderResolvesAliases(t *testing.T) {
	cases := map[string]string{
		"bedrock":         "amazon-bedrock",
		"AWS-Bedrock":     "amazon-bedrock",
		"  bedrock  ":     "amazon-bedrock",
		"vertex":          "google-vertex",
		"gemini":          "google",
		"azure":           "azure-openai-responses",
		"copilot":         "github-copilot",
		"codex":           "openai-codex",
		"moonshot":        "moonshotai",
		"vercel":          "vercel-ai-gateway",
		"hf":              "huggingface",
		"anthropic":       "anthropic",       // canonical passes through
		"amazon-bedrock":  "amazon-bedrock",  // already canonical
		"totally-unknown": "totally-unknown", // unknown returned unchanged (lowered)
		"Totally-UNKNOWN": "totally-unknown",
		"":                "",
	}
	for in, want := range cases {
		if got := canonicalProvider(in); got != want {
			t.Errorf("canonicalProvider(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalProviderAliasesAreKnown(t *testing.T) {
	for alias, canon := range providerAliases {
		if !isKnownProvider(canon) {
			t.Errorf("alias %q maps to %q which is not a known provider", alias, canon)
		}
	}
}

func TestResolveInsecureOnlyWithExplicitBaseURL(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")

	resolved, err := Resolve(Args{Provider: "moonshotai", InsecureTLS: true}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.InsecureTLS {
		t.Fatal("InsecureTLS must not be set for built-in provider base URLs")
	}
	assertDefaultTransportStillSecure(t)

	resolved, err = Resolve(Args{Provider: "openai", InsecureTLS: true, BaseURL: "https://my-llm.internal/v1"}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !resolved.InsecureTLS {
		t.Fatal("InsecureTLS must be set with --insecure and explicit --base-url")
	}
	assertDefaultTransportStillSecure(t)
}

func TestResolveInsecureFromConfigRequiresExplicitBaseURL(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	if err := SaveConfig(Config{Provider: "openai", Insecure: true}); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(Args{Provider: "openai"}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.InsecureTLS {
		t.Fatal("InsecureTLS must not be set without a custom base URL")
	}
	assertDefaultTransportStillSecure(t)

	resolved, err = Resolve(Args{Provider: "openai", BaseURL: "https://my-llm.internal/v1"}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !resolved.InsecureTLS {
		t.Fatal("InsecureTLS must be set when config insecure=true and --base-url is provided")
	}
	assertDefaultTransportStillSecure(t)
}

func TestDefaultXAIModelIsGrok45(t *testing.T) {
	if got := defaultModelForProvider("xai"); got != "grok-4.5" {
		t.Fatalf("default xAI model = %q, want grok-4.5", got)
	}
}

func assertDefaultTransportStillSecure(t *testing.T) {
	t.Helper()
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("http.DefaultTransport must not be made insecure")
	}
}

func TestBuildToolRegistryIncludesGlob(t *testing.T) {
	cwd := t.TempDir()
	reg := buildToolRegistry(Args{}, cwd, nil)
	expected := []string{"read", "write", "edit", "bash", "glob"}
	for _, name := range expected {
		if _, ok := reg[name]; !ok {
			t.Errorf("expected tool %q in registry, missing", name)
		}
	}

	summaries := toolSummaries(reg, Args{})
	var summaryNames []string
	for _, s := range summaries {
		summaryNames = append(summaryNames, s.Name)
	}
	for _, name := range expected {
		found := false
		for _, sn := range summaryNames {
			if sn == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected tool %q in toolSummaries, missing", name)
		}
	}

	// Filtered tools
	filtered := buildToolRegistry(Args{Tools: []string{"glob", "read"}}, cwd, nil)
	if len(filtered) != 2 || filtered["glob"] == nil || filtered["read"] == nil {
		t.Errorf("expected filtered registry with glob and read, got: %#v", filtered)
	}
}
