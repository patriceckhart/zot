package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testIndex = `[Home](https://www.zot.sh/docs)
[Quickstart](https://www.zot.sh/docs/quickstart)
[Tools](https://www.zot.sh/docs/tools)
[Tools again](https://www.zot.sh/docs/tools)
[Not a page](https://www.zot.sh/docs/../../escape)
[Extension](https://www.zot.sh/extensions/example)
[Other host](https://example.com/docs/foreign)
`

func TestPageNames(t *testing.T) {
	names, err := pageNames(testIndex)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"index.md", "quickstart.md", "tools.md"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if _, err := pageNames("<html>not the index</html>"); err == nil {
		t.Fatal("invalid index accepted")
	}
}

func TestRefresh(t *testing.T) {
	for _, failure := range []string{"", "http", "html", "large"} {
		t.Run(failure, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/llms.txt":
					fmt.Fprint(w, testIndex)
				case "/docs/tools.md":
					switch failure {
					case "http":
						http.Error(w, "not found", http.StatusNotFound)
					case "html":
						fmt.Fprint(w, "<html>error page</html>")
					case "large":
						fmt.Fprint(w, strings.Repeat("x", (2<<20)+1))
					default:
						fmt.Fprint(w, "# Tools\n")
					}
				default:
					fmt.Fprint(w, "# Documentation\n")
				}
			}))
			defer server.Close()
			out := t.TempDir()
			stale := filepath.Join(out, "old.md")
			if err := os.WriteFile(stale, []byte("old snapshot"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := refresh(context.Background(), server.Client(), server.URL, out, false, io.Discard)
			if failure != "" {
				if err == nil {
					t.Fatal("failed download accepted")
				}
				entries, readErr := os.ReadDir(out)
				if readErr != nil || len(entries) != 1 || entries[0].Name() != "old.md" {
					t.Fatalf("failed download changed snapshot: %v, %v", entries, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(stale); !os.IsNotExist(err) {
				t.Fatalf("stale page not removed: %v", err)
			}
			tools, err := os.ReadFile(filepath.Join(out, "tools.md"))
			if err != nil || string(tools) != "# Tools\n" {
				t.Fatalf("tools = %q, %v", tools, err)
			}
			source, err := os.ReadFile(filepath.Join(out, "SOURCE.txt"))
			if err != nil || !strings.Contains(string(source), "tools.md") || !strings.Contains(string(source), "SHA-256") {
				t.Fatalf("source = %q, %v", source, err)
			}
		})
	}
}

func TestOfflineFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "offline fixture", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	for _, state := range []string{"valid", "strict", "missing", "absent-directory", "corrupt", "partial", "manifest", "unverified", "timeout"} {
		t.Run(state, func(t *testing.T) {
			out := t.TempDir()
			if state == "absent-directory" {
				out = filepath.Join(out, "website")
			}
			if state != "missing" && state != "absent-directory" {
				files := map[string][]byte{}
				var source strings.Builder
				source.WriteString("Fixture\nSHA-256 of downloaded Markdown:\n")
				for _, name := range []string{"index.md", "quickstart.md", "tools.md"} {
					data := []byte("# " + name + "\n")
					files[name] = data
					fmt.Fprintf(&source, "%x  %s\n", sha256.Sum256(data), name)
				}
				files["SOURCE.txt"] = []byte(source.String())
				switch state {
				case "corrupt":
					files["tools.md"] = []byte("corrupted")
				case "partial":
					delete(files, "tools.md")
				case "manifest":
					files["SOURCE.txt"] = []byte("no hashes")
				case "unverified":
					files["extra.md"] = []byte("# Extra")
				}
				if err := writeSnapshot(out, files); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadDir(out)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			ctx := context.Background()
			if state == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 0)
				defer cancel()
			}
			var warnings bytes.Buffer
			err = refresh(ctx, server.Client(), server.URL, out, state != "strict", &warnings)
			wantFallback := state == "valid" || state == "timeout" || state == "missing" || state == "absent-directory"
			if (err == nil) != wantFallback {
				t.Fatalf("fallback success = %v, want %v, error: %v", err == nil, wantFallback, err)
			}
			if wantFallback && !strings.Contains(warnings.String(), "warning: docs refresh failed:") {
				t.Fatalf("fallback warning missing: %q", warnings.String())
			}
			after, err := os.ReadDir(out)
			if (err != nil && !os.IsNotExist(err)) || len(before) != len(after) {
				t.Fatal("failed download modified snapshot")
			}
			if (state == "missing" || state == "absent-directory") && !strings.Contains(warnings.String(), "repository documentation only") {
				t.Fatal("missing repository-only fallback warning")
			}
		})
	}
}

func TestDownloadCancellationAndWriteError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := download(ctx, http.DefaultClient, "http://127.0.0.1:1/page"); err == nil {
		t.Fatal("cancelled download succeeded")
	}
	out := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(out, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshot(out, map[string][]byte{"index.md": []byte("# Docs")}); err == nil {
		t.Fatal("write failure ignored")
	}
}
