package modes

import (
	"fmt"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestModelPickersUseHostCatalog(t *testing.T) {
	models := []provider.Model{{Provider: "anthropic", ID: "visible-model"}}
	i := NewInteractive(InteractiveConfig{
		Theme: tui.Dark,
		ModelCatalog: func() []provider.Model {
			return models
		},
		LoggedInProviders: func() []string { return []string{"anthropic"} },
	})
	i.openQuickModelPicker(1)
	if len(i.modelDialog.all) != 1 || i.modelDialog.all[0].ID != "visible-model" {
		t.Fatalf("shortcut picker = %+v", i.modelDialog.all)
	}
	i.modelDialog.Close()
	i.openModelPickerAfterRefresh(nil)
	if len(i.modelDialog.all) != 1 || i.modelDialog.all[0].ID != "visible-model" {
		t.Fatalf("model picker = %+v", i.modelDialog.all)
	}
	i.rescueDialog.Open("failed-model", []string{"anthropic"}, "anthropic", "failed-model", "failure", "prompt")
	if len(i.rescueDialog.all) != 1 || i.rescueDialog.all[0].ID != "visible-model" {
		t.Fatalf("rescue picker = %+v", i.rescueDialog.all)
	}
	models = nil
	i.modelDialog.Open("", []string{"anthropic"})
	i.rescueDialog.Open("failed-model", []string{"anthropic"}, "anthropic", "failed-model", "failure", "prompt")
	if len(i.modelDialog.all) != 0 || len(i.rescueDialog.all) != 0 {
		t.Fatal("empty host catalog fell back to unfiltered models")
	}
}

func TestDisabledProviderBlocksExistingShortcutAndFastModelSwap(t *testing.T) {
	const current = "claude-3-5-haiku-20241022"
	const target = "claude-opus-4-1-20250805"
	ag := &core.Agent{Model: current, MaxTokens: 100}
	i := &Interactive{
		agent: ag,
		cfg: InteractiveConfig{
			Provider:            "anthropic",
			Model:               current,
			QuickModelShortcuts: []QuickModelShortcut{{Provider: "anthropic", Model: target}},
			CheckProviderEnabled: func(name string) error {
				return fmt.Errorf("provider %q is disabled", name)
			},
			PersistModel: func(string, string) { t.Fatal("disabled selection was persisted") },
		},
	}
	i.applyQuickModelShortcut(1)
	if ag.Model != current || ag.MaxTokens != 100 || i.statusErr == "" {
		t.Fatalf("disabled shortcut changed model: %q, %d, error %q", ag.Model, ag.MaxTokens, i.statusErr)
	}
	// Removing the policy restriction still allows explicit selections. The
	// visibility of speculative models is not consulted by the switch path.
	i.cfg.CheckProviderEnabled = func(string) error { return nil }
	i.cfg.PersistModel = nil
	i.applyQuickModelShortcut(1)
	if ag.Model != target {
		t.Fatalf("enabled shortcut did not switch: %q, error %q", ag.Model, i.statusErr)
	}
}
