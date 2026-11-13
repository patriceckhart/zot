package agent

import (
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestResolveXAIGrok46And47(t *testing.T) {
	for _, id := range []string{"grok-4.6", "grok-4.7"} {
		t.Run(id, func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			t.Setenv("XAI_API_KEY", "synthetic-key")
			r, err := Resolve(Args{Provider: "xai", Model: id, CWD: t.TempDir(), NoContextFiles: true, NoSkill: true}, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.Provider != "xai" || r.Model != id {
				t.Fatalf("requested xai/%s, resolved %s/%s", id, r.Provider, r.Model)
			}
			m, err := provider.FindModel(r.Provider, r.Model)
			if err != nil || m.API != provider.APIResponses {
				t.Fatalf("model=%+v, err=%v", m, err)
			}
		})
	}
}
