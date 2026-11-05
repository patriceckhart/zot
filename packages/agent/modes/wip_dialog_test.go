package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestWIPCommandOpensSubviewWithoutToggling(t *testing.T) {
	for _, active := range []bool{false, true} {
		i := newKeymapInteractive(nil)
		i.runCtx = context.Background()
		i.agent.SetWIP(active)
		before := i.agent.WIPState()
		i.ed.SetValue("/wip")
		i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyEnter})
		if !i.settingsDialog.Active() || !i.settingsDialog.direct || i.settingsDialog.title != "wip" {
			t.Fatal("/wip did not open the WIP subview")
		}
		if i.agent.WIPState() != before {
			t.Fatal("opening the subview changed WIP state")
		}
		text := strings.Join(i.settingsDialog.Render(tui.Dark, 100), "\n")
		if !strings.Contains(text, i.wipStatus()) || !strings.Contains(text, "cleanup request") {
			t.Fatalf("missing state or cleanup action: %s", text)
		}
		i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyEsc})
		if i.settingsDialog.Active() || i.agent.WIPState() != before {
			t.Fatal("Esc did not cancel without changing state")
		}
	}
}

func TestWIPSubviewActions(t *testing.T) {
	i := newKeymapInteractive(nil)
	i.runCtx = context.Background()
	i.runSlash(i.runCtx, "/wip")
	// The current off option is selected initially. Move up to on.
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyUp})
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyEnter})
	if i.settingsDialog.Active() || !i.agent.WIPState().Active {
		t.Fatal("on action did not enable WIP and close the subview")
	}
	i.runSlash(i.runCtx, "/wip")
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyDown})
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyEnter})
	if i.settingsDialog.Active() || i.agent.WIPState().Active {
		t.Fatal("off action did not disable WIP and close the subview")
	}
	i.agent.SetWIP(true)
	i.agent.WIPSettings.DonePrompt = "clean up the draft"
	i.busy = true
	i.runSlash(i.runCtx, "/wip")
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyDown})
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyDown})
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyEnter})
	if i.settingsDialog.Active() || i.agent.WIPState().Active {
		t.Fatal("done action did not disable WIP and close the subview")
	}
	prompts := i.agent.PendingQueuedMessages()
	if len(prompts) != 1 || prompts[0] != "clean up the draft" {
		t.Fatalf("done queued %v", prompts)
	}
}

func TestWIPMarkersFollowLiveState(t *testing.T) {
	i := newKeymapInteractive(nil)
	i.runCtx = context.Background()
	i.agent.SetWIP(true)
	i.agent.SetMessages([]provider.Message{{Role: provider.RoleUser, WIP: true, Content: []provider.Content{provider.TextBlock{Text: "draft"}}}})
	chat := strings.Join(i.cachedChatLocked(80), "\n")
	if strings.Count(chat, "wip start") != 1 || strings.Contains(chat, "wip end") {
		t.Fatal("active WIP phase has incorrect boundaries")
	}
	i.runSlash(i.runCtx, "/wip off")
	chat = strings.Join(i.cachedChatLocked(80), "\n")
	if strings.Count(chat, "wip start") != 1 || strings.Count(chat, "wip end") != 1 {
		t.Fatal("disabling WIP did not close the phase without another prompt")
	}
}

func TestWIPSubviewCtrlCCancels(t *testing.T) {
	i := newKeymapInteractive(nil)
	i.runCtx = context.Background()
	i.runSlash(i.runCtx, "/wip")
	i.handleKey(i.runCtx, tui.Key{Kind: tui.KeyCtrlC})
	if i.settingsDialog.Active() || i.agent.WIPState().Active {
		t.Fatal("Ctrl+C did not cancel the WIP subview")
	}
}
