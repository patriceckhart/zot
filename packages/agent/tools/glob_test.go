package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestGlobBasic(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"main.go",
		"README.md",
		"pkg/util.go",
		"pkg/util_test.go",
		"pkg/sub/deep.go",
		"docs/index.html",
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &GlobTool{CWD: dir}

	// 1. Simple extension match across tree (no slash in pattern)
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.go"}), nil)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	for _, want := range []string{"main.go", "pkg/util.go", "pkg/util_test.go", "pkg/sub/deep.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in result, got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "README.md") || strings.Contains(text, "docs/index.html") {
		t.Errorf("unexpected file in result:\n%s", text)
	}

	// 2. Globstar match with path prefix
	res, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "pkg/**/*.go"}), nil)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}
	text = res.Content[0].(provider.TextBlock).Text
	if strings.Contains(text, "main.go") {
		t.Errorf("did not expect main.go in pkg/**/*.go, got:\n%s", text)
	}
	for _, want := range []string{"pkg/util.go", "pkg/util_test.go", "pkg/sub/deep.go"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in result, got:\n%s", want, text)
		}
	}

	// 3. Specific path segment
	res, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "pkg/*.go"}), nil)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}
	text = res.Content[0].(provider.TextBlock).Text
	if strings.Contains(text, "pkg/sub/deep.go") {
		t.Errorf("did not expect deep.go in single-star pkg/*.go, got:\n%s", text)
	}
	if !strings.Contains(text, "pkg/util.go") || !strings.Contains(text, "pkg/util_test.go") {
		t.Errorf("missing pkg/util.go in pkg/*.go, got:\n%s", text)
	}
}

func TestGlobBraceExpansion(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"a.go",
		"b.json",
		"c.ts",
		"d.txt",
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.WriteFile(p, []byte("content"), 0o644)
	}

	tool := &GlobTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.{go,json}"}), nil)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "b.json") {
		t.Errorf("expected a.go and b.json, got:\n%s", text)
	}
	if strings.Contains(text, "c.ts") || strings.Contains(text, "d.txt") {
		t.Errorf("unexpected files in result:\n%s", text)
	}
}

func TestGlobGitignore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := []string{
		"app.go",
		"error.log",
		"ignored/secret.go",
		"sub/normal.go",
		"sub/test.log",
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("content"), 0o644)
	}

	tool := &GlobTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "**/*"}), nil)
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if strings.Contains(text, "secret.go") || strings.Contains(text, "error.log") || strings.Contains(text, "test.log") {
		t.Errorf("result contains gitignored files:\n%s", text)
	}
	if !strings.Contains(text, "app.go") || !strings.Contains(text, "sub/normal.go") {
		t.Errorf("missing tracked files in:\n%s", text)
	}
}

func TestGlobNestedGitignore(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "vendor")
	os.MkdirAll(nested, 0o755)
	os.WriteFile(filepath.Join(nested, ".gitignore"), []byte("cached/\n"), 0o644)

	files := []string{
		"root.go",
		"vendor/keep.go",
		"vendor/cached/temp.go",
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("content"), 0o644)
	}

	tool := &GlobTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "**/*.go"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if strings.Contains(text, "temp.go") {
		t.Errorf("nested gitignore rule was not respected: %s", text)
	}
	if !strings.Contains(text, "root.go") || !strings.Contains(text, "vendor/keep.go") {
		t.Errorf("expected files missing: %s", text)
	}
}

func TestGlobHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"visible.txt",
		".hidden.txt",
		".config/settings.json",
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("content"), 0o644)
	}

	tool := &GlobTool{CWD: dir}

	// Default: skip hidden
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.*"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if strings.Contains(text, ".hidden.txt") || strings.Contains(text, "settings.json") {
		t.Errorf("hidden files should be skipped by default: %s", text)
	}
	if !strings.Contains(text, "visible.txt") {
		t.Errorf("visible.txt missing: %s", text)
	}

	// Hidden: true
	res, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "**/*", "hidden": true}), nil)
	if err != nil {
		t.Fatal(err)
	}
	text = res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(text, ".hidden.txt") || !strings.Contains(text, ".config/settings.json") {
		t.Errorf("hidden files missing when hidden=true: %s", text)
	}
}

func TestGlobSubdirectoryPath(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"root.txt",
		"sub/a.txt",
		"sub/b.txt",
		"other/c.txt",
	}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("content"), 0o644)
	}

	tool := &GlobTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.txt", "path": "sub"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if strings.Contains(text, "root.txt") || strings.Contains(text, "other/c.txt") {
		t.Errorf("result should only contain files from sub: %s", text)
	}
	if !strings.Contains(text, "sub/a.txt") || !strings.Contains(text, "sub/b.txt") {
		t.Errorf("sub files missing: %s", text)
	}
}

func TestGlobNoMatches(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)

	tool := &GlobTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.rs"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(provider.TextBlock).Text
	if text != "No files matched the pattern." {
		t.Fatalf("expected 'No files matched the pattern.', got: %q", text)
	}
	details := res.Details.(map[string]any)
	if details["matches"] != 0 {
		t.Fatalf("matches detail want 0, got %v", details["matches"])
	}
}

func TestGlobValidationErrors(t *testing.T) {
	dir := t.TempDir()
	tool := &GlobTool{CWD: dir}

	// Empty pattern
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": ""}), nil); err == nil {
		t.Fatal("expected error for empty pattern")
	}

	// Non-existent directory
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.go", "path": "nonexistent"}), nil); err == nil {
		t.Fatal("expected error for non-existent path")
	}

	// File instead of directory
	filePath := filepath.Join(dir, "file.txt")
	os.WriteFile(filePath, []byte("text"), 0o644)
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.txt", "path": "file.txt"}), nil); err == nil {
		t.Fatal("expected error for file passed as path")
	}
}

func TestGlobSandboxing(t *testing.T) {
	dir := t.TempDir()
	sandboxDir := filepath.Join(dir, "sandbox")
	outsideDir := filepath.Join(dir, "outside")
	os.MkdirAll(sandboxDir, 0o755)
	os.MkdirAll(outsideDir, 0o755)

	s := NewSandbox(sandboxDir)
	s.Lock()

	tool := &GlobTool{CWD: sandboxDir, Sandbox: s}

	// Inside sandbox should succeed
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.go"}), nil)
	if err != nil {
		t.Fatalf("expected success inside sandbox, got %v", err)
	}
	if res.Content[0].(provider.TextBlock).Text != "No files matched the pattern." {
		t.Fatalf("unexpected content: %v", res.Content[0])
	}

	// Outside sandbox should fail
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": "*.go", "path": outsideDir}), nil); err == nil {
		t.Fatal("expected sandboxing error for path outside sandbox")
	}
}
