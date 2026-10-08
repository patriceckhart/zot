package continuous

import (
	"bytes"
	"context"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestCompactionPreservesImagesInKeptTail(t *testing.T) {
	ctx := context.Background()
	h, _ := compactionHarness(t, []scriptStep{{text: long(200)}, {text: "image answer"}})
	defer h.r.Close()
	c := h.root(t)
	if _, err := h.r.Submit(ctx, c.ID, "a", "first", long(200)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	img := attachmentImage(t)
	if _, err := h.r.SubmitWith(ctx, c.ID, "a", "image", "", SubmitOptions{Images: []provider.ImageBlock{img}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Compact(ctx, h.svc.currentEngine(), c.ID, "", 1200); err != nil {
		t.Fatal(err)
	}
	snap, _ := h.r.Snapshot(ctx)
	messages, err := ModelContext(ctx, snap, c.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachedImage(t, messages, img)
	var exported bytes.Buffer
	if err := h.r.ExportSession(ctx, c.ID, &exported); err != nil {
		t.Fatal(err)
	}
	_, restored := reopenProjection(t, exported.Bytes())
	assertAttachedImage(t, restored, img)
	if _, err := h.r.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
}
