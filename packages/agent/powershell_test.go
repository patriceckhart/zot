package agent

import (
	"testing"

	"github.com/patriceckhart/zot/packages/agent/tools"
)

func TestPowerShellConfigToolSelection(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name, goos string
		preference *bool
		args       Args
		want       bool
	}{
		{"default", "windows", nil, Args{}, false},
		{"enabled", "windows", &enabled, Args{}, true},
		{"disabled", "windows", &disabled, Args{}, false},
		{"other OS", "darwin", &enabled, Args{}, false},
		{"no tools", "windows", &enabled, Args{NoTools: true}, false},
		{"explicit exclusion", "windows", &enabled, Args{Tools: []string{"read"}}, false},
		{"explicit inclusion", "windows", &disabled, Args{Tools: []string{"powershell"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := buildConfiguredToolRegistry(tc.args, t.TempDir(), nil, Config{PowerShellEnabled: tc.preference}, tc.goos)
			if (reg["powershell"] != nil) != tc.want {
				t.Fatalf("registry: %v", reg)
			}
			if tc.name == "enabled" && reg["bash"] == nil {
				t.Fatal("setting removed bash")
			}
		})
	}
}

func TestPowerShellSettingPersistence(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		if err := (configSettingsStore{}).SetPowerShell(enabled); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PowerShellEnabled == nil || *cfg.PowerShellEnabled != enabled || cfg.Theme != "dark" {
			t.Fatalf("config not preserved: %+v", cfg)
		}
		reg := buildConfiguredToolRegistry(Args{}, t.TempDir(), nil, cfg, "windows")
		if (reg["powershell"] != nil) != enabled {
			t.Fatal("saved preference not applied")
		}
	}
}

func TestPowerShellToolSelection(t *testing.T) {
	cwd := t.TempDir()
	sb := tools.NewSandbox(cwd)
	if _, ok := buildToolRegistry(Args{}, cwd, sb)["powershell"]; ok {
		t.Fatal("PowerShell enabled by default")
	}
	args, err := ParseArgs([]string{"--tools", "read,powershell,glob"})
	if err != nil {
		t.Fatal(err)
	}
	reg := buildToolRegistry(args, cwd, sb)
	ps, ok := reg["powershell"].(*tools.PowerShellTool)
	if !ok || ps.CWD != cwd || ps.Sandbox != sb {
		t.Fatalf("tool: %#v", reg["powershell"])
	}
	if len(reg) != 3 || reg["bash"] != nil {
		t.Fatalf("registry: %v", reg)
	}
	found := false
	for _, summary := range toolSummaries(reg, args) {
		if summary.Name == "powershell" {
			found = true
		}
	}
	if !found {
		t.Fatal("PowerShell missing from prompt summaries")
	}
	replacement := tools.NewSandbox(cwd)
	r := &Resolved{ToolRegistry: reg, Sandbox: sb}
	r.UseSandbox(replacement)
	if ps.Sandbox != replacement {
		t.Fatal("sandbox not replaced")
	}
	if len(buildToolRegistry(Args{NoTools: true, Tools: []string{"powershell"}}, cwd, sb)) != 0 {
		t.Fatal("--no-tools ignored")
	}
	both := buildToolRegistry(Args{Tools: []string{"bash", "powershell"}}, cwd, sb)
	if both["bash"] == nil || both["powershell"] == nil {
		t.Fatal("cannot select both shells")
	}
}
