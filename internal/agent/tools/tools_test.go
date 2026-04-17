package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/internal/provider"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ReadTool{CWD: dir}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "a.txt"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("got %q", got)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("1\n2\n3\n4\n5\n"), 0o644)
	tool := &ReadTool{CWD: dir}
	res, _ := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "a.txt", "offset": 2, "limit": 2}), nil)
	got := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(got, "2\t2") || !strings.Contains(got, "3\t3") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "1\t1") || strings.Contains(got, "4\t4") {
		t.Fatalf("leaked lines: %q", got)
	}
}

func TestReadBinaryRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "b.bin")
	os.WriteFile(p, []byte{0x00, 0x01, 0x02}, 0o644)
	tool := &ReadTool{CWD: dir}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "b.bin"}), nil); err == nil {
		t.Fatal("want binary rejection")
	}
}

func TestWriteCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	tool := &WriteTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"path": "sub/a.txt", "content": "hi"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sub", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hi" {
		t.Fatalf("got %q", string(b))
	}
}

func TestEditSingle(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("hello world\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"oldText": "world", "newText": "gopher"}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello gopher\n" {
		t.Fatalf("got %q", string(b))
	}
}

func TestEditMultiple(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("a\nb\nc\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": "a.txt",
		"edits": []map[string]any{
			{"oldText": "a", "newText": "A"},
			{"oldText": "c", "newText": "C"},
		},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "A\nb\nC\n" {
		t.Fatalf("got %q", string(b))
	}
}

func TestEditAmbiguous(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("x\nx\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"oldText": "x", "newText": "y"}},
	}), nil)
	if err == nil {
		t.Fatal("want ambiguous error")
	}
}

func TestEditPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("hello\r\nworld\r\n"), 0o644)
	tool := &EditTool{CWD: dir}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"oldText": "world", "newText": "gopher"}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello\r\ngopher\r\n" {
		t.Fatalf("got %q", string(b))
	}
}

func TestBashSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell only")
	}
	tool := &BashTool{CWD: t.TempDir()}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "echo hi"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(got, "hi") || !strings.Contains(got, "[exit 0]") {
		t.Fatalf("got %q", got)
	}
	if res.IsError {
		t.Fatal("unexpected error flag")
	}
}

func TestBashFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell only")
	}
	tool := &BashTool{CWD: t.TempDir()}
	res, _ := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "false"}), nil)
	if !res.IsError {
		t.Fatal("want error")
	}
	got := res.Content[0].(provider.TextBlock).Text
	if !strings.Contains(got, "[exit 1]") {
		t.Fatalf("got %q", got)
	}
}
