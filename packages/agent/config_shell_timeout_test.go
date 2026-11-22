package agent

import (
	"os"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/tools"
)

func TestShellTimeoutConfig(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{ShellTimeoutSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	if err := (configSettingsStore{}).SetInlineImages(false); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.ShellTimeoutSeconds != 30 {
		t.Fatalf("config = %+v, error = %v", cfg, err)
	}
	enabled := true
	cfg.PowerShellEnabled = &enabled
	for _, args := range []Args{{}, {Tools: []string{"bash", "powershell"}}, {Codemode: true}} {
		reg := buildConfiguredToolRegistry(args, t.TempDir(), nil, cfg, "windows")
		if reg["bash"].(*tools.BashTool).TimeoutSeconds != 30 {
			t.Fatal("bash lost configured timeout")
		}
		if reg["powershell"].(*tools.PowerShellTool).TimeoutSeconds != 30 {
			t.Fatal("PowerShell lost configured timeout")
		}
	}
	worker := (&repoWorkerTool{name: "bash", timeoutSeconds: cfg.ShellTimeoutSeconds}).tool(t.TempDir()).(*tools.BashTool)
	if worker.TimeoutSeconds != 30 {
		t.Fatal("worker lost configured timeout")
	}
}

func TestShellTimeoutConfigInvalid(t *testing.T) {
	for _, input := range []string{
		`{"shell_timeout_seconds":-1}`,
		`{"shell_timeout_seconds":9223372037}`,
		`{"shell_timeout_seconds":1.5}`,
		`{"shell_timeout_seconds":"30"}`,
	} {
		t.Run(input, func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			if err := os.WriteFile(ConfigPath(), []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("loaded invalid timeout")
			}
			if _, err := Resolve(Args{}, false); err == nil {
				t.Fatal("resolution ignored invalid timeout")
			}
		})
	}
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{ShellTimeoutSeconds: -1}); err == nil {
		t.Fatal("saved invalid timeout")
	}
}
