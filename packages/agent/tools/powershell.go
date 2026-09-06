package tools

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf16"

	"github.com/patriceckhart/zot/packages/core"
)

// PowerShellTool provides opt-in native command execution on Windows.
type PowerShellTool struct {
	CWD     string
	Sandbox *Sandbox
}

func (t *PowerShellTool) Name() string { return "powershell" }
func (t *PowerShellTool) Description() string {
	return "Run a native PowerShell command on Windows (pwsh.exe, falling back to powershell.exe), without profiles or interactive input. stdout+stderr merged. Optional timeout in seconds."
}
func (t *PowerShellTool) Schema() json.RawMessage { return json.RawMessage(bashSchema) }

func (t *PowerShellTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	return executeShell(ctx, raw, progress, t.CWD, "PS>", t.checkPermission, func(ctx context.Context, command string) (*exec.Cmd, error) {
		path, err := resolvePowerShell(runtime.GOOS, exec.LookPath)
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, path, powerShellArgs(command)...), nil
	})
}

func (t *PowerShellTool) checkPermission(_ string) error {
	// The existing shell heuristics parse neither PowerShell expressions nor
	// cmdlets. Do not treat them as enforcement for a different language.
	if t.Sandbox.Locked() {
		return fmt.Errorf("jailed: PowerShell execution is unavailable while jailed")
	}
	if t.Sandbox != nil && t.Sandbox.Permissions != nil {
		if strings.ToLower(strings.TrimSpace(t.Sandbox.Permissions.Bash.Mode)) != "ask" {
			return fmt.Errorf("permission denied: PowerShell requires bash permission mode ask; bash allowlists do not apply to PowerShell")
		}
	}
	return nil
}

func resolvePowerShell(goos string, lookPath func(string) (string, error)) (string, error) {
	if goos != "windows" {
		return "", fmt.Errorf("the powershell tool is only available on Windows")
	}
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := lookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("PowerShell not found: install PowerShell or add pwsh.exe or powershell.exe to PATH")
}

func powerShellArgs(command string) []string {
	// EncodedCommand preserves quotes, multiline scripts and Unicode through
	// Windows command-line parsing. PowerShell expects UTF-16LE here.
	script := "[Console]::OutputEncoding = $OutputEncoding = [System.Text.UTF8Encoding]::new($false)\n" + command
	units := utf16.Encode([]rune(script))
	buf := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(buf[i*2:], unit)
	}
	return []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-OutputFormat", "Text", "-EncodedCommand", base64.StdEncoding.EncodeToString(buf)}
}
