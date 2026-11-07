package docs

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// embeddedDocs holds repository references and a build-time website snapshot.
// Embedding the parent directory also works when generated website docs are absent.
//
//go:embed README.md LICENSE docs
var embeddedDocs embed.FS

func documentationFiles(source fs.FS) (map[string]string, error) {
	files := map[string]string{"README.md": "README.md", "index.md": ""}
	// Repository references take precedence over website pages of the same name.
	for _, pattern := range []string{"docs/website/*.md", "docs/*.md"} {
		names, err := fs.Glob(source, pattern)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			files[path.Base(name)] = name
		}
	}
	if _, err := fs.Stat(source, "docs/website/index.md"); err == nil {
		files["website-source.txt"] = "docs/website/SOURCE.txt"
		// The website and this repository share the same MIT license and author.
		files["website-license.txt"] = "LICENSE"
	}
	return files, nil
}

var docLinks = regexp.MustCompile(`\]\(((?:https://www\.zot\.sh)?/docs(?:/[a-z0-9-]+(?:\.md)?)?|docs/[a-z0-9-]+\.md|\.\./README\.md)(#[^)]*)?\)`)

func localDocLinks(data []byte, files map[string]string) []byte {
	return docLinks.ReplaceAllFunc(data, func(link []byte) []byte {
		parts := docLinks.FindSubmatch(link)
		target := strings.TrimPrefix(string(parts[1]), "https://www.zot.sh")
		name := path.Base(target)
		if target == "/docs" {
			name = "index.md"
		} else if !strings.HasSuffix(name, ".md") {
			name += ".md"
		}
		if _, ok := files[name]; !ok {
			return link
		}
		return []byte("](" + name + string(parts[2]) + ")")
	})
}

// EnsureInstalled writes bundled docs into $ZOT_HOME/docs without downloading
// anything. Existing matching files are untouched. Unrelated files are retained.
func EnsureInstalled(zotHome string) (string, error) {
	return installDocumentation(zotHome, embeddedDocs)
}

func installDocumentation(zotHome string, source fs.FS) (string, error) {
	dir := filepath.Join(zotHome, "docs")
	files, err := documentationFiles(source)
	if err != nil {
		return dir, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		var data []byte
		if files[name] != "" {
			data, err = fs.ReadFile(source, files[name])
			if err != nil {
				return dir, err
			}
		}
		if strings.HasSuffix(name, ".md") {
			data = localDocLinks(data, files)
		}
		if name == "index.md" {
			var catalog strings.Builder
			if len(data) == 0 {
				catalog.WriteString("# zot Documentation\n\nThis build includes repository reference docs only. Website documentation was not downloaded at build time.\n")
			}
			catalog.WriteString("\n## Bundled documentation\n\n")
			if _, ok := files["website-source.txt"]; ok {
				catalog.WriteString("Repository reference docs take precedence over website pages with the same name.\nWebsite-only pages may describe newer features. See [website-source.txt](website-source.txt)\nfor provenance and downloaded-content hashes.\n\n")
			}
			for _, other := range names {
				if other != "index.md" && strings.HasSuffix(other, ".md") {
					fmt.Fprintf(&catalog, "- [%s](%s)\n", other, other)
				}
			}
			data = append(data, catalog.String()...)
		}
		dst := filepath.Join(dir, name)
		if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return dir, fmt.Errorf("install documentation %s: %w", name, err)
		}
	}
	return dir, nil
}
