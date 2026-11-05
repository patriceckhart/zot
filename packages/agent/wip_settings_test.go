package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestWIPSettingsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOT_HOME", home)
	fallback := core.WIPSettings{Instruction: "fallback"}
	got, err := loadWIPSettings(fallback)
	if err != nil || got != fallback {
		t.Fatalf("missing file: %+v %v", got, err)
	}
	path := filepath.Join(home, "settings.json")
	if err := os.WriteFile(path, []byte(`{"wip":{"defaultOn":true,"instruction":"custom","donePrompt":"cleanup"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = loadWIPSettings(fallback)
	if err != nil || !got.DefaultOn || got.Instruction != "custom" || got.DonePrompt != "cleanup" {
		t.Fatalf("settings: %+v %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"wip":{"instruction":"<wip-mode>"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWIPSettings(fallback); err == nil {
		t.Fatal("invalid instruction accepted")
	}
}
