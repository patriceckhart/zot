package agent

import (
	"strings"
	"testing"
	"time"
)

func TestBuildSystemPromptCustomOmitsBuiltInDocs(t *testing.T) {
	got := BuildSystemPrompt(SystemPromptOpts{
		CWD:        "/workspace",
		Custom:     "Custom instructions",
		Append:     []string{"Additional context"},
		Now:        time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC),
		ZotDocsDir: "/zot/docs",
	})

	if strings.Contains(got, "Zot's own docs") || strings.Contains(got, "/zot/docs") {
		t.Fatalf("custom prompt includes built-in docs guidance:\n%s", got)
	}
	for _, want := range []string{
		"Custom instructions",
		"Additional context",
		"Current date: 2026-08-06",
		"Current working directory: /workspace",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("custom prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildSystemPromptPlatform(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		wantHint  bool
		wantLabel string
	}{
		{"windows", true, "Platform: windows/amd64"},
		{"linux", false, "Platform: linux/amd64"},
		{"darwin", false, "Platform: darwin/amd64"},
	} {
		got := BuildSystemPrompt(SystemPromptOpts{CWD: "/w", Tools: []ToolSummary{{Name: "bash"}}, GOOS: tc.goos, GOARCH: "amd64"})
		if !strings.Contains(got, tc.wantLabel) {
			t.Errorf("%s: missing %q:\n%s", tc.goos, tc.wantLabel, got)
		}
		if strings.Contains(got, "cmd.exe /C") != tc.wantHint {
			t.Errorf("%s: windows shell hint present=%v, want %v", tc.goos, !tc.wantHint, tc.wantHint)
		}
	}
}

func TestBuildSystemPromptDefaultIncludesBuiltInDocs(t *testing.T) {
	got := BuildSystemPrompt(SystemPromptOpts{
		Tools:      []ToolSummary{{Name: "read"}},
		CWD:        "/workspace",
		Now:        time.Date(2026, time.August, 6, 0, 0, 0, 0, time.UTC),
		ZotDocsDir: "/zot/docs",
	})

	for _, want := range []string{"Zot's own docs are installed under /zot/docs", "Start with index.md", "Repository reference docs take precedence"} {
		if !strings.Contains(got, want) {
			t.Errorf("default prompt missing %q:\n%s", want, got)
		}
	}
}
