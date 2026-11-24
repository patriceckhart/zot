package skills

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPinsResolveUnionAndMissing(t *testing.T) {
	a := &Skill{Name: "a", Body: "first"}
	b := &Skill{Name: "ext:b", Body: "second", DisableModelInvocation: true}
	pins := Pins{Global: []string{"ext:b", "a", "missing"}, Project: []string{"a", "ext:b"}}
	selected, warnings := pins.Resolve([]*Skill{b, a})
	if len(selected) != 2 || selected[0] != a || selected[1] != b {
		t.Fatalf("selected = %#v", selected)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "missing") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestPreloadPromptPreservesSkillLocationAndRequest(t *testing.T) {
	s := &Skill{Name: "review", Body: "Inspect carefully.", Path: filepath.Join("skills", "review", "SKILL.md")}
	selected := []*Skill{s}
	got, instructions := PreloadPromptWithSnapshot(selected, "my request")
	if legacy := PreloadPrompt(selected, "my request"); legacy != got {
		t.Fatalf("PreloadPrompt = %q, want %q", legacy, got)
	}
	if !strings.HasPrefix(got, instructions) || strings.Contains(instructions, "my request") || !strings.Contains(instructions, "Skill directory: "+filepath.Dir(s.Path)) {
		t.Fatalf("invalid instruction snapshot: %q", instructions)
	}
	for _, want := range []string{"# Skill: review", "Inspect carefully.", "Skill directory: " + filepath.Dir(s.Path), "User request:\nmy request"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if got, instructions := PreloadPromptWithSnapshot(nil, "unchanged"); got != "unchanged" || instructions != "" {
		t.Fatal(got)
	}
}
