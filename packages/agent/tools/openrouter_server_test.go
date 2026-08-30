package tools

import "testing"

func TestOpenRouterServerToolsHaveStableNames(t *testing.T) {
	got := map[string]bool{}
	for _, tool := range OpenRouterServerTools() {
		name := tool.Name()
		if !IsOpenRouterServerTool(name) {
			t.Errorf("%q is not listed in OpenRouterServerToolNames", name)
		}
		if got[name] {
			t.Errorf("duplicate server tool %q", name)
		}
		got[name] = true
		if tool.Description() == "" {
			t.Errorf("%q has empty description", name)
		}
	}
	if len(got) != len(OpenRouterServerToolNames()) {
		t.Fatalf("got %d tools; want %d", len(got), len(OpenRouterServerToolNames()))
	}
}
