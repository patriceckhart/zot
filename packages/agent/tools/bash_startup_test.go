package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBashStartup(t *testing.T) {
	if runtime.GOOS == "windows" || !currentShell().isBash {
		t.Skip("requires Bash")
	}
	for _, tc := range []struct {
		name, startup, command, want string
		wantExit                     int
	}{
		{"alias", "alias la='printf alias-ok'\n", "la", "alias-ok", 0},
		{"alias in compound command", "alias la='printf alias-ok'\n", "if true\nthen\nla\nfi", "alias-ok", 0},
		{"startup output", "printf startup-output\n", "printf command-output", "startup-outputcommand-output", 0},
		{"function", "greet() { printf function-ok\n}\n", "greet", "function-ok", 0},
		{"environment", "export ZOT_STARTUP_TEST=environment-ok\n", "printf '%s' \"$ZOT_STARTUP_TEST\"", "environment-ok", 0},
		{"precedence", "export ZOT_STARTUP_TEST=\"${ZOT_STARTUP_TEST}-zot\"\n", "printf '%s' \"$ZOT_STARTUP_TEST\"", "inherited-zot", 0},
		{"noninteractive", "", "printf '%s' \"$-\"", "", 0},
		{"failure", "return 23\n", "printf should-not-run", "", 23},
		{"syntax error", "if then\n", "printf should-not-run", "", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			inherited := filepath.Join(dir, "inherited")
			if err := os.WriteFile(inherited, []byte("export ZOT_STARTUP_TEST=inherited\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BASH_ENV", inherited)
			path := filepath.Join(dir, "startup ' with spaces")
			if err := os.WriteFile(path, []byte(tc.startup), 0o600); err != nil {
				t.Fatal(err)
			}
			tool := &BashTool{CWD: dir, StartupFile: path}
			res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": tc.command}), nil)
			if err != nil {
				t.Fatal(err)
			}
			var output struct {
				Output   string `json:"output"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal(res.StructuredContent, &output); err != nil {
				t.Fatal(err)
			}
			// Bash versions differ in the status returned by source on syntax errors.
			if (tc.wantExit >= 0 && output.ExitCode != tc.wantExit) || (tc.wantExit < 0 && output.ExitCode == 0) || res.IsError != (tc.wantExit != 0) {
				t.Fatalf("result = %+v, error = %v", output, res.IsError)
			}
			if !strings.Contains(output.Output, tc.want) || strings.Contains(output.Output, "should-not-run") {
				t.Fatalf("output = %q, want %q", output.Output, tc.want)
			}
			if tc.name == "noninteractive" && strings.Contains(output.Output, "i") {
				t.Fatalf("shell is interactive: %q", output.Output)
			}
		})
	}
}

func TestBashStartupMissingAndInvalid(t *testing.T) {
	if runtime.GOOS == "windows" || !currentShell().isBash {
		t.Skip("requires Bash")
	}
	t.Setenv("BASH_ENV", "")
	dir := t.TempDir()
	tool := &BashTool{CWD: dir, StartupFile: filepath.Join(dir, "missing")}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "true"}), nil)
	if err != nil || res.IsError {
		t.Fatalf("missing startup: result = %+v, error = %v", res, err)
	}
	tool.StartupFile = dir
	_, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "true"}), nil)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory startup: error = %v", err)
	}
}

func TestBashStartupFreshShell(t *testing.T) {
	if runtime.GOOS == "windows" || !currentShell().isBash {
		t.Skip("requires Bash")
	}
	t.Setenv("BASH_ENV", "")
	dir := t.TempDir()
	path := filepath.Join(dir, ".bashrc")
	if err := os.WriteFile(path, []byte("alias la='printf startup-ok'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &BashTool{CWD: dir, StartupFile: path}
	for _, command := range []string{"unalias la", "la"} {
		res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": command}), nil)
		if err != nil || res.IsError {
			t.Fatalf("%s: result = %+v, error = %v", command, res, err)
		}
	}
}

func TestConfiguredLocalBashStartup(t *testing.T) {
	if !currentShell().isBash {
		t.Skip("requires local Bash")
	}
	t.Setenv("BASH_ENV", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "startup")
	if err := os.WriteFile(path, []byte("export ZOT_STARTUP_TEST=configured-local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &BashTool{CWD: dir, Shell: []string{currentShell().path, "-c"}, StartupFile: path}
	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "printf '%s' \"$ZOT_STARTUP_TEST\""}), nil)
	if err != nil || res.IsError || !strings.Contains(string(res.StructuredContent), "configured-local") {
		t.Fatalf("result = %+v, error = %v", res, err)
	}
}

func TestBashStartupOtherShellsUnchanged(t *testing.T) {
	tool := &BashTool{StartupFile: t.TempDir()}
	for _, shell := range []shellCommand{{path: "/bin/sh", flag: "-c"}, {path: "cmd", flag: "/C"}} {
		cmd, err := tool.newCommand(context.Background(), shell, "echo hello")
		if err != nil {
			t.Fatal(err)
		}
		if len(cmd.Args) != 3 || cmd.Args[2] != "echo hello" {
			t.Fatalf("shell arguments changed: %q", cmd.Args)
		}
	}
}
