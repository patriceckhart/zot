package agent

import (
	"fmt"

	"github.com/patriceckhart/zot/internal/agent/tools"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
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
}

// HasCredential reports whether a credential was resolved.
func (r Resolved) HasCredential() bool { return r.Credential != "" }

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
	if provName != "anthropic" && provName != "openai" {
		return Resolved{}, fmt.Errorf("provider must be anthropic or openai (got %q)", provName)
	}

	// Try the requested provider first.
	cred, method, accountID, credErr := ResolveCredentialFull(provName, args.APIKey)

	// If the user did NOT explicitly pick a provider and the default one
	// has no credentials, auto-fall-back to whichever provider is actually
	// logged in. That way running plain `zot` after `/login` (any provider)
	// never shows a "not logged in" banner.
	userPickedProvider := args.Provider != ""
	if credErr != nil && !userPickedProvider {
		other := "openai"
		if provName == "openai" {
			other = "anthropic"
		}
		if c, m, a, err := ResolveCredentialFull(other, args.APIKey); err == nil {
			provName = other
			cred, method, accountID, credErr = c, m, a, err
		}
	}

	model := firstNonEmpty(args.Model, cfg.Model)
	if model == "" {
		if provName == "openai" {
			model = "gpt-5"
		} else {
			model = provider.DefaultModel.ID
		}
	}
	// If the resolved model belongs to a different provider (e.g. config
	// says gpt-5 but we fell back to anthropic), pick that provider's default.
	if m, err := provider.FindModel("", model); err == nil && m.Provider != provName {
		if provName == "openai" {
			model = "gpt-5"
		} else {
			model = provider.DefaultModel.ID
		}
	}
	if _, err := provider.FindModel(provName, model); err != nil {
		return Resolved{}, err
	}

	if credErr != nil && requireCred {
		return Resolved{}, fmt.Errorf("%w; set %s_API_KEY, pass --api-key, or run `zot` and /login",
			credErr, envVarName(provName))
	}

	sandbox := tools.NewSandbox(args.CWD)
	reg := buildToolRegistry(args, args.CWD, sandbox)
	summaries := toolSummaries(reg, args)

	sys := BuildSystemPrompt(SystemPromptOpts{
		CWD:    args.CWD,
		Tools:  summaries,
		Custom: args.SystemPrompt,
		Append: args.AppendSystemPrompt,
	})

	reasoning := firstNonEmpty(args.Reasoning, cfg.Reasoning)

	max := args.MaxSteps
	if max <= 0 {
		max = 50
	}

	return Resolved{
		Provider:     provName,
		Model:        model,
		Credential:   cred,
		AuthMethod:   method,
		AccountID:    accountID,
		BaseURL:      args.BaseURL,
		CWD:          args.CWD,
		Reasoning:    reasoning,
		ToolRegistry: reg,
		ToolSummary:  summaries,
		SystemPrompt: sys,
		MaxSteps:     max,
		Sandbox:      sandbox,
	}, nil
}

// NewClient returns a provider.Client for r, choosing the auth mode
// based on r.AuthMethod. Panics if no credential is present; callers
// must check HasCredential() first.
func (r Resolved) NewClient() provider.Client {
	if !r.HasCredential() {
		panic("NewClient called without credential; check HasCredential first")
	}
	if r.Provider == "openai" {
		if r.AuthMethod == "oauth" {
			return provider.NewOpenAICodex(r.Credential, r.AccountID, r.BaseURL)
		}
		return provider.NewOpenAI(r.Credential, r.BaseURL)
	}
	if r.AuthMethod == "oauth" {
		return provider.NewAnthropicOAuth(r.Credential, r.BaseURL)
	}
	return provider.NewAnthropic(r.Credential, r.BaseURL)
}

// UseSandbox replaces the sandbox pointer that every tool in r's
// registry references. Used to keep the /lock state stable across
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

func envVarName(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI"
	default:
		return "ANTHROPIC"
	}
}
