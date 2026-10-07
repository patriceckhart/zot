package continuous

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

func TestAttachedDriverReleasesPromptWatches(t *testing.T) {
	for _, prompt := range []string{"hello", ""} {
		t.Run("prompt="+prompt, func(t *testing.T) {
			steps := make([]scriptStep, 20)
			for i := range steps {
				steps[i].text = "answer"
			}
			_, dial, parent := attachHarness(t, &effectTool{name: "effect"}, steps)
			ctx, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			client := dial()
			var conv Conversation
			if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
				t.Fatal(err)
			}
			driver := &AttachedDriver{Client: client, ConversationID: conv.ID}
			view := core.NewAgent(nil, "scripted", "", core.NewRegistry())
			for i := 0; i < len(steps); i++ {
				err := driver.Prompt(ctx, view, prompt, func(core.AgentEvent) {})
				if (err == nil) != (prompt != "") {
					t.Fatalf("prompt %d: %v", i, err)
				}
				deadline := time.Now().Add(time.Second)
				for {
					calls, watches := client.pending()
					if calls == 0 && watches == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("prompt %d left %d calls and %d watches registered", i, calls, watches)
					}
					runtime.Gosched()
				}
			}
		})
	}
}
