package attachments

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/continuous"
)

func TestReadFilesIsBoundedAndHonorsJail(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("client content"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := ReadFiles(context.Background(), root, []string{"notes.txt"}, nil)
	if err != nil || len(files) != 1 || string(files[0].Data) != "client content" || files[0].Name != "notes.txt" {
		t.Fatalf("files: %v", err)
	}
	if _, err := ReadFiles(context.Background(), root, []string{root}, nil); err == nil {
		t.Fatal("directory admitted")
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	jail := tools.NewSandbox(root)
	jail.Lock()
	if _, err := ReadFiles(context.Background(), root, []string{outside}, jail); err == nil {
		t.Fatal("attachment escaped jail")
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := ReadFiles(context.Background(), root, []string{link}, jail); err == nil {
			t.Fatal("symlink escaped jail")
		}
	}
	if err := os.WriteFile(path, make([]byte, continuous.MaxAttachmentBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFiles(context.Background(), root, []string{path}, nil); err == nil {
		t.Fatal("oversized file admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadFiles(ctx, root, []string{path}, nil); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}
