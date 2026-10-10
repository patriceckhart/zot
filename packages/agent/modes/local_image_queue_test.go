package modes

import (
	"bytes"
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

type imageQueueClient struct {
	requests chan provider.Request
	release  chan struct{}
	calls    atomic.Int32
}

func (*imageQueueClient) Name() string { return "image-queue-test" }
func (c *imageQueueClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	select {
	case c.requests <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c.calls.Add(1) == 1 {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	events := make(chan provider.Event, 1)
	events <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}
	close(events)
	return events, nil
}

func lastUserInput(req provider.Request) (string, []provider.ImageBlock) {
	for n := len(req.Messages) - 1; n >= 0; n-- {
		msg := req.Messages[n]
		if msg.Role != provider.RoleUser {
			continue
		}
		var text string
		var images []provider.ImageBlock
		for _, block := range msg.Content {
			switch b := block.(type) {
			case provider.TextBlock:
				text += b.Text
			case provider.ImageBlock:
				images = append(images, b)
			}
		}
		return text, images
	}
	return "", nil
}

func TestLocalQueueRetainsImagesAndSubmissionOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &imageQueueClient{requests: make(chan provider.Request, 4), release: make(chan struct{})}
	iv := NewInteractive(InteractiveConfig{Agent: core.NewAgent(client, "test", "", nil)})
	iv.runCtx = ctx
	iv.SubmitOrQueue("first", nil)
	select {
	case <-client.requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	iv.SubmitOrQueue("before image", nil)
	img := transferImage(t)
	original := bytes.Clone(img.Data)
	iv.SubmitOrQueue("image prompt", []provider.ImageBlock{img})
	iv.submitOrQueuePrompt(ctx, "after image")
	img.Data[0] = 0
	close(client.release)
	for _, want := range []string{"before image", "image prompt", "after image"} {
		select {
		case req := <-client.requests:
			text, images := lastUserInput(req)
			if text != want {
				t.Fatalf("last user input = %q, want %q", text, want)
			}
			if want == "image prompt" && (len(images) != 1 || !bytes.Equal(images[0].Data, original)) {
				t.Fatal("local queued image bytes were lost or mutated")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	waitAttachedIdle(t, iv, false)
}

func TestLocalQueuedImageSlideBackPreservesNewestFirst(t *testing.T) {
	iv := NewInteractive(InteractiveConfig{Agent: core.NewAgent(nil, "test", "", nil)})
	iv.busy = true
	iv.SubmitOrQueue("older text", nil)
	img := transferImage(t)
	iv.ed.SetValue("image [clipboard image #1]")
	iv.clipboardImages = []clipboardImageAttachment{{Marker: "[clipboard image #1]", Image: img}}
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if !iv.ed.IsEmpty() || len(iv.clipboardImages) != 0 {
		t.Fatal("successful local image submission did not clear input")
	}
	iv.SubmitOrQueue("newer text", nil)
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
	if iv.ed.Value() != "newer text" {
		t.Fatal("slide-back did not restore the newest prompt")
	}
	iv.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
	text, images := preparePromptWithClipboardImages(iv.ed.SubmitValue(), iv.clipboardImages)
	if text != "image" || len(images) != 1 || !bytes.Equal(images[0].Data, img.Data) || len(iv.queued) != 0 {
		t.Fatal("slide-back lost local image prompt")
	}
	if iv.agent.QueuedMessageCount() != 1 {
		t.Fatal("slide-back consumed the older agent-loop prompt first")
	}
}

func TestLocalImageQueueCancelledWithActiveTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &imageQueueClient{requests: make(chan provider.Request, 2), release: make(chan struct{})}
	iv := NewInteractive(InteractiveConfig{Agent: core.NewAgent(client, "test", "", nil)})
	iv.runCtx = ctx
	iv.SubmitOrQueue("first", nil)
	select {
	case <-client.requests:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	iv.SubmitOrQueue("image", []provider.ImageBlock{transferImage(t)})
	iv.CancelTurn()
	// The idle flag is published before transcript persistence and queue
	// cleanup finish. Wait for both parts of cancellation to settle.
	for {
		iv.mu.Lock()
		settled := !iv.busy && len(iv.queued) == 0
		iv.mu.Unlock()
		if settled {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("cancelled turn retained a queued image")
		}
		runtime.Gosched()
	}
	if client.calls.Load() != 1 {
		t.Fatal("cancelled turn executed a queued image")
	}
}
