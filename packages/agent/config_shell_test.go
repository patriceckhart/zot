package agent

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/tools"
)

func TestShellConfig(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	prefix := []string{"container", "exec", "-i", "-w", "/work", "mydev", "bash", "-c"}
	if err := SaveConfig(Config{Shell: prefix}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || !reflect.DeepEqual(cfg.Shell, prefix) {
		t.Fatalf("loaded shell = %q, error = %v", cfg.Shell, err)
	}
	// Ordinary settings updates must retain the argv prefix.
	if err := (configSettingsStore{}).SetInlineImages(false); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig()
	if err != nil || !reflect.DeepEqual(cfg.Shell, prefix) {
		t.Fatalf("updated shell = %q, error = %v", cfg.Shell, err)
	}
	for _, args := range []Args{{}, {Tools: []string{"bash"}}, {Codemode: true}} {
		reg := buildConfiguredToolRegistry(args, t.TempDir(), nil, cfg, runtime.GOOS)
		if !reflect.DeepEqual(reg["bash"].(*tools.BashTool).Shell, prefix) {
			t.Fatal("registry lost shell prefix")
		}
		prompt := BuildSystemPrompt(SystemPromptOpts{Tools: toolSummaries(reg, args), GOOS: "windows"})
		if strings.Contains(prompt, "cmd.exe /C") {
			t.Fatal("configured shell received default Windows shell guidance")
		}
	}
	if reg := buildConfiguredToolRegistry(Args{NoTools: true}, t.TempDir(), nil, cfg, runtime.GOOS); len(reg) != 0 {
		t.Fatal("shell config enabled disabled tools")
	}
	worker := (&repoWorkerTool{name: "bash", shell: prefix}).tool(t.TempDir()).(*tools.BashTool)
	if !reflect.DeepEqual(worker.Shell, prefix) {
		t.Fatal("worker lost shell prefix")
	}
}

func TestShellConfigInvalid(t *testing.T) {
	for _, input := range []string{`{"shell":"sh -c"}`, `{"shell":[""]}`, `{"shell":["sh","\u0000"]}`} {
		t.Run(input, func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			if err := os.WriteFile(ConfigPath(), []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid shell accepted")
			}
			if _, err := Resolve(Args{}, false); err == nil {
				t.Fatal("resolution ignored invalid shell")
			}
		})
	}
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Shell: []string{""}}); err == nil {
		t.Fatal("saved invalid shell")
	}
}

func TestStartupPreConfiguredShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{Shell: []string{"/bin/sh", "-c", `printf wrapper-ok`}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runZotfileStartupPre(context.Background(), "!ignored-command", t.TempDir(), nil, nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "wrapper-ok") {
		t.Fatalf("output = %q", out.String())
	}
}
