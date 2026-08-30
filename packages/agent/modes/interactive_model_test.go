package modes

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

func TestReasoningSelectorUsesCurrentModelLevels(t *testing.T) {
	i := &Interactive{cfg: InteractiveConfig{
		Provider:  "openai",
		Model:     "gpt-5.6-sol",
		Reasoning: "minimum",
	}}

	item := i.reasoningSettingItem()
	var levels []string
	for _, option := range item.options {
		levels = append(levels, option.value)
	}
	want := []string{"", "low", "medium", "high", "xhigh", "max"}
	if !slices.Equal(levels, want) {
		t.Fatalf("reasoning levels = %q, want %q", levels, want)
	}
	if item.options[item.choice].value != "low" {
		t.Fatalf("clamped current level = %q, want low", item.options[item.choice].value)
	}
}

func TestReasoningSelectorOnlyOffersOffForUnsupportedModel(t *testing.T) {
	i := &Interactive{cfg: InteractiveConfig{
		Provider:  "google",
		Model:     "gemini-2.0-flash",
		Reasoning: "high",
	}}

	item := i.reasoningSettingItem()
	if len(item.options) != 1 || item.options[0].value != "" {
		t.Fatalf("reasoning levels = %#v, want off only", item.options)
	}
	if item.hint != "current model does not support reasoning" {
		t.Fatalf("hint = %q", item.hint)
	}
}

func TestReasoningCommandOpensDirectSelector(t *testing.T) {
	i := &Interactive{
		cfg: InteractiveConfig{
			Reasoning: "high",
		},
		settingsDialog: newSettingsDialog(),
	}

	i.runSlash(context.Background(), "/reasoning")

	if !i.settingsDialog.Active() || !i.settingsDialog.selecting || !i.settingsDialog.direct {
		t.Fatal("reasoning command did not open the direct option selector")
	}
	lines := i.settingsDialog.Render(tui.Dark, 100)
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "reasoning level") || !strings.Contains(text, "high") {
		t.Fatalf("reasoning selector missing current level: %q", text)
	}
	i.settingsDialog.HandleKey(tui.Key{Kind: tui.KeyDown})
	act := i.settingsDialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	i.applySettingChange(act)
	if i.cfg.Reasoning != "xhigh" || i.settingsDialog.Active() {
		t.Fatalf("reasoning selection was not applied and closed: level=%q active=%v", i.cfg.Reasoning, i.settingsDialog.Active())
	}
	if i.statusOK != "reasoning level xhigh" {
		t.Fatalf("reasoning status = %q", i.statusOK)
	}

	i.runSlash(context.Background(), "/reasoning")
	act = i.settingsDialog.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if !act.Close || i.settingsDialog.Active() {
		t.Fatal("escape did not close the direct reasoning selector")
	}
}

func TestNormalizeModelQueryKeepsPresetAtSign(t *testing.T) {
	if got := normalizeModelQuery("@preset/flash"); got != "@presetflash" {
		t.Fatalf("id = %q; want @presetflash", got)
	}
	if got := normalizeModelQuery("preset"); got != "preset" {
		t.Fatalf("query = %q; want preset", got)
	}
	haystack := normalizeModelQuery("openrouter @preset/flash Flash (preset)")
	if !strings.Contains(haystack, normalizeModelQuery("preset")) {
		t.Fatalf("%q does not contain preset", haystack)
	}
}

func TestModelDialogAdvertisesReasoningSelector(t *testing.T) {
	d := newModelDialog()
	d.Open("", nil, "high")

	text := strings.Join(d.Render(tui.Dark, 100), "\n")
	if !strings.Contains(text, "current reasoning: high") || !strings.Contains(text, "/reasoning") {
		t.Fatalf("model dialog missing reasoning guidance: %q", text)
	}
}

func TestModelPickerSkipsLlamaRefreshWhenRouterIsNotConfigured(t *testing.T) {
	i := &Interactive{
		cfg: InteractiveConfig{
			RefreshLlamaCPPModels: func(context.Context) error { return nil },
		},
		modelRefresh: make(chan modelRefreshResult, 1),
		modelDialog:  newModelDialog(),
	}

	i.runSlash(context.Background(), "/model")

	if !i.modelDialog.Active() {
		t.Fatal("model picker did not open immediately")
	}
	if i.statusOK == "refreshing models" {
		t.Fatal("refresh status shown without llama.cpp router configuration")
	}
}
