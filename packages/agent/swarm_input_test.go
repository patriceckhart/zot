package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestSwarmStartupInputCompatibilityAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, data                  string
		credential, prompt, invalid bool
	}{
		{name: "legacy", data: `{"value":"synthetic","method":"apikey","account_id":"account"}`, credential: true},
		{name: "prompt", data: `{"prompt":{"text":"stdin task"}}`, prompt: true},
		{name: "combined", data: `{"value":"synthetic","method":"apikey","prompt":{"text":"stdin task"}}`, credential: true, prompt: true},
		{name: "missing credential", data: `{}`, credential: true, invalid: true},
		{name: "missing prompt", data: `{}`, prompt: true, invalid: true},
		{name: "empty prompt", data: `{"prompt":{}}`, prompt: true, invalid: true},
		{name: "invalid", data: `{"private payload`, prompt: true, invalid: true},
		{name: "invalid image", data: `{"prompt":{"images":[{"mime_type":"image/png","data":"AAE="}]}}`, prompt: true, invalid: true},
		{name: "bounded", data: strings.Repeat(" ", swarm.MaxPromptWireBytes) + `{}`, prompt: true, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := Args{Prompt: "argv task"}
			p, err := readSwarmStartupInput(strings.NewReader(tc.data), tc.credential, tc.prompt, &args)
			if tc.invalid {
				if err == nil || strings.Contains(err.Error(), "private payload") {
					t.Fatal("invalid input accepted or payload exposed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.credential && args.inheritedCredential != "synthetic" {
				t.Fatal("credential was not inherited")
			}
			want := "argv task"
			if tc.prompt {
				want = "stdin task"
			}
			if p.Text != want {
				t.Fatal("startup task changed")
			}
		})
	}
}

func TestSwarmStartupImagesReachProviderAndSession(t *testing.T) {
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	input := swarm.StartupInput{Prompt: &swarm.Prompt{Text: "describe", Images: []provider.ImageBlock{{MimeType: "image/png", Data: pngData.Bytes()}}}}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	args := Args{}
	p, err := readSwarmStartupInput(bytes.NewReader(data), false, true, &args)
	if err != nil {
		t.Fatal(err)
	}
	client := &startCaptureClient{}
	ag := core.NewAgent(client, "test", "", core.NewRegistry())
	if err := ag.Prompt(context.Background(), p.Text, p.Images, nil); err != nil {
		t.Fatal(err)
	}
	checkImage := func(messages []provider.Message) {
		t.Helper()
		for _, msg := range messages {
			for _, content := range msg.Content {
				if img, ok := content.(provider.ImageBlock); ok && bytes.Equal(img.Data, pngData.Bytes()) {
					return
				}
			}
		}
		t.Fatal("image bytes missing")
	}
	checkImage(client.requests[0].Messages)
	path := filepath.Join(t.TempDir(), "swarm.jsonl")
	session, err := core.NewSessionAtPath(path, t.TempDir(), "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	WriteNewTranscript(ag, session, 0)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, messages, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	checkImage(messages)
}
