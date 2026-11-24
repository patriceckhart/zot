package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestSkillPinsPersistIndependentScopes(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	for _, pin := range []struct {
		cwd, name string
		global    bool
	}{
		{a, "review", false}, {a, "review", true}, {b, "other", false},
	} {
		if err := toggleSkillPin(pin.cwd, pin.name, pin.global); err != nil {
			t.Fatal(err)
		}
	}
	if err := toggleSkillPin(a, "review", false); err != nil {
		t.Fatal(err)
	}
	pins, err := loadSkillPins(a)
	if err != nil || len(pins.Project) != 0 || !slices.Equal(pins.Global, []string{"review"}) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	pins, err = loadSkillPins(b)
	if err != nil || !slices.Equal(pins.Project, []string{"other"}) || !slices.Equal(pins.Global, []string{"review"}) {
		t.Fatalf("pins = %+v, %v", pins, err)
	}
	info, err := os.Stat(filepath.Join(ZotHome(), "skill-pins.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(a)
	if err != nil || len(entries) != 0 {
		t.Fatalf("project was modified: %v, %v", entries, err)
	}
}

func TestSkillPinsDirectoryAliases(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	root := t.TempDir()
	target, alias := filepath.Join(root, "target"), filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := toggleSkillPin(target, "review", false); err != nil {
		t.Fatal(err)
	}
	pins, err := loadSkillPins(alias)
	if err != nil || !slices.Equal(pins.Project, []string{"review"}) {
		t.Fatalf("alias pins = %+v, %v", pins, err)
	}
}

func TestSkillPinsRejectCorruptFileWithoutOverwriting(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	path := filepath.Join(ZotHome(), "skill-pins.json")
	original := []byte(`{"global":["review"],"projects":42}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	pins, err := loadSkillPins(t.TempDir())
	if err == nil || len(pins.Global) != 0 {
		t.Fatalf("pins = %+v, err = %v", pins, err)
	}
	if err := toggleSkillPin(t.TempDir(), "new", true); err == nil {
		t.Fatal("corrupt file overwritten")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("file changed: %q, %v", got, err)
	}
}

func TestPreloadSkillPinsFreshOnlyAndNoSkill(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".zot", "skills", "review")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\ndescription: Review things.\ndisable-model-invocation: true\n---\nInspect carefully.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := toggleSkillPin(cwd, "review", true); err != nil {
		t.Fatal(err)
	}
	if err := toggleSkillPin(cwd, "review", false); err != nil {
		t.Fatal(err)
	}
	if err := toggleSkillPin(cwd, "missing", false); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name            string
		fresh, disabled bool
	}{
		{"fresh", true, false}, {"resumed", false, false}, {"disabled", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			args := Args{CWD: cwd, WithSkills: true, NoExt: true, NoSkill: tt.disabled}
			got, instructions := preloadSkillPins(args, tt.fresh, "question", &diagnostics)
			if tt.fresh && !tt.disabled {
				if strings.Count(got, "# Skill: review") != 1 || !strings.Contains(instructions, "Inspect carefully.") || !strings.HasPrefix(got, instructions) {
					t.Fatal(got)
				}
				if !strings.Contains(diagnostics.String(), "missing") {
					t.Fatal("missing warning")
				}
			} else if got != "question" || instructions != "" || diagnostics.Len() != 0 {
				t.Fatalf("prompt = %q, warnings = %q", got, diagnostics.String())
			}
		})
	}
}

func TestOpenSessionFreshness(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	isolateSessionEnvironment(t)
	cwd := t.TempDir()
	r := Resolved{Provider: "test", Model: "test"}
	ag := core.NewAgent(nil, "test", "", nil)
	args := Args{CWD: cwd, Continue: true}
	sess, fresh, err := openOrCreateSessionState(args, r, ag, "test")
	if err != nil || !fresh {
		t.Fatalf("continue without history: fresh=%v, err=%v", fresh, err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "previous"}}}); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	sess, fresh, err = openOrCreateSessionState(args, r, ag, "test")
	if err != nil || fresh {
		t.Fatalf("continue with history: fresh=%v, err=%v", fresh, err)
	}
	sess.Close()
	// A persisted session compacted to no messages is still a resumed session.
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	empty, err := core.NewSessionAtPath(path, cwd, "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "old"}}}); err != nil {
		t.Fatal(err)
	}
	if err := empty.AppendCompaction(nil); err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	sess, fresh, err = openOrCreateSessionState(Args{CWD: cwd, Session: path}, r, ag, "test")
	if err != nil || fresh || len(ag.Messages()) != 0 {
		t.Fatalf("empty resume: fresh=%v, err=%v", fresh, err)
	}
	sess.Close()
	sess, fresh, err = openOrCreateSessionState(Args{CWD: cwd, Session: filepath.Join(t.TempDir(), "new.jsonl")}, r, ag, "test")
	if err != nil || !fresh {
		t.Fatalf("new explicit path: fresh=%v, err=%v", fresh, err)
	}
	sess.Close()
	_, fresh, err = openOrCreateSessionState(Args{NoSess: true}, r, ag, "test")
	if err != nil || !fresh {
		t.Fatalf("no persistence: fresh=%v, err=%v", fresh, err)
	}
}
