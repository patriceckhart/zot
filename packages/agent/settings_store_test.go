package agent

import "testing"

func TestConfigSettingsStorePersistsShowInstructionsAtStartup(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetShowInstructionsAtStartup(true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ShowInstructionsAtStartup == nil || !*cfg.ShowInstructionsAtStartup {
		t.Fatal("show_instructions_at_startup was not persisted as enabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigSettingsStorePersistsOpenRouterServerTools(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetOpenRouterServerTools(false); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenRouterServerToolsEnabled == nil || *cfg.OpenRouterServerToolsEnabled {
		t.Fatal("openrouter_server_tools_enabled was not persisted as disabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestOpenRouterServerToolsEnabledDefaultsOff(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{}); err != nil {
		t.Fatal(err)
	}
	if OpenRouterServerToolsEnabled() {
		t.Fatal("nil preference should default to disabled")
	}
}

func TestConfigSettingsStorePersistsJailByDefault(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetJailByDefault(true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JailByDefault == nil || !*cfg.JailByDefault {
		t.Fatal("jail_by_default was not persisted as enabled")
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestConfigSettingsStorePersistsAutoCompactThreshold(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetAutoCompactThreshold(70); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoCompactThreshold == nil || *cfg.AutoCompactThreshold != 70 {
		t.Fatalf("auto_compact_threshold = %v, want 70", cfg.AutoCompactThreshold)
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}
