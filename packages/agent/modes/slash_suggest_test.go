package modes

import (
	"slices"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/skills"
)

func TestSlashSuggesterHidesUnjailUntilJailed(t *testing.T) {
	s := newSlashSuggester()

	if got := commandNames(s.matches("/unj")); contains(got, "/unjail") {
		t.Fatalf("/unjail should be hidden while not jailed, got %v", got)
	}
	if got := commandNames(s.matches("/ja")); !contains(got, "/jail") {
		t.Fatalf("/jail should be visible while not jailed, got %v", got)
	}

	s.SetJailed(true)
	if got := commandNames(s.matches("/unj")); !contains(got, "/unjail") {
		t.Fatalf("/unjail should be visible while jailed, got %v", got)
	}
	if got := commandNames(s.matches("/ja")); contains(got, "/jail") {
		t.Fatalf("/jail should be hidden while jailed, got %v", got)
	}
}

func TestSlashSuggesterShowsLlamaOnlyWhenConfigured(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/llama")); contains(got, "/llama") {
		t.Fatalf("/llama visible without login: %v", got)
	}
	s.SetLlamaConfigured(true)
	if got := commandNames(s.matches("/llama")); !contains(got, "/llama") {
		t.Fatalf("/llama missing with login: %v", got)
	}
}

func TestSlashSuggesterShowsNewOnlyWithSessions(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/new")); contains(got, "/new") {
		t.Fatalf("/new visible with sessions disabled: %v", got)
	}
	s.SetSessionsEnabled(true)
	if got := commandNames(s.matches("/new")); !contains(got, "/new") {
		t.Fatalf("/new missing with sessions enabled: %v", got)
	}
}

func TestSlashSuggesterHasSwarm(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/sw")); !contains(got, "/swarm") {
		t.Fatalf("/swarm missing from suggestions, got %v", got)
	}
}

func TestSlashSuggesterPrefersExactSessionCommand(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/session")); !slices.Equal(got, []string{"/session"}) {
		t.Fatalf("matches = %v, want exact /session", got)
	}
	if got := s.Selection("/session"); got != "/session" {
		t.Fatalf("selection = %q, want /session", got)
	}
}

func TestSlashSuggesterCompletesSessionActions(t *testing.T) {
	s := newSlashSuggester()
	if got, want := commandNames(s.matches("/session ")), []string{
		"/session timeline",
		"/session export",
		"/session import",
		"/session fork",
		"/session tree",
	}; !slices.Equal(got, want) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
	if got := commandNames(s.matches("/session t")); !slices.Equal(got, []string{"/session timeline", "/session tree"}) {
		t.Fatalf("filtered matches = %v", got)
	}
	if got := s.Selection("/session t"); got != "/session timeline" {
		t.Fatalf("selection = %q, want /session timeline", got)
	}
}

func TestSlashCommandsAreCaseInsensitive(t *testing.T) {
	s := newSlashSuggester()
	if got := commandNames(s.matches("/EX")); !contains(got, "/exit") {
		t.Fatalf("/EX did not suggest /exit: %v", got)
	}
	if !isKnownSlashCommand("/Exit") {
		t.Fatal("/Exit was not recognized as a built-in command")
	}
	if !slashCancelsTurn("/CLEAR") {
		t.Fatal("/CLEAR did not retain /clear cancellation semantics")
	}
	if !slashCancelsTurn("/NEW") {
		t.Fatal("/NEW did not retain /new cancellation semantics")
	}
}

func TestSlashSuggesterBuiltinsShadowExtensionsCaseInsensitively(t *testing.T) {
	s := newSlashSuggester()
	s.SetExtra([]slashCommand{{Name: "/EXIT", Desc: "extension exit"}})
	matches := commandNames(s.matches("/exit"))
	if len(matches) != 1 || matches[0] != "/exit" {
		t.Fatalf("matches = %v, want only built-in /exit", matches)
	}
}

func TestSlashSuggesterGroupsDynamicBuiltinsWithMainCommands(t *testing.T) {
	s := newSlashSuggester()
	s.SetExtra([]slashCommand{
		{Name: "/answer", Desc: "extension command"},
		{Name: "/mcp", Desc: "built-in MCP status", Builtin: true},
		{Name: "/notifier", Desc: "extension command"},
	})
	inExtensions := false
	foundBuiltin := false
	for _, command := range s.matches("/") {
		if command.Header {
			inExtensions = true
			continue
		}
		switch command.Name {
		case "/mcp":
			foundBuiltin = true
			if inExtensions {
				t.Fatal("built-in /mcp appears in the extensions group")
			}
		case "/answer", "/notifier":
			if !inExtensions {
				t.Fatalf("extension %s appears in the main group", command.Name)
			}
		}
	}
	if !foundBuiltin {
		t.Fatal("built-in /mcp missing from the main group")
	}
	names := commandNames(s.allCatalog())
	for idx, name := range names {
		if name == "/swarm" && (idx+1 >= len(names) || names[idx+1] != "/wip") {
			t.Fatalf("/wip does not follow /swarm: %v", names)
		}
		if name == "/wip" && (idx+1 >= len(names) || names[idx+1] != "/mcp") {
			t.Fatalf("/mcp does not follow /wip: %v", names)
		}
	}
	for _, command := range s.matches("/mc") {
		if command.Header {
			t.Fatal("built-in-only matches have an extensions divider")
		}
	}
	s.SetExtra(nil)
	if contains(commandNames(s.matches("/mcp")), "/mcp") {
		t.Fatal("removed built-in command is still visible")
	}
}

func TestSlashSuggesterCompletesVisibleSkills(t *testing.T) {
	s := newSlashSuggester()
	s.SetSkills([]*skills.Skill{
		{Name: "test-fix", Description: "Fix failing tests."},
		{Name: "ai-development-workflow", Description: "Guide AI development."},
		{Name: "manual-only", Description: "Explicit invocation only.", DisableModelInvocation: true},
		{Name: "builtin", Description: "Internal.", Builtin: true},
	})

	if got, want := commandNames(s.matches("/skill:")), []string{
		"/skill:ai-development-workflow",
		"/skill:manual-only",
		"/skill:test-fix",
	}; !slices.Equal(got, want) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
	s.lastMatches = s.matches("/skill:")
	if got := s.Selection("/skill:"); got != "/skill:ai-development-workflow" {
		t.Fatalf("initial selection = %q", got)
	}
	s.Down()
	if got := s.Selection("/skill:"); got != "/skill:manual-only" {
		t.Fatalf("selection after down = %q", got)
	}
	if got := commandNames(s.matches("/SKILL:ai-dev")); !slices.Equal(got, []string{"/skill:ai-development-workflow"}) {
		t.Fatalf("filtered matches = %v", got)
	}
	if got := s.Selection("/skill:ai-dev"); got != "/skill:ai-development-workflow" {
		t.Fatalf("selection = %q", got)
	}
}

func TestSlashSuggesterRefreshesSkillsOncePerInputSession(t *testing.T) {
	s := newSlashSuggester()
	if s.SkillInputStarted("/skill") {
		t.Fatal("refresh started before colon")
	}
	if !s.SkillInputStarted("/skill:") {
		t.Fatal("first /skill: did not request refresh")
	}
	if s.SkillInputStarted("/skill:review") {
		t.Fatal("same input session requested another refresh")
	}
	if s.SkillInputStarted("") {
		t.Fatal("leaving skill input requested refresh")
	}
	if !s.SkillInputStarted("/SKILL:") {
		t.Fatal("new case-insensitive input session did not request refresh")
	}
}

func commandNames(cmds []slashCommand) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if !c.Header {
			out = append(out, c.Name)
		}
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
