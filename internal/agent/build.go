package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/patriceckhart/zot/internal/agent/tools"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/skills"
)

// Resolved is the effective configuration after merging CLI, config, defaults.
type Resolved struct {
	Provider   string
	Model      string
	Credential string // api key or oauth access token
	AuthMethod string // "apikey" | "oauth" | "" (no credential yet)
	AccountID  string // ChatGPT account id (for openai oauth), "" otherwise
	BaseURL    string
	CWD        string
	Reasoning  string

	ToolRegistry core.Registry
	ToolSummary  []ToolSummary
	SystemPrompt string
	MaxSteps     int
	Sandbox      *tools.Sandbox

	// SkillTool is the on-demand skill loader registered with the
	// agent's tool registry, or nil if no SKILL.md files were
	// discovered. Exposed so the tui can list / preview skills.
	SkillTool *skills.Tool

	// Bookkeeping for MergeExtensionTools. Captured at Resolve time
	// so the system prompt can be rebuilt later without re-running
	// resolve.
	systemAppend     []string
	systemCustom     string
	toolDescriptions map[string]string
}

// HasCredential reports whether a credential was resolved.
func (r Resolved) HasCredential() bool { return r.Credential != "" }

// MergeExtensionTools folds every tool registered by an extension
// into r's ToolRegistry and re-renders the system prompt's tool
// summary so the model sees both built-in and extension tools.
//
// Idempotent: calling twice with the same manager state has no
// effect on the second pass (existing names are preserved). Built-in
// tools always win on conflict.
func (r *Resolved) MergeExtensionTools(mgr ExtensionToolSource) {
	if mgr == nil {
		return
	}
	infos := mgr.Tools()
	if len(infos) == 0 {
		return
	}
	changed := false
	for _, info := range infos {
		if _, exists := r.ToolRegistry[info.Name]; exists {
			continue
		}
		r.ToolRegistry[info.Name] = mgr.NewExtensionTool(info)
		changed = true
	}
	if !changed {
		return
	}
	// Re-render the system prompt with the merged tool list. Skill
	// addendum is preserved by walking the existing append slice.
	append_ := r.systemAppend
	r.SystemPrompt = BuildSystemPrompt(SystemPromptOpts{
		CWD:    r.CWD,
		Tools:  toolSummariesFromRegistry(r.ToolRegistry, r.toolDescriptions),
		Custom: r.systemCustom,
		Append: append_,
	})
}

// ExtensionToolSource is the slice of the extension manager that
// MergeExtensionTools needs. Lives here as an interface so the
// build package doesn't import internal/agent/extensions (which
// imports core, which imports... avoid the cycle).
type ExtensionToolSource interface {
	Tools() []ExtensionToolInfo
	NewExtensionTool(info ExtensionToolInfo) core.Tool
}

// ExtensionToolInfo mirrors extensions.ToolInfo so we can declare
// ExtensionToolSource here without importing the extensions
// package. The cli wires a tiny adapter to bridge them.
type ExtensionToolInfo struct {
	Extension   string
	Name        string
	Description string
	Schema      []byte
}

// toolSummariesFromRegistry rebuilds the system-prompt tool list
// from a (possibly extended) registry, using cached descriptions for
// the human-readable summary text.
func toolSummariesFromRegistry(reg core.Registry, cached map[string]string) []ToolSummary {
	out := make([]ToolSummary, 0, len(reg))
	for name, t := range reg {
		desc := t.Description()
		if d, ok := cached[name]; ok && d != "" {
			desc = d
		}
		out = append(out, ToolSummary{Name: name, Description: desc})
	}
	return out
}

// Resolve merges args, config, and env into a Resolved set.
//
// Unlike the earlier version, Resolve NEVER returns an error for
// missing credentials: the TUI can start without them and launch a
// login flow. requireCred controls whether missing credentials are a
// hard error (used by print/json modes).
func Resolve(args Args, requireCred bool) (Resolved, error) {
	cfg, _ := LoadConfig()

	// User-requested provider (explicit > config > default).
	provName := firstNonEmpty(args.Provider, cfg.Provider, "anthropic")
	if provName != "anthropic" && provName != "openai" && provName != "kimi" && provName != "ollama" {
		// Unknown provider (maybe removed or renamed). Fall back to
		// the first provider that has credentials, or anthropic.
		provName = "anthropic"
		if _, _, _, err := ResolveCredentialFull("openai", ""); err == nil {
			provName = "openai"
		}
		if _, _, _, err := ResolveCredentialFull("kimi", ""); err == nil {
			provName = "kimi"
		}
		if _, _, _, err := ResolveCredentialFull("anthropic", ""); err == nil {
			provName = "anthropic"
		}
		// Reset the saved config so this doesn't keep happening.
		cfg.Provider = provName
		cfg.Model = ""
		_ = SaveConfig(cfg)
	}

	var (
		cred      string
		method    string
		accountID string
		credErr   error
	)
	if provName == "ollama" {
		if args.BaseURL == "" {
			args.BaseURL = "http://localhost:11434"
		}
		cred = firstNonEmpty(args.APIKey, "ollama")
		method = "apikey"
	} else {
		cred, method, accountID, credErr = ResolveCredentialFull(provName, args.APIKey)
	}

	// If the user did NOT explicitly pick a provider and the default one
	// has no credentials, auto-fall-back to whichever provider is actually
	// logged in. That way running plain `zot` after `/login` (any provider)
	// never shows a "not logged in" banner.
	userPickedProvider := args.Provider != ""
	if credErr != nil && !userPickedProvider && provName != "ollama" {
		for _, other := range []string{"anthropic", "openai", "kimi"} {
			if other == provName {
				continue
			}
			if c, m, a, err := ResolveCredentialFull(other, args.APIKey); err == nil {
				provName = other
				cred, method, accountID, credErr = c, m, a, err
				break
			}
		}
	}

	model := firstNonEmpty(args.Model, cfg.Model)
	if model == "" {
		switch provName {
		case "openai":
			model = "gpt-5"
		case "kimi":
			model = "kimi-for-coding"
		case "ollama":
			return Resolved{}, fmt.Errorf("ollama requires --model (e.g. --model llama3)")
		default:
			model = provider.DefaultModel.ID
		}
	}
	// If the resolved model belongs to a different provider (e.g. config
	// says gpt-5 but we fell back to anthropic), pick that provider's default.
	if provName != "ollama" {
		if m, err := provider.FindModel("", model); err == nil && m.Provider != provName {
			switch provName {
			case "openai":
				model = "gpt-5"
			case "kimi":
				model = "kimi-for-coding"
			default:
				model = provider.DefaultModel.ID
			}
		}
	}
	resolvedModel, err := provider.FindModel(provName, model)
	if err != nil && provName != "ollama" {
		return Resolved{}, err
	}
	if err != nil && provName == "ollama" {
		resolvedModel = provider.Model{
			Provider:      "ollama",
			ID:            model,
			DisplayName:   model,
			ContextWindow: 32768,
			MaxOutput:     8192,
			BaseURL:       args.BaseURL,
			Source:        "ollama",
		}
	}

	// If the model defines a base URL (e.g. local ollama) and the
	// user didn't pass --base-url, use the model's URL.
	if args.BaseURL == "" && resolvedModel.BaseURL != "" {
		args.BaseURL = resolvedModel.BaseURL
	}

	// If the model has a base URL, credentials are optional (local
	// models like ollama don't need real API keys).
	if resolvedModel.BaseURL != "" && credErr != nil {
		cred = "ollama"
		credErr = nil
		requireCred = false
	}

	if credErr != nil && requireCred {
		return Resolved{}, fmt.Errorf("%w; set %s_API_KEY, pass --api-key, or run `zot` and /login",
			credErr, envVarName(provName))
	}

	sandbox := tools.NewSandbox(args.CWD)
	reg := buildToolRegistry(args, args.CWD, sandbox)

	// Skill discovery: scan project + global locations + built-in
	// skills shipped with the binary. If any are found, register
	// the on-demand `skill` loader tool plus a system-prompt
	// manifest so the model knows what's available.
	//
	// --no-skill bypasses the entire mechanism: no manifest in the
	// system prompt, no `skill` tool in the registry. Useful for a
	// clean-room run with zero extra context biasing the model.
	var (
		discovered    []*skills.Skill
		skillTool     *skills.Tool
		skillAddendum string
	)
	if !args.NoSkill {
		homeDir, _ := os.UserHomeDir()
		discovered, _ = skills.Discover(ZotHome(), args.CWD, homeDir, args.WithSkills)
		if len(discovered) > 0 {
			skillTool = skills.NewTool(discovered)
			reg[skillTool.Name()] = skillTool
			skillAddendum = skills.SystemPromptAddendum(discovered)
		}
	}
	_ = skillTool

	summaries := toolSummaries(reg, args)

	append_ := append([]string(nil), args.AppendSystemPrompt...)
	if skillAddendum != "" {
		append_ = append(append_, skillAddendum)
	}

	// Custom system prompt resolution order:
	//   1. --system-prompt flag (highest priority; ad-hoc per run)
	//   2. $ZOT_HOME/SYSTEM.md (persistent user override)
	//   3. built-in default (defaultIdentity + defaultGuidelines)
	custom := args.SystemPrompt
	if custom == "" {
		custom = readUserSystemPrompt(ZotHome())
	}

	sys := BuildSystemPrompt(SystemPromptOpts{
		CWD:    args.CWD,
		Tools:  summaries,
		Custom: custom,
		Append: append_,
	})

	reasoning := firstNonEmpty(args.Reasoning, cfg.Reasoning)

	max := args.MaxSteps // 0 = unlimited

	return Resolved{
		Provider:         provName,
		Model:            model,
		Credential:       cred,
		AuthMethod:       method,
		AccountID:        accountID,
		BaseURL:          args.BaseURL,
		CWD:              args.CWD,
		Reasoning:        reasoning,
		ToolRegistry:     reg,
		ToolSummary:      summaries,
		SystemPrompt:     sys,
		MaxSteps:         max,
		Sandbox:          sandbox,
		SkillTool:        skillTool,
		systemAppend:     append_,
		systemCustom:     custom,
		toolDescriptions: descMapFromSummaries(summaries),
	}, nil
}

// readUserSystemPrompt looks for $ZOT_HOME/SYSTEM.md and returns its
// trimmed contents, or "" when the file is missing / unreadable /
// empty. Errors are intentionally swallowed: the file is optional,
// and any failure to read it should fall back to the built-in
// default system prompt rather than crash the run.
func readUserSystemPrompt(zotHome string) string {
	if zotHome == "" {
		return ""
	}
	path := filepath.Join(zotHome, "SYSTEM.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// descMapFromSummaries indexes the human-readable descriptions for
// the renderToolsSection rebuild path.
func descMapFromSummaries(summaries []ToolSummary) map[string]string {
	out := make(map[string]string, len(summaries))
	for _, s := range summaries {
		out[s.Name] = s.Description
	}
	return out
}

// NewClient returns a provider.Client for r, choosing the auth mode
// based on r.AuthMethod. Panics if no credential is present; callers
// must check HasCredential() first.
func (r Resolved) NewClient() provider.Client {
	if !r.HasCredential() {
		panic("NewClient called without credential; check HasCredential first")
	}
	switch r.Provider {
	case "ollama":
		return provider.NewOpenAI(r.Credential, r.BaseURL)
	case "kimi":
		return provider.NewKimiWithHeaders(r.Credential, r.BaseURL, kimiCodeHeaders())
	case "openai":
		if r.AuthMethod == "oauth" {
			inner := provider.NewOpenAICodex(r.Credential, r.AccountID, r.BaseURL)
			return r.wrapWithRefresh(inner)
		}
		return provider.NewOpenAI(r.Credential, r.BaseURL)
	default:
		if r.AuthMethod == "oauth" {
			inner := provider.NewAnthropicOAuth(r.Credential, r.BaseURL)
			return r.wrapWithRefresh(inner)
		}
		return provider.NewAnthropic(r.Credential, r.BaseURL)
	}
}

// wrapWithRefresh wraps an OAuth client so the access token is
// refreshed automatically before each API call. Without this, long
// sessions (hours) silently fail when the 1-hour token expires.
func (r Resolved) wrapWithRefresh(inner provider.Client) provider.Client {
	provName := r.Provider
	baseURL := r.BaseURL
	accountID := r.AccountID

	refreshFn := func(ctx context.Context) (string, error) {
		tok, err := refreshIfExpired(provName, loadOAuthToken(provName))
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil
	}

	factory := func(token string) provider.Client {
		switch provName {
		case "openai":
			return provider.NewOpenAICodex(token, accountID, baseURL)
		case "kimi":
			return provider.NewKimiWithHeaders(token, baseURL, kimiCodeHeaders())
		default:
			return provider.NewAnthropicOAuth(token, baseURL)
		}
	}

	return provider.NewRefreshingClient(inner, refreshFn, factory)
}

// UseSandbox replaces the sandbox pointer that every tool in r's
// registry references. Used to keep the /jail state stable across
// agent rebuilds (e.g. /login, /model switching providers).
func (r *Resolved) UseSandbox(s *tools.Sandbox) {
	if s == nil || r == nil {
		return
	}
	r.Sandbox = s
	for name, t := range r.ToolRegistry {
		switch v := t.(type) {
		case *tools.ReadTool:
			v.Sandbox = s
		case *tools.WriteTool:
			v.Sandbox = s
		case *tools.EditTool:
			v.Sandbox = s
		case *tools.BashTool:
			v.Sandbox = s
		}
		_ = name
	}
}

// NewAgent constructs a core.Agent from r. Requires a credential.
func (r Resolved) NewAgent() *core.Agent {
	a := core.NewAgent(r.NewClient(), r.Model, r.SystemPrompt, r.ToolRegistry)
	a.MaxSteps = r.MaxSteps
	a.Reasoning = r.Reasoning
	return a
}

func buildToolRegistry(args Args, cwd string, sandbox *tools.Sandbox) core.Registry {
	if args.NoTools {
		return core.Registry{}
	}
	all := map[string]core.Tool{
		"read":  &tools.ReadTool{CWD: cwd, Sandbox: sandbox},
		"write": &tools.WriteTool{CWD: cwd, Sandbox: sandbox},
		"edit":  &tools.EditTool{CWD: cwd, Sandbox: sandbox},
		"bash":  &tools.BashTool{CWD: cwd, Sandbox: sandbox},
	}
	reg := core.Registry{}
	if len(args.Tools) == 0 {
		for _, t := range all {
			reg[t.Name()] = t
		}
		return reg
	}
	for _, name := range args.Tools {
		if t, ok := all[name]; ok {
			reg[name] = t
		}
	}
	return reg
}

func toolSummaries(reg core.Registry, args Args) []ToolSummary {
	order := []string{"read", "write", "edit", "bash"}
	var out []ToolSummary
	for _, name := range order {
		if t, ok := reg[name]; ok {
			out = append(out, ToolSummary{Name: t.Name(), Description: t.Description()})
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func kimiCodeHeaders() map[string]string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	deviceID := ""
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".kimi", "device_id")); err == nil {
			deviceID = strings.TrimSpace(string(b))
		}
	}
	if deviceID == "" {
		deviceID = "zot"
	}
	return map[string]string{
		"User-Agent":         "KimiCLI/1.41.0",
		"X-Msh-Platform":     "kimi_cli",
		"X-Msh-Version":      "1.41.0",
		"X-Msh-Device-Name":  host,
		"X-Msh-Device-Model": runtime.GOOS + "-" + runtime.GOARCH,
		"X-Msh-Os-Version":   runtime.GOOS,
		"X-Msh-Device-Id":    deviceID,
	}
}

func envVarName(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI"
	case "kimi":
		return "KIMI"
	case "ollama":
		return "OLLAMA"
	default:
		return "ANTHROPIC"
	}
}
