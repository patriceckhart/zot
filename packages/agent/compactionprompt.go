package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveCompactionInstructions loads optional persistent instructions before
// per-run additions. Missing files are normal, other read failures are errors.
func resolveCompactionInstructions(zotHome string, additions []string) (string, error) {
	var parts []string
	if zotHome != "" {
		path := filepath.Join(zotHome, "COMPACT.md")
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("read compaction instructions %s: %w", path, err)
		}
		if text := strings.TrimSpace(string(raw)); text != "" {
			parts = append(parts, text)
		}
	}
	for _, addition := range additions {
		if text := strings.TrimSpace(addition); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}
