package swarm

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/provider"
)

func swarmTestImage(t *testing.T) provider.ImageBlock {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return provider.ImageBlock{MimeType: "image/png", Data: data.Bytes()}
}

func TestPromptFrameRoundTripAndValidation(t *testing.T) {
	img := swarmTestImage(t)
	original := bytes.Clone(img.Data)
	frame, err := EncodePrompt("describe\nshutdown\ncancel", []provider.ImageBlock{img})
	if err != nil {
		t.Fatal(err)
	}
	img.Data[0] = 0
	if strings.ContainsAny(frame, "\r\n") {
		t.Fatal("image prompt broke line framing")
	}
	prompt, err := DecodePrompt(frame)
	if err != nil || prompt.Text != "describe\nshutdown\ncancel" || len(prompt.Images) != 1 || !bytes.Equal(prompt.Images[0].Data, original) {
		t.Fatal("image prompt did not round-trip")
	}
	for _, input := range []string{
		`user-prompt null`, `user-prompt {}`, `user-prompt {"private":"invalid"`,
		`user-prompt {"text":"test","images":[{"mime_type":"image/png","data":"AAE="}]}`,
	} {
		if _, err := DecodePrompt(input); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid prompt accepted or payload exposed")
		}
	}
	if _, err := EncodePrompt("test", []provider.ImageBlock{{MimeType: "image/png", Data: make([]byte, continuous.MaxAttachmentBytes+1)}}); err == nil {
		t.Fatal("oversized image accepted")
	}
	if _, err := EncodePrompt("test", make([]provider.ImageBlock, continuous.MaxAttachments+1)); err == nil {
		t.Fatal("too many images accepted")
	}
	legacy, err := DecodePrompt("user old text")
	if err != nil || legacy.Text != "old text" || len(legacy.Images) != 0 {
		t.Fatal("legacy text prompt changed")
	}
}

func TestSpawnCopiesImagesAndResumeDoesNotResendTask(t *testing.T) {
	f := New(Config{Root: t.TempDir(), RepoRoot: t.TempDir(), NewRunner: func(*Agent) Runner {
		return RunnerFunc(func(ctx context.Context, _ Sink) error {
			<-ctx.Done()
			return ctx.Err()
		})
	}})
	defer f.StopAll()
	img := swarmTestImage(t)
	original := bytes.Clone(img.Data)
	a, err := f.SpawnReq(context.Background(), SpawnRequest{Task: "describe", Images: []provider.ImageBlock{img}})
	if err != nil {
		t.Fatal(err)
	}
	img.Data[0] = 0
	if len(a.Images) != 1 || !bytes.Equal(a.Images[0].Data, original) {
		t.Fatal("spawn retained caller-owned image bytes")
	}
	for _, arg := range defaultChildArgs("zot", a, a.SessionPath, a.InboxPath) {
		if arg == a.Task {
			t.Fatal("image startup task leaked into argv")
		}
	}
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	resumed, err := f.Resume(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resuming || len(resumed.Images) != 0 {
		t.Fatal("resume would resubmit initial images")
	}
}

func TestImageFollowUpCrossesInboxAsOneFrame(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "in.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	inbox := NewInbox(path)
	defer inbox.Close()
	f := New(Config{})
	f.agents["test"] = &Agent{ID: "test", inbox: inbox}
	img := swarmTestImage(t)
	if err := f.SendUserTurnWithImages("test", "describe\nshutdown", []provider.ImageBlock{img}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-ln.Lines():
		prompt, err := DecodePrompt(msg)
		if err != nil || prompt.Text != "describe\nshutdown" || len(prompt.Images) != 1 || !bytes.Equal(prompt.Images[0].Data, img.Data) {
			t.Fatal("inbox dropped or corrupted image bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("image follow-up not delivered")
	}
}
