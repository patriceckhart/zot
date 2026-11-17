package agent

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pendingExtensionUpdate creates an installed extension with an upstream commit
// that has not yet been pulled. It returns the file that the update should add.
func pendingExtensionUpdate(t *testing.T) string {
	t.Helper()
	gitAvailable(t)
	home := makeFakeZotHome(t)
	t.Setenv("ZOT_HOME", home)
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	initBareRepo(t, remote)
	source := filepath.Join(tmp, "source")
	initWorkRepo(t, source)
	writeFile(t, filepath.Join(source, "extension.json"), `{"name":"test","exec":"./x"}`)
	mustRun(t, source, "git", "add", "extension.json")
	mustRun(t, source, "git", "commit", "-q", "-m", "add manifest")
	mustRun(t, source, "git", "remote", "add", "origin", remote)
	mustRun(t, source, "git", "push", "-u", "origin", "main")
	extDir := filepath.Join(home, "extensions", "test")
	mustRun(t, "", "git", "clone", "-q", remote, extDir)
	configRepo(t, extDir)
	writeFile(t, filepath.Join(source, "NEW.md"), "new upstream content\n")
	mustRun(t, source, "git", "add", "NEW.md")
	mustRun(t, source, "git", "commit", "-q", "-m", "update extension")
	mustRun(t, source, "git", "push", "origin", "main")
	return filepath.Join(extDir, "NEW.md")
}

func TestExtUpdateCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr bool
		updated bool
	}{
		{name: "update", updated: true},
		{name: "help", args: []string{"--help"}},
		{name: "short help", args: []string{"-h"}},
		{name: "help word", args: []string{"help"}},
		{name: "unknown flag", args: []string{"--unknown"}, wantErr: true},
		{name: "unexpected name", args: []string{"test"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := pendingExtensionUpdate(t)
			handled, err := runExtCommand(append([]string{"ext", "update"}, tc.args...), "dev")
			if !handled || (err != nil) != tc.wantErr {
				t.Fatalf("handled = %v, err = %v", handled, err)
			}
			_, err = os.Stat(file)
			if tc.updated {
				if err != nil {
					t.Fatalf("extension was not updated: %v", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("expected no extension update, stat error = %v", err)
			}
		})
	}
}

func TestUpdateCommandExtensionRefresh(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    string
		args       []string
		statusCode int
		wantErr    bool
		updated    bool
	}{
		{name: "current", version: "1.0.0", statusCode: 200, updated: true},
		{name: "newer local build", version: "1.1.0", statusCode: 200, updated: true},
		{name: "check current", version: "1.0.0", args: []string{"--check"}, statusCode: 200},
		{name: "check newer available", version: "0.9.0", args: []string{"--check"}, statusCode: 200},
		{name: "query failure", version: "1.0.0", statusCode: 500, wantErr: true},
		{name: "dev build", version: "dev", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := pendingExtensionUpdate(t)
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
				if tc.statusCode == 0 {
					t.Fatal("unexpected HTTP request")
				}
				return &http.Response{
					StatusCode: tc.statusCode,
					Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.0.0","html_url":"https://example.com/releases/tag/v1.0.0"}`)),
					Header:     make(http.Header),
				}, nil
			})
			handled, err := runUpdateCommand(append([]string{"update"}, tc.args...), tc.version)
			if !handled || (err != nil) != tc.wantErr {
				t.Fatalf("handled = %v, err = %v", handled, err)
			}
			_, err = os.Stat(file)
			if tc.updated {
				if err != nil {
					t.Fatalf("extension was not updated: %v", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("expected no extension update, stat error = %v", err)
			}
		})
	}
}
