package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestStartupInformationPrecedesTranscript(t *testing.T) {
	for _, width := range []int{30, 80} {
		ag := core.NewAgent(nil, "", "", nil)
		ag.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "conversation begins"}}}})
		i := NewInteractive(InteractiveConfig{Agent: ag, StartupExtensionErrors: []string{"discovery diagnostic"}})
		i.Notify("early", "success", "connected at startup")
		i.finishStartup()
		// Populate the idle cache before asynchronous startup results arrive.
		i.cachedChatLocked(width)
		i.NotifyStartup("late", "info", "delayed initialization")
		i.updateInfo = UpdateInfo{Available: true, Latest: "2.0", Current: "1.0"}
		i.showStartupChangelog(ChangelogPayload{Version: "2.0", Body: "\x00H:release heading\n\nrelease details"})
		i.Notify("runtime", "info", "runtime notification")
		chat := stripANSIBytes(strings.Join(i.cachedChatLocked(width), "\n"))
		previous := -1
		for _, text := range []string{"ask anything", "update available", "release details", "connected at startup", "delayed initialization", "discovery diagnostic", "conversation begins", "runtime notification"} {
			at := strings.Index(chat, text)
			if at < 0 || at <= previous {
				t.Fatalf("width %d: %q out of order or absent in %q", width, text, chat)
			}
			previous = at
		}
		if strings.Contains(chat, "\x00H:") {
			t.Fatal("internal release heading marker leaked")
		}
		if len(ag.Messages()) != 1 {
			t.Fatal("startup information entered model context")
		}
	}
}

func TestStartupNotesWithoutUpdateInformation(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	i.Notify("extension", "info", "startup details")
	chat := stripANSIBytes(strings.Join(i.startupBlockLocked(80), "\n"))
	if !strings.Contains(chat, "see commands.\n\n[extension] startup details") {
		t.Fatalf("extension information should directly follow the welcome text: %q", chat)
	}
	if strings.Contains(chat, "update available") || strings.Contains(chat, "release notes") {
		t.Fatalf("unexpected update information: %q", chat)
	}
}

func TestStartupChangelogDoesNotTakeFocus(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	i.ed.SetValue("already typing")
	calls := 0
	i.cfg.OnChangelogDismiss = func() {
		calls++
		if got := i.ChangelogVersion(); got != "1.2.3" {
			t.Fatalf("callback version = %q", got)
		}
	}
	i.showStartupChangelog(ChangelogPayload{Version: "1.2.3", Body: "release notes"})
	if calls != 1 || i.dialogOwnsInput() {
		t.Fatal("release notes took focus or were not marked shown")
	}
	if i.ed.Value() != "already typing" {
		t.Fatal("release notes changed input")
	}
}

func TestStartupNotesLifecycle(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	i.Notify("early", "info", "initial note")
	if len(i.startupNotes) != 1 || len(i.extNotes) != 0 {
		t.Fatal("initial notification was not classified as startup")
	}
	// Even a prompt without credentials ends automatic classification.
	i.startTurn(context.Background(), "hello")
	i.Notify("runtime", "warn", "runtime note")
	i.statusErr = "keep this runtime error"
	i.NotifyStartup("late", "info", "late startup note")
	if len(i.startupNotes) != 2 || len(i.extNotes) != 1 || i.statusErr != "keep this runtime error" {
		t.Fatal("late startup notification altered runtime state")
	}
	i.ClearNotes("late")
	if len(i.startupNotes) != 1 || len(i.extNotes) != 1 {
		t.Fatal("clear_notes did not preserve other owners")
	}
	i.ClearNotes("runtime")
	if len(i.startupNotes) != 1 || len(i.extNotes) != 0 {
		t.Fatal("clear_notes did not clear runtime notes")
	}
	i.runSlash(context.Background(), "/clear")
	if len(i.startupNotes) != 0 {
		t.Fatal("/clear left startup extension notes")
	}
}

func TestStartupChangelogHasBottomRule(t *testing.T) {
	for _, width := range []int{24, 80, 160} {
		i := NewInteractive(InteractiveConfig{})
		i.showStartupChangelog(ChangelogPayload{Version: "1.2.3", URL: "https://example.com/release", Body: "Release details"})
		rows := i.startupBlockLocked(width)
		if len(rows) < 2 {
			t.Fatalf("width %d: missing release notes", width)
		}
		got := stripANSIBytes(rows[len(rows)-2])
		want := strings.Repeat("─", width)
		if got != want {
			t.Fatalf("width %d: bottom rule = %q, want %q", width, got, want)
		}
		if rows[len(rows)-1] != "" {
			t.Fatalf("width %d: missing spacing after bottom rule", width)
		}
	}
}

func TestStartupChangelogBodyWraps(t *testing.T) {
	const width = 24
	rows := renderStartupChangelogBody("\x00H:long release heading with Unicode ä\n\n**Details** with a very long description.", tui.Theme{}, width)
	assertRowsFitWidth(t, rows, width)
}
