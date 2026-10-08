package modes

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestAttachedImagesRejectedBeforeClearingOrQueueing(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, source := range []string{"editor", "bridge"} {
			name := source + "/idle"
			if busy {
				name = source + "/busy"
			}
			t.Run(name, func(t *testing.T) {
				agent := core.NewAgent(nil, "test-model", "", nil)
				history := attachedExecutionHistory()
				agent.SetMessages(history)
				iv := NewInteractive(InteractiveConfig{Agent: agent, PromptDriver: func(context.Context, *core.Agent, string, func(core.AgentEvent)) error {
					t.Error("image prompt submitted host work")
					return nil
				}})
				iv.runCtx = context.Background()
				iv.busy = busy
				if source == "editor" {
					text := "describe [clipboard image #1]"
					iv.ed.SetValue(text)
					iv.clipboardImages = []clipboardImageAttachment{{Marker: "[clipboard image #1]", Image: provider.ImageBlock{}}}
					iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
					if iv.ed.Value() != text || len(iv.clipboardImages) != 1 {
						t.Fatal("rejected image input was cleared")
					}
				} else {
					iv.SubmitOrQueue("describe this", []provider.ImageBlock{{}})
				}
				if !strings.Contains(iv.statusErr, "supports text only") {
					t.Fatalf("missing explanation: %q", iv.statusErr)
				}
				if agent.QueuedMessageCount() != 0 || len(iv.queued) != 0 {
					t.Fatal("image prompt queued without its image")
				}
				if !reflect.DeepEqual(agent.Messages(), history) || iv.busy != busy {
					t.Fatal("rejection changed the transcript or active turn")
				}
			})
		}
	}
}
