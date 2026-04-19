package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitFrontmatter(t *testing.T) {
	in := "---\nname: foo\ndescription: bar\n---\nbody text\n"
	front, body := splitFrontmatter(in)
	if front != "name: foo\ndescription: bar" {
		t.Errorf("front = %q", front)
	}
	if body != "body text\n" {
		t.Errorf("body = %q", body)
	}

	front2, body2 := splitFrontmatter("no frontmatter here")
	if front2 != "" || body2 != "no frontmatter here" {
		t.Errorf("expected pass-through, got front=%q body=%q", front2, body2)
	}
}

func TestParseFrontmatter(t *testing.T) {
	front := `name: code-review
description: "Review a recent change."
allowed-tools: [read, bash]
permissions:
  bash: ["git diff*", "git log*"]
`
	s := &Skill{}
	parseFrontmatter(front, s)
	if s.Name != "code-review" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Description != "Review a recent change." {
		t.Errorf("description = %q", s.Description)
	}
	if got := s.AllowedTools; len(got) != 2 || got[0] != "read" || got[1] != "bash" {
		t.Errorf("allowed-tools = %v", got)
	}
	if got := s.Permissions["bash"]; len(got) != 2 || got[0] != "git diff*" || got[1] != "git log*" {
		t.Errorf("permissions[bash] = %v", got)
	}
}

func TestDiscoverProjectAndGlobalPriorityAndDedup(t *testing.T) {
	tmp := t.TempDir()
	zotHome := filepath.Join(tmp, "home")
	cwd := filepath.Join(tmp, "proj")

	mk := func(dir, name, desc string) {
		full := filepath.Join(dir, name)
		os.MkdirAll(full, 0o755)
		body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n# " + name + "\n"
		os.WriteFile(filepath.Join(full, "SKILL.md"), []byte(body), 0o644)
	}

	// Same skill name in BOTH project and global; project should win.
	mk(filepath.Join(cwd, ".zot", "skills"), "shared", "project version")
	mk(filepath.Join(zotHome, "skills"), "shared", "global version")
	// Unique skill in global only.
	mk(filepath.Join(zotHome, "skills"), "global-only", "from global")

	skills, errs := Discover(zotHome, cwd, "", true /* includeUser */)
	if len(errs) > 0 {
		t.Fatalf("errs: %v", errs)
	}
	// Expect the two user skills + every built-in shipped with the
	// binary (currently the write-zot-extension authoring guide).
	builtins := loadBuiltins()
	want := 2 + len(builtins)
	if len(skills) != want {
		t.Fatalf("expected %d skills (2 user + %d built-in), got %d (%v)", want, len(builtins), len(skills), skills)
	}
	shared := FindByName(skills, "shared")
	if shared == nil || shared.Description != "project version" {
		t.Errorf("expected project to win for 'shared', got %v", shared)
	}
	if FindByName(skills, "global-only") == nil {
		t.Errorf("global-only skill missing")
	}
	// At least one built-in should have made it through.
	for _, b := range builtins {
		if FindByName(skills, b.Name) == nil {
			t.Errorf("built-in skill %q missing from Discover output", b.Name)
		}
	}
}

func TestVisibleSkillsHidesBuiltins(t *testing.T) {
	in := []*Skill{
		{Name: "user-one"},
		{Name: "built-one", Builtin: true},
		{Name: "user-two"},
	}
	out := VisibleSkills(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 visible skills, got %d (%v)", len(out), out)
	}
	for _, s := range out {
		if s.Builtin {
			t.Errorf("built-in %q leaked into visible set", s.Name)
		}
	}
}

func TestSystemPromptAddendum(t *testing.T) {
	skills := []*Skill{
		{Name: "a", Description: "Do A."},
		{Name: "b", Description: "Do B."},
	}
	out := SystemPromptAddendum(skills)
	if want := "- a — Do A.\n- b — Do B.\n"; !contains(out, want) {
		t.Errorf("addendum missing entries:\n%s", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
