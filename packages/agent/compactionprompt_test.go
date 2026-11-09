package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveCompactionInstructions(t *testing.T) {
	for _, state := range []string{"missing", "empty", "configured", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "COMPACT.md")
			if state == "unreadable" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if state != "missing" {
				text := " \n "
				if state == "configured" {
					text = "  Reference Markdown by path.\n"
				}
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prompt, got, err := resolveCompactionInstructions(home, []string{"  Keep blockers. ", " \n ", "Keep next steps."})
			if state == "unreadable" {
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("want contextual read error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "Keep blockers.\n\nKeep next steps."
			wantPrompt := ""
			if state == "configured" {
				wantPrompt = "Reference Markdown by path."
			}
			if prompt != wantPrompt {
				t.Fatalf("prompt = %q, want %q", prompt, wantPrompt)
			}
			if got != want {
				t.Fatalf("instructions = %q, want %q", got, want)
			}
		})
	}
}

func TestCompactionPromptFlag(t *testing.T) {
	args, err := ParseArgs([]string{"--append-compaction-prompt", "first", "--append-compaction-prompt", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args.AppendCompactionPrompt, []string{"first", "second"}) {
		t.Fatalf("additions = %v", args.AppendCompactionPrompt)
	}
	if _, err := ParseArgs([]string{"--append-compaction-prompt"}); err == nil {
		t.Fatal("missing value accepted")
	}
	opts, err := parseContinuousRunArgs([]string{"--store", t.TempDir(), "--append-compaction-prompt", "keep blockers", "task"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opts.hostArgs, []string{"--append-compaction-prompt", "keep blockers"}) {
		t.Fatalf("continuous host args = %v", opts.hostArgs)
	}
}

func TestResolveWiresCompactionInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "COMPACT.md"), []byte("Persistent summary guidance."), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(Args{Provider: "openai", Model: "gpt-5", APIKey: "synthetic-key", CWD: t.TempDir(), NoTools: true, NoSkill: true, NoContextFiles: true, AppendCompactionPrompt: []string{"Per-run guidance."}}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "Per-run guidance."
	if r.CompactionPrompt != "Persistent summary guidance." || r.NewAgent().CompactionPrompt != r.CompactionPrompt {
		t.Fatal("replacement compaction template not wired to agent")
	}
	if r.CompactionInstructions != want || r.NewAgent().CompactionInstructions != want {
		t.Fatal("compaction instructions not wired to agent")
	}
	if strings.Contains(r.SystemPrompt, "Persistent summary guidance.") || strings.Contains(r.SystemPrompt, "Per-run guidance.") {
		t.Fatal("summary instructions leaked into normal system prompt")
	}
}
