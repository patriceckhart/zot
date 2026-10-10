package swarm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestImageStartupHelperProcess(t *testing.T) {
	if os.Getenv("ZOT_SWARM_IMAGE_HELPER") != "1" {
		return
	}
	if os.Getenv("ZOT_SWARM_PROMPT_STDIN") != "1" || os.Getenv("ZOT_SWARM_CREDENTIAL_STDIN") != "1" {
		os.Exit(2)
	}
	var input StartupInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		os.Exit(3)
	}
	img := swarmTestImage(t)
	if input.Prompt == nil || input.Prompt.Text != "private-initial-task" || len(input.Prompt.Images) != 1 || !bytes.Equal(input.Prompt.Images[0].Data, img.Data) || input.Value != "synthetic-credential" {
		os.Exit(4)
	}
	for _, value := range append(os.Args, os.Environ()...) {
		if strings.Contains(value, input.Prompt.Text) || strings.Contains(value, input.Value) || strings.Contains(value, base64.StdEncoding.EncodeToString(img.Data)) {
			os.Exit(5)
		}
	}
	fmt.Println(`{"type":"agent_stopped","data":{"reason":"completed"}}`)
	os.Exit(0)
}

func TestExecRunnerTransfersImagesAndCredentialOnSameStdin(t *testing.T) {
	t.Setenv("ZOT_SWARM_IMAGE_HELPER", "1")
	root := t.TempDir()
	a := &Agent{ID: "image-test", Task: "private-initial-task", Images: []provider.ImageBlock{swarmTestImage(t)}, Dir: root,
		SessionPath: filepath.Join(root, "session.json"), InboxPath: filepath.Join(root, "inbox"), EventLogPath: filepath.Join(root, "events.jsonl")}
	r := &execRunner{agent: a, Command: []string{os.Args[0], "-test.run=^TestImageStartupHelperProcess$"},
		resolveCredential: func(context.Context, string) (Credential, error) {
			return Credential{Value: "synthetic-credential", Method: "apikey"}, nil
		}}
	if err := r.Run(context.Background(), agentSink{a: a}); err != nil {
		t.Fatal(err)
	}
}
