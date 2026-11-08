package docs

import (
	"embed"

	"github.com/patriceckhart/zot/internal/docsinstall"
)

// embeddedDocs holds repository references and a build-time website snapshot.
// Embedding the parent directory also works when generated website docs are absent.
//
//go:embed README.md LICENSE docs
var embeddedDocs embed.FS

// EnsureInstalled writes bundled docs into $ZOT_HOME/docs without downloading
// anything. Existing matching files are untouched. Unrelated files are retained.
func EnsureInstalled(zotHome string) (string, error) {
	return docsinstall.Install(zotHome, embeddedDocs)
}
