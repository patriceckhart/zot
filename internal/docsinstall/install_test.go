package docsinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestInstallWithOptionalWebsiteDocs(t *testing.T) {
	for _, withWebsite := range []bool{false, true} {
		name := "repository-only"
		if withWebsite {
			name = "with-website"
		}
		t.Run(name, func(t *testing.T) {
			source := fstest.MapFS{
				"README.md":          {Data: []byte("# Readme")},
				"LICENSE":            {Data: []byte("MIT license fixture")},
				"docs/extensions.md": {Data: []byte("# Repository extensions")},
			}
			if withWebsite {
				source["docs/website/index.md"] = &fstest.MapFile{Data: []byte("# Website index\n[Tools](/docs/tools)")}
				source["docs/website/tools.md"] = &fstest.MapFile{Data: []byte("# Website tools")}
				source["docs/website/extensions.md"] = &fstest.MapFile{Data: []byte("# Website extensions")}
				source["docs/website/SOURCE.txt"] = &fstest.MapFile{Data: []byte("website provenance")}
			}
			dir, err := Install(t.TempDir(), source)
			if err != nil {
				t.Fatal(err)
			}
			index, err := os.ReadFile(filepath.Join(dir, "index.md"))
			if err != nil || !strings.Contains(string(index), "](extensions.md)") {
				t.Fatalf("missing local catalog: %q, %v", index, err)
			}
			extensions, err := os.ReadFile(filepath.Join(dir, "extensions.md"))
			if err != nil || string(extensions) != "# Repository extensions" {
				t.Fatalf("repository reference not preferred: %q, %v", extensions, err)
			}
			if withWebsite {
				if !strings.Contains(string(index), "](tools.md)") {
					t.Fatal("website links not installed locally")
				}
				for _, name := range []string{"tools.md", "website-source.txt", "website-license.txt"} {
					if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
						t.Fatalf("missing %s: %v", name, err)
					}
				}
			} else {
				if !strings.Contains(string(index), "repository reference docs only") {
					t.Fatal("reduced bundle not explained in index")
				}
				if _, err := os.Stat(filepath.Join(dir, "website-source.txt")); !os.IsNotExist(err) {
					t.Fatalf("unexpected website metadata: %v", err)
				}
			}
		})
	}
}

func TestBuildWithoutGeneratedWebsiteDirectory(t *testing.T) {
	// Compile the real embed declaration in a fresh module without a website
	// directory. This catches patterns that require generated files to exist.
	dir := t.TempDir()
	for _, name := range []string{"docs.go", "README.md", "LICENSE", "docs/extensions.md", "internal/docsinstall/docsinstall.go"} {
		data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/patriceckhart/zot\n\ngo 1.25.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build without generated docs: %v\n%s", err, output)
	}
}
