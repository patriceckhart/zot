package tools

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestResolvePowerShell(t *testing.T) {
	for _, tc := range []struct{ name, goos, available, want string }{
		{"preferred", "windows", "pwsh.exe", "pwsh.exe"},
		{"fallback", "windows", "powershell.exe", "powershell.exe"},
		{"missing", "windows", "", ""},
		{"unsupported", "linux", "pwsh.exe", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			got, err := resolvePowerShell(tc.goos, func(name string) (string, error) {
				calls = append(calls, name)
				if name == tc.available {
					return name, nil
				}
				return "", fmt.Errorf("not found")
			})
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v", got, err)
			}
			if tc.goos != "windows" && len(calls) != 0 {
				t.Fatal("searched on unsupported OS")
			}
			if tc.name == "preferred" && len(calls) != 1 {
				t.Fatal("did not prefer pwsh")
			}
		})
	}
}

func TestPowerShellArgs(t *testing.T) {
	command := "Write-Output 'Grüße 世界'\nWrite-Output \"quoted\""
	args := powerShellArgs(command)
	want := []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-OutputFormat", "Text", "-EncodedCommand"}
	if !reflect.DeepEqual(args[:len(args)-1], want) {
		t.Fatalf("args: %v", args)
	}
	data, err := base64.StdEncoding.DecodeString(args[len(args)-1])
	if err != nil {
		t.Fatal(err)
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	script := string(utf16.Decode(units))
	if !strings.HasSuffix(script, "\n"+command) || !strings.Contains(script, "UTF8Encoding") {
		t.Fatalf("script: %q", script)
	}
}

func TestPowerShellPermissions(t *testing.T) {
	sb := NewSandbox(t.TempDir())
	tool := &PowerShellTool{Sandbox: sb}
	if err := tool.checkPermission(""); err != nil {
		t.Fatal(err)
	}
	sb.Lock()
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"Get-Location"}`), nil); err == nil || !strings.Contains(err.Error(), "jailed") {
		t.Fatalf("got %v", err)
	}
	sb.Unlock()
	sb.Permissions = &PermissionSet{}
	for _, mode := range []string{"", "none", "allowlist", "invalid", "ask"} {
		sb.Permissions.Bash.Mode = mode
		sb.Permissions.Bash.Allow = []string{"Get-Location"}
		if err := tool.checkPermission("Get-Location"); (err == nil) != (mode == "ask") {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
}

func TestPowerShellInvalidArgs(t *testing.T) {
	tool := &PowerShellTool{}
	for _, raw := range []string{`{`, `{}`, `{"command":"  "}`, `{"command":123}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if runtime.GOOS != "windows" {
		_, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"Get-Location"}`), nil)
		if err == nil || !strings.Contains(err.Error(), "only available on Windows") {
			t.Fatalf("got %v", err)
		}
	}
}

func TestPowerShellExecution(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows execution")
	}
	if _, err := resolvePowerShell(runtime.GOOS, exec.LookPath); err != nil {
		t.Skip(err)
	}
	cwd := t.TempDir()
	tool := &PowerShellTool{CWD: cwd}
	for _, tc := range []struct {
		name, command, want string
		failure             bool
		timeout             int
	}{
		{"unicode and quoting", "Write-Output 'Grüße 世界'; Write-Output '\"quoted\"'", "Grüße 世界", false, 0},
		{"cwd", "(Get-Location).Path", "", false, 0},
		{"stderr", "[Console]::Error.WriteLine('stderr output')", "stderr output", false, 0},
		{"exit", "exit 7", "[exit 7]", true, 0},
		{"error", "throw 'failure'", "failure", true, 0},
		{"timeout", "Start-Sleep -Seconds 60", "", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var streamed strings.Builder
			res, err := tool.Execute(ctx, mustJSON(t, map[string]any{"command": tc.command, "timeout": tc.timeout}), func(s string) { streamed.WriteString(s) })
			if err != nil {
				t.Fatal(err)
			}
			text := res.Content[0].(provider.TextBlock).Text
			if res.IsError != tc.failure || !strings.Contains(text, tc.want) || !strings.HasPrefix(text, "PS> ") {
				t.Fatalf("result: %+v, %s", res, text)
			}
			if tc.name == "cwd" {
				// Windows temp paths can use short names or different casing
				// than Get-Location. Compare directory identity, not spelling.
				gotPath := strings.TrimSpace(streamed.String())
				gotInfo, err := os.Stat(gotPath)
				if err != nil {
					t.Fatalf("stat reported cwd %q (expected %q): %v", gotPath, cwd, err)
				}
				wantInfo, err := os.Stat(cwd)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(gotInfo, wantInfo) {
					t.Fatalf("reported cwd %q is not the expected directory %q", gotPath, cwd)
				}
			}
			if tc.name == "unicode and quoting" && !strings.Contains(streamed.String(), "Grüße 世界") {
				t.Fatalf("stream: %q", streamed.String())
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := tool.Execute(ctx, json.RawMessage(`{"command":"Get-Location"}`), nil)
	if err == nil && !res.IsError {
		t.Fatal("cancellation succeeded")
	}
}
