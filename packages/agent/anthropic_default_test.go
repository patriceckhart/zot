package agent

import "testing"

// Regression coverage for #240: startup and stale-model recovery share the current default.
func TestResolveAnthropicDefaultAndRetiredModels(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		explicit   string
		want       string
	}{
		{"fresh", "", "", "claude-sonnet-5-5"},
		{"retired config", "claude-opus-4-1", "", "claude-sonnet-5-5"},
		{"retired explicit", "claude-haiku-4-5", "claude-3-7-sonnet-20250219", "claude-sonnet-5-5"},
		{"deprecated selection", "claude-sonnet-4-5", "", "claude-sonnet-4-5"},
		{"current selection", "claude-haiku-5-5", "", "claude-haiku-5-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			if tc.configured != "" {
				if err := SaveConfig(Config{Provider: "anthropic", Model: tc.configured}); err != nil {
					t.Fatal(err)
				}
			}
			if got := defaultModelForProvider("anthropic"); got != "claude-sonnet-5-5" {
				t.Errorf("default = %q", got)
			}
			r, err := Resolve(Args{Provider: "anthropic", APIKey: "synthetic-key", Model: tc.explicit, CWD: t.TempDir(), NoContextFiles: true}, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.Provider != "anthropic" || r.Model != tc.want {
				t.Fatalf("resolved = %s/%s, want anthropic/%s", r.Provider, r.Model, tc.want)
			}
			if tc.configured != "" {
				cfg, err := LoadConfig()
				if err != nil {
					t.Fatal(err)
				}
				wantSaved := tc.configured
				if tc.explicit == "" {
					wantSaved = tc.want
				}
				if cfg.Model != wantSaved {
					t.Fatalf("saved model = %q, want %q", cfg.Model, wantSaved)
				}
			}
		})
	}
}
