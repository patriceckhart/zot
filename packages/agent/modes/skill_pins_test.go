package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/patriceckhart/zot/packages/agent/skills"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func pinnedTestConfig() InteractiveConfig {
	return InteractiveConfig{
		PreloadPinnedSkills: true,
		LoadSkillPins:       func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot:       func() []*skills.Skill { return []*skills.Skill{{Name: "review", Body: "Inspect carefully."}} },
	}
}

func TestPinnedSkillsLifecycle(t *testing.T) {
	i := NewInteractive(pinnedTestConfig())
	i.ed.SetValue("typed request")
	if got := strings.Join(i.pinnedSkillsNotice(80), ""); !strings.Contains(got, "review") {
		t.Fatal(got)
	}
	if got := i.ed.Value(); got != "typed request" {
		t.Fatal("notice changed editor")
	}
	first := i.preloadPinnedPromptLocked("first")
	if !strings.Contains(first, "Inspect carefully.") {
		t.Fatal(first)
	}
	if got := i.preloadPinnedPromptLocked("second"); got != "second" {
		t.Fatal(got)
	}
	if len(i.pinnedSkillsNotice(80)) != 0 {
		t.Fatal("notice survived consumption")
	}
	i.runSlash(context.Background(), "/clear")
	if !strings.Contains(i.preloadPinnedPromptLocked("after clear"), "Inspect carefully.") {
		t.Fatal("clear did not rearm pins")
	}
	i.SetPinnedSkillsPending(true)
	i.SetPinnedSkillsPending(false)
	if got := i.preloadPinnedPromptLocked("resumed"); got != "resumed" {
		t.Fatal("resume reinjected pins")
	}
	cfg := pinnedTestConfig()
	cfg.PreloadPinnedSkills = false
	resumed := NewInteractive(cfg)
	if got := resumed.preloadPinnedPromptLocked("existing"); got != "existing" {
		t.Fatal("resumed startup injected pins")
	}
}

func TestSkillPinPickerTogglesAndSaveFailure(t *testing.T) {
	cfg := pinnedTestConfig()
	pins := skills.Pins{}
	fail := false
	cfg.LoadSkillPins = func(string) (skills.Pins, error) { return pins, nil }
	cfg.ToggleSkillPin = func(cwd, name string, global bool) error {
		if fail {
			return errors.New("read-only preferences")
		}
		if name != "review" {
			t.Fatalf("name = %q", name)
		}
		if global {
			if len(pins.Global) == 0 {
				pins.Global = []string{name}
			} else {
				pins.Global = nil
			}
		} else {
			if len(pins.Project) == 0 {
				pins.Project = []string{name}
			} else {
				pins.Project = nil
			}
		}
		return nil
	}
	i := NewInteractive(cfg)
	i.openSkillsDialog()
	for _, key := range []rune{'p', 'g'} {
		if !i.handleSkillPinKey(tui.Key{Kind: tui.KeyRune, Rune: key}) {
			t.Fatal("key ignored")
		}
	}
	rendered := strings.Join(i.skillsDialog.Render(tui.Theme{}, 100), "\n")
	if !strings.Contains(rendered, "[pg]") {
		t.Fatal(rendered)
	}
	if len(i.pinnedSkills) != 1 {
		t.Fatal("scope union duplicated skill")
	}
	i.handleSkillPinKey(tui.Key{Kind: tui.KeyRune, Rune: 'p'})
	if len(i.pinnedSkills) != 1 || len(i.skillsDialog.pins.Project) != 0 {
		t.Fatal("project unpin removed global pin")
	}
	fail = true
	i.handleSkillPinKey(tui.Key{Kind: tui.KeyRune, Rune: 'g'})
	if !strings.Contains(i.statusErr, "read-only") || len(i.skillsDialog.pins.Global) != 1 {
		t.Fatal("save failure was not preserved")
	}
	i.skillsDialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if i.handleSkillPinKey(tui.Key{Kind: tui.KeyRune, Rune: 'g'}) {
		t.Fatal("body view toggled a pin")
	}
}

func TestPinnedSkillsNoticeShowsScopes(t *testing.T) {
	cfg := pinnedTestConfig()
	cfg.LoadSkillPins = func(string) (skills.Pins, error) {
		return skills.Pins{
			Project: []string{"local", "shared"},
			Global:  []string{"everywhere", "shared"},
		}, nil
	}
	cfg.SkillSnapshot = func() []*skills.Skill {
		return []*skills.Skill{{Name: "local"}, {Name: "everywhere"}, {Name: "shared"}}
	}
	i := NewInteractive(cfg)
	text := stripANSIBytes(strings.Join(i.pinnedSkillsNotice(120), "\n"))
	for _, want := range []string{"local (project)", "everywhere (global)", "shared (project + global)"} {
		if !strings.Contains(text, want) {
			t.Errorf("notice missing %q: %q", want, text)
		}
	}
	if strings.Count(text, "shared") != 1 {
		t.Fatalf("both scopes duplicated the skill: %q", text)
	}
}

func TestPinnedSkillsWarningsAndNoticeWidth(t *testing.T) {
	cfg := pinnedTestConfig()
	cfg.LoadSkillPins = func(string) (skills.Pins, error) {
		return skills.Pins{Global: []string{"missing", "review"}}, nil
	}
	i := NewInteractive(cfg)
	i.refreshSkillPins()
	if len(i.reloadErrors) != 1 || !strings.Contains(i.reloadErrors[0], "missing") {
		t.Fatalf("warnings = %v", i.reloadErrors)
	}
	i.pinnedSkills = []*skills.Skill{{Name: strings.Repeat("界", 30)}}
	for _, width := range []int{1, 4, 12, 30, 80} {
		for _, row := range i.pinnedSkillsNotice(width) {
			if got := runewidth.StringWidth(stripANSIBytes(row)); got > width {
				t.Fatalf("width %d exceeds %d", got, width)
			}
		}
	}
}

func TestPinnedSkillsEnterProviderAndPersistedTranscript(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &preparationCaptureClient{requests: make(chan provider.Request, 1)}
	ag := core.NewAgent(client, "test", "base", nil)
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	persisted := make(chan error, 4)
	ag.OnMessageAppended = func(m provider.Message) { persisted <- sess.AppendMessage(m) }
	done := make(chan struct{}, 1)
	cfg := pinnedTestConfig()
	cfg.Agent = ag
	cfg.FlushSession = func() { done <- struct{}{} }
	i := NewInteractive(cfg)
	// Construction only prepares the notice. It must not contact a provider.
	select {
	case <-client.requests:
		t.Fatal("startup invoked model")
	default:
	}
	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("image")}
	i.startTurnWithImages(ctx, "question", []provider.ImageBlock{image})
	select {
	case req := <-client.requests:
		content := req.Messages[0].Content
		if len(content) != 2 {
			t.Fatalf("content = %#v", content)
		}
		text := content[0].(provider.TextBlock).Text
		if !strings.Contains(text, "Inspect carefully.") || !strings.HasSuffix(text, "question") {
			t.Fatal(text)
		}
		if _, ok := content[1].(provider.ImageBlock); !ok {
			t.Fatal("image lost")
		}
	case <-ctx.Done():
		t.Fatal("provider not called")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("turn did not finish")
	}
	for range 2 {
		select {
		case err := <-persisted:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("message not persisted")
		}
	}
	_, messages, err := core.ReadSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || !strings.Contains(messages[0].Content[0].(provider.TextBlock).Text, "Inspect carefully.") {
		t.Fatalf("messages = %#v", messages)
	}
}
