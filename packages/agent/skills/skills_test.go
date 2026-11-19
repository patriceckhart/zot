package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
disable-model-invocation: true
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
	if !s.DisableModelInvocation {
		t.Error("disable-model-invocation was not parsed")
	}
	if got := s.AllowedTools; len(got) != 2 || got[0] != "read" || got[1] != "bash" {
		t.Errorf("allowed-tools = %v", got)
	}
	if got := s.Permissions["bash"]; len(got) != 2 || got[0] != "git diff*" || got[1] != "git log*" {
		t.Errorf("permissions[bash] = %v", got)
	}
}

func TestParseFrontmatterBlockDescription(t *testing.T) {
	for _, marker := range []string{">", ">- # summary", "|", "|+"} {
		t.Run(marker, func(t *testing.T) {
			front := "name: ponytail\ndescription: " + marker + `
  Lazy senior dev mode: the smallest change that fully solves the task,
  and a reply a busy human understands in one read.

disable-model-invocation: true
`
			s := &Skill{}
			parseFrontmatter(front, s)
			if want := "Lazy senior dev mode: the smallest change that fully solves the task, and a reply a busy human understands in one read."; s.Description != want {
				t.Errorf("description = %q, want %q", s.Description, want)
			}
			if !s.DisableModelInvocation {
				t.Error("field after block description was not parsed")
			}
		})
	}
}

func TestDiscoverProjectAndGlobalPriorityAndDedup(t *testing.T) {
	t.Setenv("ZOT_AGENT_SKILLS", "")
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

func TestDiscoverWithoutUserSkillsStillIncludesBuiltins(t *testing.T) {
	got, errs := Discover("", "", "", false)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	for _, builtin := range loadBuiltins() {
		if FindByName(got, builtin.Name) == nil {
			t.Errorf("built-in skill %q missing", builtin.Name)
		}
	}
}

func TestKebabCase(t *testing.T) {
	cases := map[string]string{
		"writing-git-commits": "writing-git-commits",
		"Writing Git Commits": "writing-git-commits",
		"git_commits":         "git-commits",
		"git.commits":         "git-commits",
		"Git/Commits":         "git-commits",
	}
	for in, want := range cases {
		if got := KebabCase(in); got != want {
			t.Errorf("KebabCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverSourcesFollowsSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory symlinks require elevated privileges on Windows")
	}

	target := t.TempDir()
	path := filepath.Join(target, "nested", "review")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\ndescription: linked\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "skills")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}

	got, errs := DiscoverSources([]Source{{Root: root, Prefix: "linked:"}}, false)
	if len(errs) != 0 || len(got) != 1 || got[0].Name != "linked:nested-review" {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
}

func TestDiscoverSourcesFollowsSymlinkedSkillDir(t *testing.T) {
	shared := t.TempDir()
	writeSkill(t, filepath.Join(shared, "tdd"), "name: tdd", "shared body")
	root := t.TempDir()
	linkDir(t, filepath.Join(shared, "tdd"), filepath.Join(root, "tdd"))

	got, errs := DiscoverSources([]Source{{Root: root, Label: "global"}}, false)
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
	if got[0].Name != "tdd" || got[0].Body != "shared body" || got[0].Source != "global" {
		t.Errorf("skill = %#v", got[0])
	}
	if want := filepath.Join(mustEvalSymlinks(t, root), "tdd", "SKILL.md"); got[0].Path != want {
		t.Errorf("path = %q, want the link location %q", got[0].Path, want)
	}
}

func TestDiscoverSourcesFollowsSymlinkedCategoryDir(t *testing.T) {
	shared := t.TempDir()
	writeSkill(t, filepath.Join(shared, "code-review"), "name: code-review", "named body")
	writeSkill(t, filepath.Join(shared, "prototype"), "", "unnamed body")
	root := t.TempDir()
	linkDir(t, shared, filepath.Join(root, "engineering"))

	got, errs := DiscoverSources([]Source{{Root: root}}, false)
	if len(errs) != 0 || len(got) != 2 {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
	if got[0].Name != "code-review" || got[1].Name != "engineering-prototype" {
		t.Errorf("names = %q, %q", got[0].Name, got[1].Name)
	}
}

func TestDiscoverSourcesSymlinkCycleTerminates(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "review"), "name: review", "body")
	linkDir(t, root, filepath.Join(root, "loop"))
	linkDir(t, root, filepath.Join(root, "alias"))

	got, errs := DiscoverSources([]Source{{Root: root}}, false)
	if len(errs) != 0 || len(got) != 1 || got[0].Name != "review" {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
}

func TestDiscoverSourcesIgnoresDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "review"), "name: review", "body")
	missing := filepath.Join(t.TempDir(), "gone")
	linkDir(t, missing, filepath.Join(root, "gone"))

	got, errs := DiscoverSources([]Source{{Root: root}}, false)
	if len(errs) != 0 || len(got) != 1 || got[0].Name != "review" {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
}

func TestDiscoverSourcesWalksLinkedDirOnce(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "shared")
	writeSkill(t, real, "name: review", "body")
	linkDir(t, real, filepath.Join(root, "alias"))

	got, errs := DiscoverSources([]Source{{Root: root}}, false)
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
	if want := filepath.Join(mustEvalSymlinks(t, root), "shared", "SKILL.md"); got[0].Path != want {
		t.Errorf("path = %q, want %q", got[0].Path, want)
	}
}

func TestDiscoverSourcesKeepsUnnamedRealSkillWhenAliasSortsFirst(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "z", "review")
	writeSkill(t, real, "", "body")
	linkDir(t, real, filepath.Join(root, "alias"))

	got, errs := DiscoverSources([]Source{{Root: root}}, false)
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
	if got[0].Name != "z-review" || got[0].Path != filepath.Join(mustEvalSymlinks(t, root), "z", "review", "SKILL.md") {
		t.Errorf("skill = %#v, want the real directory name and path", got[0])
	}
}

func TestDiscoverSourcesIgnoresCyclicSymlink(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "review"), "name: review", "body")
	linkDir(t, filepath.Join(root, "loop"), filepath.Join(root, "loop"))

	got, errs := DiscoverSources([]Source{{Root: root}}, false)
	if len(errs) != 0 || len(got) != 1 || got[0].Name != "review" {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
}

func TestSymlinkLoopError(t *testing.T) {
	if !symlinkLoopError(fmt.Errorf("stat: %w", syscall.ELOOP)) {
		t.Error("ELOOP should be ignored")
	}
	if runtime.GOOS == "windows" && !symlinkLoopError(fmt.Errorf("stat: %w", syscall.Errno(1921))) {
		t.Error("Windows cyclic-link error should be ignored")
	}
	if symlinkLoopError(fmt.Errorf("stat: %w", os.ErrPermission)) {
		t.Error("permission errors must still be reported")
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func writeSkill(t *testing.T, dir, front, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	frontmatter := "---\ndescription: test\n---\n"
	if front != "" {
		frontmatter = "---\n" + front + "\ndescription: test\n---\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(frontmatter+body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func linkDir(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func TestDiscoverSourcesRecursiveAndPrefixed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Something", "Writing Git Commits")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\ndescription: nested\n---\nbody"
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, errs := DiscoverSources([]Source{{Root: root, Label: "extension Git Tools", Prefix: "git-tools:"}}, false)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(got) != 1 || got[0].Name != "git-tools:something-writing-git-commits" {
		t.Fatalf("skills = %#v", got)
	}
	if got[0].Source != "extension Git Tools" || got[0].Body != "body" {
		t.Errorf("skill metadata = %#v", got[0])
	}
}

func TestDiscoverSourcesHomeDirectoryOverlap(t *testing.T) {
	t.Setenv("ZOT_AGENT_SKILLS", "")
	home := t.TempDir()
	for _, compat := range []string{".agents", ".claude"} {
		dir := filepath.Join(home, compat, "skills", "review")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+compat[1:]+"-review\n---\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, errs := DiscoverSources(SearchSources(t.TempDir(), home, home), false)
	if len(got) != 2 || len(errs) != 0 {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
	for _, s := range got {
		if !strings.HasPrefix(s.Source, "project") {
			t.Errorf("source = %q, want project precedence", s.Source)
		}
	}
}

func TestDiscoverSourcesSameFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: review\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, second := range []string{root, path} {
		got, errs := DiscoverSources([]Source{{Root: root, Label: "first"}, {Root: second, Label: "second"}}, false)
		if len(got) != 1 || len(errs) != 0 || got[0].Source != "first" {
			t.Fatalf("second=%q skills=%#v errs=%v", second, got, errs)
		}
	}
	got, errs := DiscoverSources([]Source{{Root: root, Prefix: "one:"}, {Root: root, Prefix: "two:"}}, false)
	if len(got) != 2 || len(errs) != 0 || got[0].Name != "one:review" || got[1].Name != "two:review" {
		t.Fatalf("namespaced skills=%#v errs=%v", got, errs)
	}
}

func TestDiscoverSourcesSameFileSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: review\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.Symlink(path, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	got, errs := DiscoverSources([]Source{{Root: root, Label: "first"}, {Root: alias}}, false)
	if len(got) != 1 || len(errs) != 0 || got[0].Source != "first" {
		t.Fatalf("skills=%#v errs=%v", got, errs)
	}
}

func TestDiscoverSourcesDuplicateDiagnostic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		dir := filepath.Join(root, "review")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\n---\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, errs := DiscoverSources([]Source{{Root: a}, {Root: b}}, false)
	if len(got) != 1 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "shadowed") {
		t.Fatalf("got=%#v errs=%v", got, errs)
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
		{Name: "built-a", Description: "Do A.", Builtin: true},
		{Name: "user-b", Description: "Do B.", Path: "/tmp/skills/user-b/SKILL.md"},
		{Name: "manual-c", Description: "Do C.", DisableModelInvocation: true},
	}
	out := SystemPromptAddendum(skills)
	if !contains(out, "- built-a [builtin]: Do A.\n") {
		t.Errorf("builtin entry missing or wrong:\n%s", out)
	}
	if !contains(out, "- user-b [/tmp/skills/user-b/SKILL.md]: Do B.\n") {
		t.Errorf("user entry missing path pointer:\n%s", out)
	}
	if contains(out, "manual-c") {
		t.Errorf("manual skill leaked into system prompt:\n%s", out)
	}
}

func TestSystemPromptAddendumEmptyWhenAllSkillsDisableModelInvocation(t *testing.T) {
	out := SystemPromptAddendum([]*Skill{{Name: "manual", DisableModelInvocation: true}})
	if out != "" {
		t.Fatalf("SystemPromptAddendum() = %q, want empty", out)
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
