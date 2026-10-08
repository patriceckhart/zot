package continuous

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestHostTransfersAttachmentsAndAttachedViewReloadsImages(t *testing.T) {
	r, dial, hostCtx := attachHarness(t, &effectTool{name: "effect"}, []scriptStep{{text: "first answer"}, {text: "second answer"}})
	ctx, cancel := context.WithTimeout(hostCtx, 5*time.Second)
	defer cancel()
	client := dial()
	var conv Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	img := attachmentImage(t)
	var sub Submission
	if err := client.CallInto(ctx, "conversation.submit", map[string]any{
		"id": conv.ID, "content": "review", "request_id": "files",
		"files": []FileAttachment{{Name: "client-only.txt", Data: []byte("transferred file contents")}, {Name: "photo.png", Data: img.Data}},
	}, &sub); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WaitSubmission(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sub.Content, "transferred file contents") {
		t.Fatal("host did not receive file content")
	}
	snap, _ := r.Snapshot(ctx)
	messages, err := ModelContext(ctx, snap, conv.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, messages, img)
	d := &AttachedDriver{Client: client, ConversationID: conv.ID}
	view := core.NewAgent(nil, "scripted", "", nil)
	if _, err := d.Load(ctx, view); err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, view.Messages(), img)
	var user provider.Message
	if err := d.PromptWithImages(ctx, view, "", []provider.ImageBlock{img}, func(ev core.AgentEvent) {
		if e, ok := ev.(core.EvUserMessage); ok {
			user = e.Message
		}
	}); err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, []provider.Message{user}, img)
	lastUser := view.Messages()[2]
	assertAttachedImage(t, []provider.Message{lastUser}, img)
}
