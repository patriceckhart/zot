package agent

import "testing"

func TestClickToPositionPersistence(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.TUIClickToPosition {
		t.Fatalf("default should be disabled, error = %v", err)
	}
	for _, enabled := range []bool{true, false} {
		if err := (configSettingsStore{}).SetTUIClickToPosition(enabled); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TUIClickToPosition != enabled || cfg.Theme != "dark" {
			t.Fatal("setting not persisted or unrelated setting changed")
		}
	}
}
