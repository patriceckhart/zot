package docs

import (
	"bytes"
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// embeddedDocs holds zot's user-facing documentation so installed
// binaries do not depend on a source checkout existing on disk.
//
//go:embed README.md docs/*.md
var embeddedDocs embed.FS

var docFiles = map[string]string{
	"README.md":     "README.md",
	"codemode.md":   "docs/codemode.md",
	"extensions.md": "docs/extensions.md",
	"rpc.md":        "docs/rpc.md",
	"skills.md":     "docs/skills.md",
	"themes.md":     "docs/themes.md",
}

// EnsureInstalled writes the embedded docs into $ZOT_HOME/docs and
// returns that directory. Existing files are left untouched when their
// content already matches the embedded copy.
func EnsureInstalled(zotHome string) (string, error) {
	dir := filepath.Join(zotHome, "docs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}

	names := make([]string, 0, len(docFiles))
	for name := range docFiles {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		src := docFiles[name]
		data, err := fs.ReadFile(embeddedDocs, src)
		if err != nil {
			return dir, err
		}
		dst := filepath.Join(dir, name)
		if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return dir, err
		}
	}
	return dir, nil
}
