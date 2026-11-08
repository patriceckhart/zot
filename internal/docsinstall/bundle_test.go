package docsinstall_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	docs "github.com/patriceckhart/zot"
	"github.com/patriceckhart/zot/internal/docsinstall"
)

// repository mirrors the embedded paths. The embed declaration itself is
// exercised through docs.EnsureInstalled and TestBuildWithoutGeneratedWebsiteDirectory.
var repository = os.DirFS(filepath.Join("..", ".."))

func TestEnsureInstalledIncludesEveryDocument(t *testing.T) {
	home := t.TempDir()
	dir, err := docs.EnsureInstalled(home)
	if err != nil {
		t.Fatal(err)
	}
	files, err := docsinstall.Files(repository)
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"docs/*.md", "docs/website/*.md"} {
		names, err := fs.Glob(repository, pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(dir, path.Base(name))); err != nil {
				t.Errorf("missing %s: %v", name, err)
			}
		}
	}
	for _, name := range []string{"providers.md", "zotfiles.md", "continuous.md", "index.md"} {
		if _, ok := files[name]; !ok {
			t.Errorf("missing required bundled file %s", name)
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range files {
		if strings.HasSuffix(name, ".md") && name != "index.md" && !strings.Contains(string(index), "]("+name+")") {
			t.Errorf("index missing %s", name)
		}
		if strings.HasPrefix(source, "docs/") && !strings.HasPrefix(source, "docs/website/") {
			original, _ := fs.ReadFile(repository, source)
			installed, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || !bytes.Equal(installed, docsinstall.LocalLinks(original, files)) {
				t.Errorf("repository reference not preferred for %s", name)
			}
		}
	}
}

func TestEnsureInstalledUpdatesAndPreservesFiles(t *testing.T) {
	home := t.TempDir()
	dir, err := docs.EnsureInstalled(home)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := filepath.Join(dir, "extensions.md")
	old := time.Unix(1000000000, 0)
	if err := os.Chtimes(unchanged, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(unchanged)
	if err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("outdated"), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "user-notes.md")
	if err := os.WriteFile(custom, []byte("user notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.EnsureInstalled(home); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(unchanged)
	if err != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("matching doc rewritten: %v", err)
	}
	if data, err := os.ReadFile(readme); err != nil || string(data) == "outdated" {
		t.Fatalf("outdated README retained: %v", err)
	}
	if data, err := os.ReadFile(custom); err != nil || string(data) != "user notes" {
		t.Fatalf("unrelated file modified: %v", err)
	}
}

func TestEnsureInstalledFilesystemErrors(t *testing.T) {
	for _, blocked := range []string{"docs", "docs/README.md"} {
		t.Run(blocked, func(t *testing.T) {
			home := t.TempDir()
			dst := filepath.Join(home, filepath.FromSlash(blocked))
			if blocked == "docs" {
				if err := os.WriteFile(dst, []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(dst, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := docs.EnsureInstalled(home); err == nil {
				t.Fatal("filesystem error ignored")
			}
		})
	}
}

func TestLocalDocLinks(t *testing.T) {
	files := map[string]string{"index.md": "", "tools.md": "", "README.md": ""}
	input := "[home](/docs) [tools](/docs/tools#bash) [web](https://www.zot.sh/docs/tools.md) [repo](docs/tools.md) [readme](../README.md#flags) [other](https://example.com/docs/tools) [missing](/docs/missing)"
	want := "[home](index.md) [tools](tools.md#bash) [web](tools.md) [repo](tools.md) [readme](README.md#flags) [other](https://example.com/docs/tools) [missing](/docs/missing)"
	if got := string(docsinstall.LocalLinks([]byte(input), files)); got != want {
		t.Fatalf("links = %s, want %s", got, want)
	}
}

func TestWebsiteSnapshotHashes(t *testing.T) {
	source, err := fs.ReadFile(repository, "docs/website/SOURCE.txt")
	if os.IsNotExist(err) {
		t.Skip("website documentation was not generated for this build")
	}
	if err != nil {
		t.Fatal(err)
	}
	names, _ := fs.Glob(repository, "docs/website/*.md")
	for _, name := range names {
		data, err := fs.ReadFile(repository, name)
		if err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("%x  %s", sha256.Sum256(data), path.Base(name))
		if !strings.Contains(string(source), line) {
			t.Errorf("snapshot hash missing or stale for %s", name)
		}
	}
}
