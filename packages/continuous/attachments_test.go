package continuous

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func attachmentImage(t *testing.T) provider.ImageBlock {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return provider.ImageBlock{MimeType: "image/png", Data: buf.Bytes()}
}

func assertAttachedImage(t *testing.T, messages []provider.Message, want provider.ImageBlock) {
	t.Helper()
	count := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			if img, ok := block.(provider.ImageBlock); ok {
				if !sameImages([]provider.ImageBlock{img}, []provider.ImageBlock{want}) {
					t.Fatal("image bytes changed")
				}
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("images=%d, want 1", count)
	}
}

func TestSubmissionAttachmentsSurviveRestartForkAndExport(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "store")
	open := func() *Runtime {
		s, err := journal.Open(ctx, dir, journal.Options{Durability: storage.Process})
		if err != nil {
			t.Fatal(err)
		}
		r, err := New(s)
		if err != nil {
			s.Close()
			t.Fatal(err)
		}
		return r
	}
	r := open()
	c, err := r.OpenRoot(ctx, "w", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	img := attachmentImage(t)
	opts := SubmitOptions{Images: []provider.ImageBlock{img}, Files: []FileAttachment{{Name: "notes.txt", Data: []byte("client-only file contents")}}}
	sub, err := r.SubmitWith(ctx, c.ID, "a", "stable", "review", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sub.Content, "client-only file contents") {
		t.Fatal("file not admitted")
	}
	// Admission owns copies, caller mutation must not change its records.
	img.Data[0] = 0
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = open()
	defer r.Close()
	img = attachmentImage(t)
	opts.Images = []provider.ImageBlock{img}
	retry, err := r.SubmitWith(ctx, c.ID, "a", "stable", "review", opts)
	if err != nil || retry.ID != sub.ID {
		t.Fatalf("retry: %v", err)
	}
	changed := opts
	changed.Images = []provider.ImageBlock{attachmentImage(t), attachmentImage(t)}
	if _, err := r.SubmitWith(ctx, c.ID, "a", "stable", "review", changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("image conflict: %v", err)
	}
	changed = opts
	changed.Files = []FileAttachment{{Name: "notes.txt", Data: []byte("changed")}}
	if _, err := r.SubmitWith(ctx, c.ID, "a", "stable", "review", changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("file conflict: %v", err)
	}
	snap, err := r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := ModelContext(ctx, snap, c.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, messages, img)
	client := &scriptedClient{steps: []scriptStep{{text: "done"}}}
	svc, err := NewService(r, EngineFunc(func(context.Context, Conversation) (*core.Agent, error) {
		return core.NewAgent(client, "test", "", nil), nil
	}), ExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, client.requests[0].Messages, img)
	child, err := r.Fork(ctx, c.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ = r.Snapshot(ctx)
	messages, err = ModelContext(ctx, snap, child.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, messages, img)
	var exported bytes.Buffer
	if err := r.ExportSession(ctx, c.ID, &exported); err != nil {
		t.Fatal(err)
	}
	_, restored := reopenProjection(t, exported.Bytes())
	assertAttachedImage(t, restored, img)
	imported := newTestRuntime(t)
	ic, err := imported.ImportSession(ctx, bytes.NewReader(exported.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	isnap, _ := imported.Snapshot(ctx)
	messages, err = ModelContext(ctx, isnap, ic.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, messages, img)
	if _, err := r.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
	format, _ := r.Format(ctx)
	if format.Version != 3 {
		t.Fatalf("format=%d", format.Version)
	}
}

func TestImageOnlySubmissionAndAttachmentValidation(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)
	c, _ := r.OpenRoot(ctx, "w", AgentConfig{})
	img := attachmentImage(t)
	if _, err := r.SubmitWith(ctx, c.ID, "a", "image-only", "", SubmitOptions{Images: []provider.ImageBlock{img}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []SubmitOptions{
		{Images: []provider.ImageBlock{{MimeType: "image/png", Data: []byte("invalid")}}},
		{Images: []provider.ImageBlock{{MimeType: "image/jpeg", Data: img.Data}}},
		{Files: []FileAttachment{{Name: "binary", Data: []byte{0}}}},
		{Files: []FileAttachment{{Name: "big", Data: make([]byte, MaxAttachmentBytes+1)}}},
		{Files: make([]FileAttachment, MaxAttachments+1)},
	} {
		before, _ := r.Snapshot(ctx)
		if _, err := r.SubmitWith(ctx, c.ID, "a", "invalid", "review", opts); err == nil {
			t.Fatal("invalid attachments admitted")
		}
		after, _ := r.Snapshot(ctx)
		if after.Revision() != before.Revision() {
			t.Fatal("invalid admission changed state")
		}
	}
}

func TestSteeredAttachmentsReachProviderOnce(t *testing.T) {
	ctx := context.Background()
	img := attachmentImage(t)
	tool := &effectTool{name: "effect", block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "effect", `{}`)}}, {text: "done"}}, tool)
	defer h.r.Close()
	c := h.root(t)
	if _, err := h.r.Submit(ctx, c.ID, "a", "first", "first"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		<-tool.started
		_, err := h.r.SubmitWith(ctx, c.ID, "b", "steer", "", SubmitOptions{Policy: PolicySteer, Images: []provider.ImageBlock{img}})
		done <- err
		close(tool.block)
	}()
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, h.client.requests[1].Messages, img)
	if _, err := h.r.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
}
