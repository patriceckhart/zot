package agent

import (
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestBindAgentSessionAlias(t *testing.T) {
	ag := core.NewAgent(nil, "test", "", nil)
	aliased := &core.Session{ID: "one", Meta: core.SessionMeta{Alias: "tidy-birch"}}
	bindAgentSession(ag, aliased)
	if ag.SessionID != "one" || ag.SessionAlias != "tidy-birch" {
		t.Fatalf("bound context = %q, %q", ag.SessionID, ag.SessionAlias)
	}
	replacement := core.NewAgent(nil, "replacement", "", nil)
	bindReplacementAgentSession(replacement, ag, aliased)
	if replacement.SessionID != ag.SessionID || replacement.SessionAlias != ag.SessionAlias {
		t.Fatal("model replacement lost the alias")
	}
	bindAgentSession(ag, nil)
	if ag.SessionAlias != "" {
		t.Fatal("non-persisted conversation retained an alias")
	}
	bindAgentSession(replacement, &core.Session{ID: "legacy"})
	if replacement.SessionAlias != "" {
		t.Fatal("legacy session inherited an alias")
	}
}
