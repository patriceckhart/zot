package modes

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func transferImage(t *testing.T) provider.ImageBlock {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return provider.ImageBlock{MimeType: "image/png", Data: buf.Bytes()}
}

type transferredPrompt struct {
	text   string
	images []provider.ImageBlock
}

func TestAttachedEditorTransfersFileAndClipboardImage(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("local-only file content"), 0o600); err != nil {
		t.Fatal(err)
	}
	img := transferImage(t)
	got := make(chan transferredPrompt, 1)
	iv := NewInteractive(InteractiveConfig{CWD: root, Agent: core.NewAgent(nil, "test", "", nil), PromptDriverWithImages: func(_ context.Context, _ *core.Agent, text string, images []provider.ImageBlock, _ func(core.AgentEvent)) error {
		got <- transferredPrompt{text, images}
		return nil
	}})
	iv.runCtx = context.Background()
	iv.ed.SetValue("review [file:notes.txt] [clipboard image #1]")
	iv.clipboardImages = []clipboardImageAttachment{{Marker: "[clipboard image #1]", Image: img}}
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	select {
	case p := <-got:
		if !strings.Contains(p.text, "local-only file content") || len(p.images) != 1 || !bytes.Equal(p.images[0].Data, img.Data) {
			t.Fatal("attachments lost before host driver")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt not transferred")
	}
	waitAttachedIdle(t, iv, false)
	if !iv.ed.IsEmpty() || len(iv.clipboardImages) != 0 {
		t.Fatal("successful input not cleared")
	}
	iv.ed.SetValue("review [file:missing.txt]")
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if iv.ed.Value() != "review [file:missing.txt]" || !strings.Contains(iv.statusErr, "attachment:") {
		t.Fatal("failed attachment was cleared or silently submitted")
	}
}

func TestAttachedQueueRetainsImagesAndPlainFollowUps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan transferredPrompt, 3)
	release := make(chan struct{})
	iv := NewInteractive(InteractiveConfig{Agent: core.NewAgent(nil, "test", "", nil), PromptDriverWithImages: func(ctx context.Context, _ *core.Agent, text string, images []provider.ImageBlock, _ func(core.AgentEvent)) error {
		got <- transferredPrompt{text, images}
		if text == "first" {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}})
	iv.runCtx = ctx
	iv.SubmitOrQueue("first", nil)
	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	img := transferImage(t)
	original := bytes.Clone(img.Data)
	iv.SubmitOrQueue("with image", []provider.ImageBlock{img})
	iv.SubmitOrQueue("plain", nil)
	img.Data[0] = 0
	close(release)
	for _, want := range []string{"with image", "plain"} {
		select {
		case p := <-got:
			if p.text != want {
				t.Fatalf("order: %q", p.text)
			}
			if want == "with image" && (len(p.images) != 1 || !bytes.Equal(p.images[0].Data, original)) {
				t.Fatal("queued image changed")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	waitAttachedIdle(t, iv, false)
	if iv.agent.QueuedMessageCount() != 0 {
		t.Fatal("attached input leaked into local agent queue")
	}
}

func TestAttachedExpandedPromptsUseHostQueue(t *testing.T) {
	for _, source := range []string{"expanded prompt", "study"} {
		t.Run(source, func(t *testing.T) {
			iv := NewInteractive(InteractiveConfig{CWD: t.TempDir(), Agent: core.NewAgent(nil, "test", "", nil), PromptDriverWithImages: func(context.Context, *core.Agent, string, []provider.ImageBlock, func(core.AgentEvent)) error {
				t.Error("queued prompt ran before current turn settled")
				return nil
			}})
			iv.busy = true
			if source == "study" {
				iv.runSlash(context.Background(), "/study explain this")
			} else {
				iv.submitOrQueuePrompt(context.Background(), "expanded prompt")
			}
			if len(iv.queued) != 1 || iv.queued[0].Text == "" || iv.agent.QueuedMessageCount() != 0 {
				t.Fatal("attached follow-up leaked into the local agent queue")
			}
		})
	}
}

func TestAttachedQueuedImagesCanBeEditedWithoutLocalExecution(t *testing.T) {
	iv := NewInteractive(InteractiveConfig{Agent: core.NewAgent(nil, "test", "", nil), PromptDriverWithImages: func(context.Context, *core.Agent, string, []provider.ImageBlock, func(core.AgentEvent)) error {
		t.Error("queued prompt ran before current turn settled")
		return nil
	}})
	iv.busy = true
	img := transferImage(t)
	iv.SubmitOrQueue("", []provider.ImageBlock{img})
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
	text, images := preparePromptWithClipboardImages(iv.ed.SubmitValue(), iv.clipboardImages)
	if text != "" || len(images) != 1 || !bytes.Equal(images[0].Data, img.Data) || len(iv.queued) != 0 {
		t.Fatal("editing lost image-only queued input")
	}
	iv.lastCtxInput = 200000
	if iv.shouldAutoCompactLocked() {
		t.Fatal("image driver enabled local compaction")
	}
	iv.runSlash(context.Background(), "/compact")
	if !strings.Contains(iv.statusErr, "attached") {
		t.Fatal("image driver allowed local compaction")
	}
}
