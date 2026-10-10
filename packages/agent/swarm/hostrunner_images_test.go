package swarm

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

type imageHostClient struct{ requests chan provider.Request }

func (imageHostClient) Name() string { return "image-host-test" }
func (c imageHostClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	select {
	case c.requests <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return (echoHostClient{}).Stream(ctx, req)
}

func TestHostRunnerTransfersAndPersistsTaskAndFollowUpImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := imageHostClient{requests: make(chan provider.Request, 3)}
	_, rt, dial := newHostFixtureWithClient(t, client)
	f := New(Config{Root: t.TempDir(), RepoRoot: t.TempDir(), NewRunner: NewHostRunnerFactory(dial, "image-workspace")})
	defer f.StopAll()
	img := swarmTestImage(t)
	original := bytes.Clone(img.Data)
	a, err := f.SpawnReq(ctx, SpawnRequest{Task: "initial image", Images: []provider.ImageBlock{img}})
	if err != nil {
		t.Fatal(err)
	}
	img.Data[0] = 0
	checkRequest := func(want string, image bool) {
		t.Helper()
		select {
		case req := <-client.requests:
			last := req.Messages[len(req.Messages)-1]
			var found bool
			var text string
			for _, content := range last.Content {
				switch block := content.(type) {
				case provider.TextBlock:
					text += block.Text
				case provider.ImageBlock:
					found = bytes.Equal(block.Data, original)
				}
			}
			if text != want || found != image {
				t.Fatalf("provider input mismatch: text=%q, image=%v", text, found)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	checkRequest("initial image", true)
	waitUntil(t, "initial image answer", func() bool { return strings.Contains(strings.Join(a.Transcript(), "\n"), "echo: initial image") })
	if err := f.SendUserTurnWithImages(a.ID, "follow-up image", []provider.ImageBlock{swarmTestImage(t)}); err != nil {
		t.Fatal(err)
	}
	checkRequest("follow-up image", true)
	waitUntil(t, "follow-up image answer", func() bool { return strings.Contains(strings.Join(a.Transcript(), "\n"), "echo: follow-up image") })
	id := a.Snapshot().ConversationID
	snap, err := rt.ConversationSnapshot(ctx, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	imageEntries := 0
	for _, entry := range snap.Entries {
		if entry.Type == "user" && len(entry.Images) > 0 {
			imageEntries++
			if !bytes.Equal(entry.Images[0].Data, original) {
				t.Fatal("persisted host image changed")
			}
		}
	}
	if imageEntries != 2 {
		t.Fatalf("persisted image turns = %d, want 2", imageEntries)
	}
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	resumed, err := f.Resume(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "resumed image conversation", func() bool { return resumed.Snapshot().ConversationID == id })
	if err := f.SendUserTurn(a.ID, "after resume"); err != nil {
		t.Fatal(err)
	}
	checkRequest("after resume", false)
}
