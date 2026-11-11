package tools

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfiguredShellHelper(t *testing.T) {
	if os.Getenv("ZOT_SHELL_HELPER") != "1" {
		return
	}
	cwd, _ := os.Getwd()
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Args []string
		CWD  string
	}{os.Args[3:], cwd})
	if os.Getenv("ZOT_SHELL_HELPER_BLOCK") == "1" {
		// Remain alive until cancellation. Output above signals readiness.
		<-time.After(time.Hour)
	}
	if os.Getenv("ZOT_SHELL_HELPER_FAIL") == "1" {
		os.Exit(23)
	}
	os.Exit(0)
}

func TestConfiguredShellExecute(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOT_SHELL_HELPER", "1")
	prefix := []string{exe, "-test.run=^TestConfiguredShellHelper$", "--", "argument with spaces", ""}
	original := append([]string(nil), prefix...)
	command := "printf '%s' \"$VALUE\"\nsecond command"
	tool := &BashTool{CWD: t.TempDir(), Shell: prefix, StartupFile: t.TempDir()}
	for _, fail := range []bool{false, true} {
		if fail {
			t.Setenv("ZOT_SHELL_HELPER_FAIL", "1")
		}
		var progress strings.Builder
		result, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": command}), func(chunk string) { progress.WriteString(chunk) })
		if err != nil {
			t.Fatal(err)
		}
		var structured struct {
			Output   string `json:"output"`
			ExitCode int    `json:"exit_code"`
		}
		if err := json.Unmarshal(result.StructuredContent, &structured); err != nil {
			t.Fatal(err)
		}
		var output struct {
			Args []string
			CWD  string
		}
		if err := json.Unmarshal([]byte(structured.Output), &output); err != nil {
			t.Fatal(err)
		}
		wantCWD, _ := os.Stat(tool.CWD)
		gotCWD, err := os.Stat(output.CWD)
		if err != nil || !os.SameFile(wantCWD, gotCWD) {
			t.Fatalf("cwd = %q, want %q", output.CWD, tool.CWD)
		}
		if !reflect.DeepEqual(output.Args, []string{"argument with spaces", "", command}) {
			t.Fatalf("arguments = %q", output.Args)
		}
		if result.IsError != fail || (fail && structured.ExitCode != 23) || (!fail && structured.ExitCode != 0) {
			t.Fatalf("result = %+v, exit = %d", result, structured.ExitCode)
		}
		if progress.String() != structured.Output {
			t.Fatal("progress differs from captured output")
		}
		if !reflect.DeepEqual(prefix, original) {
			t.Fatal("shell prefix was mutated")
		}
	}
	if !strings.Contains(tool.Description(), "configured shell argv") {
		t.Fatalf("description = %q", tool.Description())
	}
}

func TestConfiguredShellCancellation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOT_SHELL_HELPER", "1")
	t.Setenv("ZOT_SHELL_HELPER_BLOCK", "1")
	tool := &BashTool{CWD: t.TempDir(), Shell: []string{exe, "-test.run=^TestConfiguredShellHelper$", "--"}}
	for _, status := range []string{"cancelled", "timed_out"} {
		t.Run(status, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 0
			if status == "timed_out" {
				timeout = 1
			}
			result, err := tool.Execute(ctx, mustJSON(t, map[string]any{"command": "ignored", "timeout": timeout}), func(string) {
				if status == "cancelled" {
					cancel()
				}
			})
			if err != nil || !result.IsError || result.Status != status {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
		})
	}
}

func TestConfiguredShellValidation(t *testing.T) {
	for _, shell := range [][]string{nil, {}, {"sh", "-c"}, {"wrapper", ""}} {
		if err := ValidateShell(shell); err != nil {
			t.Fatalf("%q: %v", shell, err)
		}
	}
	for _, shell := range [][]string{{""}, {" "}, {"sh\x00"}, {"sh", "-c\x00"}} {
		tool := &BashTool{Shell: shell}
		if _, err := tool.newCommand(context.Background(), currentShell(), "echo ok"); err == nil {
			t.Fatalf("accepted invalid shell %q", shell)
		}
	}
}

func TestConfiguredShellErrors(t *testing.T) {
	tool := &BashTool{CWD: t.TempDir(), Shell: []string{"zot-nonexistent-shell-for-test"}}
	raw := mustJSON(t, map[string]any{"command": "echo ok"})
	if _, err := tool.Execute(context.Background(), raw, nil); err == nil || !strings.Contains(err.Error(), "start:") {
		t.Fatalf("missing executable error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tool.Shell = []string{exe}
	if _, err := tool.Execute(ctx, raw, nil); err == nil {
		t.Fatal("cancelled call started successfully")
	}
	tool.Sandbox = NewSandbox(tool.CWD)
	tool.Sandbox.Lock()
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "sudo true"}), nil); err == nil || !strings.Contains(err.Error(), "jail") {
		t.Fatalf("jail check error = %v", err)
	}
}
