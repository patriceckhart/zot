package agent

import (
	"strings"
	"testing"
)

func TestBuildSystemPromptToolGuidance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{name: "no tools"},
		{name: "default tools", tools: []string{"read", "write", "edit", "bash", "glob"}},
		{name: "bash only", tools: []string{"bash"}},
		{name: "read only", tools: []string{"read"}},
		{name: "edit only", tools: []string{"edit"}},
		{name: "write only", tools: []string{"write"}},
		{name: "powershell only", tools: []string{"powershell"}},
		{name: "edit and write", tools: []string{"edit", "write"}},
		{name: "bash and edit", tools: []string{"bash", "edit"}},
		{name: "bash and write", tools: []string{"bash", "write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var summaries []ToolSummary
			available := map[string]bool{}
			for _, name := range tc.tools {
				summaries = append(summaries, ToolSummary{Name: name})
				available[name] = true
			}
			for _, goos := range []string{"linux", "windows"} {
				got := BuildSystemPrompt(SystemPromptOpts{Tools: summaries, GOOS: goos, ZotDocsDir: "/zot/docs"})
				for _, want := range []string{"Zot's own docs are installed under /zot/docs", "index.md", "Repository reference docs take precedence"} {
					if !strings.Contains(got, want) {
						t.Errorf("%s: missing docs guidance %q", goos, want)
					}
				}
				for _, check := range []struct {
					text string
					want bool
				}{
					{"use edit with verbatim oldText", available["edit"]},
					{"Use write when creating a file", available["write"]},
					{"Do not mutate files through", available["edit"] && available["write"] && available["bash"]},
					{"use the read tool", available["read"]},
					{"cmd.exe /C", goos == "windows" && available["bash"]},
					{"prefer the read and glob tools", goos == "windows" && available["bash"] && available["read"] && available["glob"]},
				} {
					if strings.Contains(got, check.text) != check.want {
						t.Errorf("%s: guidance %q present=%v, want %v:\n%s", goos, check.text, !check.want, check.want, got)
					}
				}
			}
		})
	}
}

func TestBuildSystemPromptCustomOmitsFileGuidance(t *testing.T) {
	for _, custom := range []string{"Custom instructions", ""} {
		got := BuildSystemPrompt(SystemPromptOpts{
			Custom: custom, CustomSet: true, GOOS: "linux",
			Tools: []ToolSummary{{Name: "read"}, {Name: "edit"}, {Name: "write"}, {Name: "bash"}},
		})
		for _, unwanted := range []string{"use edit with verbatim oldText", "Use write when creating a file", "Do not mutate files through"} {
			if strings.Contains(got, unwanted) {
				t.Errorf("custom prompt includes %q:\n%s", unwanted, got)
			}
		}
	}
}
