package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestFetchChangelogSinceVersionJump(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	var pages []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		mu.Lock()
		pages = append(pages, page)
		mu.Unlock()
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("missing page size")
		}
		var releases []changelogRelease
		switch page {
		case "1":
			w.Header().Set("Link", `<https://example.invalid/releases?page=2&per_page=100>; rel="next"`)
			releases = []changelogRelease{
				{TagName: "v0.4.23", Body: "future"},
				{TagName: "v0.4.22", HTMLURL: "https://example.invalid/v0.4.22", Body: "install instructions\n## Changelog\n### Fixed\n- latest fix"},
				{TagName: "v0.4.14", Body: "already shown"},
				{TagName: "v0.4.21", Draft: true, Body: "draft"},
				{TagName: "not-a-version", Body: "invalid"},
			}
		case "2":
			// Date order is deliberately different from version order.
			for i := 15; i <= 21; i++ {
				releases = append(releases, changelogRelease{TagName: fmt.Sprintf("v0.4.%d", i), Body: fmt.Sprintf("- change %d", i)})
			}
		default:
			t.Errorf("unexpected page %q", page)
		}
		if err := json.NewEncoder(w).Encode(releases); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	info, err := fetchChangelogSince(context.Background(), "0.4.22 (abc, date)", "v0.4.14", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "0.4.22" || info.URL != "https://example.invalid/v0.4.22" {
		t.Fatalf("unexpected metadata: %+v", info)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(pages, []string{"1", "2"}) {
		t.Fatalf("pages = %v", pages)
	}
	last := -1
	for i := 22; i >= 15; i-- {
		heading := fmt.Sprintf("\x00H:zot 0.4.%d\n", i)
		pos := strings.Index(info.Body, heading)
		if pos <= last {
			t.Fatalf("missing or out-of-order heading %q in %q", heading, info.Body)
		}
		last = pos
	}
	for _, excluded := range []string{"future", "already shown", "draft", "invalid", "install instructions"} {
		if strings.Contains(info.Body, excluded) {
			t.Errorf("included %q in %q", excluded, info.Body)
		}
	}
	if !strings.Contains(info.Body, "\x00H:Fixed") || !strings.Contains(info.Body, "latest fix") {
		t.Fatalf("lost current notes: %q", info.Body)
	}
}

func TestFetchChangelogSinceSingleUpgrade(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"v0.4.22","body":"- new"},{"tag_name":"v0.4.21","body":"- old"}]`)
	}))
	defer server.Close()
	info, err := fetchChangelogSince(context.Background(), "0.4.22", "0.4.21", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if info.Body != "\x00H:zot 0.4.22\n\n- new" {
		t.Fatalf("unexpected notes: %q", info.Body)
	}
}

func TestFetchChangelogSinceFailuresDiscardPartialNotes(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	for _, failure := range []string{"http", "json", "missing current", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "1" {
					if failure == "missing current" {
						fmt.Fprint(w, `[{"tag_name":"v0.4.21","body":"- older"}]`)
						return
					}
					w.Header().Set("Link", `<https://example.invalid>; rel="next"`)
					fmt.Fprint(w, `[{"tag_name":"v0.4.22","body":"- latest"}]`)
					if failure == "cancelled" {
						cancel()
					}
					return
				}
				if failure == "http" {
					w.WriteHeader(http.StatusServiceUnavailable)
				} else {
					fmt.Fprint(w, "invalid JSON")
				}
			}))
			defer server.Close()
			info, err := fetchChangelogSince(ctx, "0.4.22", "0.4.14", server.URL)
			if err == nil || info != (ChangelogInfo{}) {
				t.Fatalf("expected error and empty notes, got %+v, %v", info, err)
			}
		})
	}
}

func TestFetchChangelogSinceLocalAndEmptyBaseline(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	for _, tc := range []struct{ version, previous, path string }{
		{"0.0.0", "0.4.14", "/latest"},
		{"0.4.22", "", "/tags/v0.4.22"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.path)
				}
				fmt.Fprint(w, `{"tag_name":"v0.4.22","body":"- latest"}`)
			}))
			defer server.Close()
			info, err := fetchChangelogSince(context.Background(), tc.version, tc.previous, server.URL)
			if err != nil || info.Version != "0.4.22" || info.Body != "- latest" {
				t.Fatalf("unexpected result: %+v, %v", info, err)
			}
		})
	}
}

func TestChangelogEligibility(t *testing.T) {
	for _, tc := range []struct {
		current, previous string
		want              bool
	}{
		{"0.4.22", "", false},
		{"0.4.22", "0.4.22", false},
		{"0.4.22", "0.4.14", true},
		{"dev", "0.4.14", false},
		{"", "0.4.14", false},
		{"0.0.0", "0.4.14", true},
	} {
		if got := ShouldShowChangelog(tc.current, Config{LastChangelogShown: tc.previous}); got != tc.want {
			t.Errorf("ShouldShowChangelog(%q, %q) = %v, want %v", tc.current, tc.previous, got, tc.want)
		}
	}
	for _, previous := range []string{"0.4.22", "0.4.23"} {
		info, err := fetchChangelogSince(context.Background(), "0.4.22", previous, "invalid endpoint")
		if err != nil || info != (ChangelogInfo{}) {
			t.Fatalf("unchanged or downgraded version fetched notes: %+v, %v", info, err)
		}
	}
}
