package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShellStructuredUTF8Boundaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	cwd := t.TempDir()
	output := strings.Repeat("€", maxScriptBashBytes) + "END"
	if err := os.WriteFile(filepath.Join(cwd, "output.txt"), []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := (&BashTool{CWD: cwd}).Execute(context.Background(), json.RawMessage(`{"command":"cat output.txt"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Output    string
		Truncated bool
		Path      string `json:"full_output_path"`
	}
	if json.Unmarshal(result.StructuredContent, &value) != nil {
		t.Fatal("invalid structured JSON")
	}
	t.Cleanup(func() { os.Remove(value.Path) })
	if !value.Truncated || !utf8.ValidString(value.Output) || strings.Contains(value.Output, "\ufffd") || !strings.HasSuffix(value.Output, "END") || !strings.Contains(value.Output, "bytes omitted") {
		t.Fatal("structured output lost the tail or split UTF-8")
	}
}

func TestShellCancellationDoesNotResolveAsStructuredExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := (&BashTool{CWD: t.TempDir()}).Execute(ctx, json.RawMessage(`{"command":"printf ready\nexec sleep 60"}`), func(string) { cancel() })
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Status != "cancelled" || len(result.StructuredContent) != 0 {
		t.Fatalf("cancellation resolved as exit data: %+v", result)
	}
}
