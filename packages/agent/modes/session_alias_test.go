package modes

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestSessionRowIncludesAliasAndEditableTitle(t *testing.T) {
	row := formatSessionRowPlain(core.SessionSummary{Alias: "chatty-lake", Title: "Investigation", Provider: "test", Model: "test"}, 120)
	for _, want := range []string{"[chatty-lake]", "Investigation"} {
		if !strings.Contains(row, want) {
			t.Fatalf("row missing %q: %s", want, row)
		}
	}
}

func TestSessionTreeCanReadActiveNamedSession(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	s, _, _, err := core.OpenNamedSession(root, cwd, "active", "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "synthetic tree question"}}}); err != nil {
		t.Fatal(err)
	}
	d := &sessionTreeDialog{}
	if !d.OpenSessionFamily(root, cwd, s.Path) || len(d.items) == 0 {
		t.Fatal("writer lease prevented read-only tree inspection")
	}
}
