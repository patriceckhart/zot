package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveCompactionInstructions loads an optional replacement template and
// separate per-run additions. Missing files are normal, other read failures are errors.
func resolveCompactionInstructions(zotHome string, additions []string) (string, string, error) {
	var prompt string
	var parts []string
	if zotHome != "" {
		path := filepath.Join(zotHome, "COMPACT.md")
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return "", "", fmt.Errorf("read compaction instructions %s: %w", path, err)
		}
		prompt = strings.TrimSpace(string(raw))
	}
	for _, addition := range additions {
		if text := strings.TrimSpace(addition); text != "" {
			parts = append(parts, text)
		}
	}
	return prompt, strings.Join(parts, "\n\n"), nil
}
