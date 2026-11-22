//go:build windows

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

// Embedded double quotes must reach cmd.exe unchanged. Go's default
// argument escaping turned them into \" which cmd does not understand.
func TestBashWindowsQuotedArguments(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "SKILL.md"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range [][]string{nil, {"cmd", "/C"}} {
		tool := &BashTool{CWD: dir, Shell: prefix}
		for _, tc := range []struct{ command, want string }{
			{`dir /s /b "` + dir + `\SKILL.md"`, "SKILL.md"},
			{`findstr /c:"hello world" "a b\SKILL.md"`, "hello world"},
			{`echo "x y" & echo done`, "done"},
		} {
			res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": tc.command}), nil)
			if err != nil {
				t.Fatal(err)
			}
			got := res.Content[0].(provider.TextBlock).Text
			if res.IsError || !strings.Contains(got, tc.want) {
				t.Errorf("%s: got\n%s", tc.command, got)
			}
		}
	}
}
