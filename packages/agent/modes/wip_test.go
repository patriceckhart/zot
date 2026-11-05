package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/ext"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestWIPMenuOrder(t *testing.T) {
	names := commandNames(newSlashSuggester().matches("/"))
	for idx, name := range names {
		if name == "/swarm" {
			if idx+1 >= len(names) || names[idx+1] != "/wip" {
				t.Fatalf("/wip must appear directly after /swarm: %v", names)
			}
			return
		}
	}
	t.Fatal("/swarm is missing from the main menu")
}

func TestWIPSlashCommandsAndExtension(t *testing.T) {
	a := core.NewAgent(nil, "model", "system", nil)
	i := NewInteractive(InteractiveConfig{Agent: a})
	i.runCtx = context.Background()
	for _, command := range []string{"/wip on", "/wip status"} {
		i.runSlash(context.Background(), command)
		if !a.WIPState().Active {
			t.Fatal(command)
		}
	}
	i.runSlash(context.Background(), "/wip off")
	if a.WIPState().Active {
		t.Fatal("off failed")
	}
	i.runSlash(context.Background(), "/wip invalid")
	if a.WIPState().Active || !strings.Contains(i.statusErr, "usage") {
		t.Fatal("invalid command changed mode")
	}
	response := ext.SetWIP(true)
	i.applyExtensionCommandResponse("test", extproto.CommandResponseFromExt{Action: response.Action, Active: response.Active})
	if !a.WIPState().Active {
		t.Fatal("extension enable failed")
	}
	a.WIPSettings.DefaultOn = false
	i.runSlash(context.Background(), "/clear")
	if a.WIPState().Active {
		t.Fatal("clear ignored default")
	}
	s := newSlashSuggester()
	if !contains(commandNames(s.matches("/wi")), "/wip") {
		t.Fatal("missing suggestion")
	}
	if !strings.Contains(strings.Join(renderHelpBlock(tui.Dark, 80, false, true, nil), "\n"), "/wip") {
		t.Fatal("missing help")
	}
}

func TestWIPDoneQueuesConfiguredCleanupOnce(t *testing.T) {
	a := core.NewAgent(nil, "model", "system", nil)
	a.WIPSettings.DonePrompt = "clean draft leftovers"
	a.SetWIP(true)
	i := NewInteractive(InteractiveConfig{Agent: a})
	// An active run queues the cleanup rather than requiring terminal/provider IO.
	i.busy = true
	i.runSlash(context.Background(), "/wip done")
	if a.WIPState().Active {
		t.Fatal("done did not disable mode")
	}
	prompts := a.PendingQueuedMessages()
	if len(prompts) != 1 || prompts[0] != "clean draft leftovers" {
		t.Fatalf("queued=%v", prompts)
	}
}

func TestWIPJSONUserMessageIsClean(t *testing.T) {
	msg := provider.Message{Role: provider.RoleUser, WIP: true, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}
	event := EventToJSON(core.EvUserMessage{Message: msg})
	if event["wip"] != true {
		t.Fatal("missing WIP tag")
	}
}
