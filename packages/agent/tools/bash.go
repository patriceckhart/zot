package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

const (
	maxBashLines = 2000
	maxBashBytes = 50 * 1024
)

// BashTool runs a shell command in the agent's cwd.
type BashTool struct {
	CWD     string
	Sandbox *Sandbox
}

type bashArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

const bashSchema = `{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer"}},"required":["command"]}`

func (t *BashTool) Name() string            { return "bash" }
func (t *BashTool) Description() string     { return shellDescription(currentShell()) }
func (t *BashTool) Schema() json.RawMessage { return json.RawMessage(bashSchema) }

func (t *BashTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	return executeShell(ctx, raw, progress, t.CWD, "$", func(command string) error {
		if err := t.Sandbox.CheckCommand(command); err != nil {
			return err
		}
		return t.Sandbox.CheckBashPermission(command)
	}, func(ctx context.Context, command string) (*exec.Cmd, error) {
		return newShellCmd(ctx, command), nil
	})
}

// executeShell shares output streaming, timeouts and result formatting across shells.
func executeShell(ctx context.Context, raw json.RawMessage, progress func(string), cwd, prompt string, check func(string) error, newCommand func(context.Context, string) (*exec.Cmd, error)) (core.ToolResult, error) {
	var a bashArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return core.ToolResult{}, fmt.Errorf("command is required")
	}
	if err := check(a.Command); err != nil {
		return core.ToolResult{}, err
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if a.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
		defer cancel()
	}

	start := time.Now()
	cmd, err := newCommand(runCtx, a.Command)
	if err != nil {
		return core.ToolResult{}, err
	}
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	// Bound output draining if descendants retain handles after cancellation.
	cmd.WaitDelay = 2 * time.Second
	setProcessGroup(cmd)

	// Capture merged stdout+stderr with line-by-line streaming.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return core.ToolResult{}, fmt.Errorf("start: %w", err)
	}

	// Writer to both the buffer (trimmed) and progress callback.
	captured := &bytes.Buffer{}
	done := make(chan struct{})

	// Watch for context cancellation and kill the entire process
	// group immediately. exec.CommandContext only kills the direct
	// process, but child processes (e.g. grep spawned by the shell)
	// keep the output pipe open and block cmd.Wait() indefinitely.
	go func() {
		select {
		case <-runCtx.Done():
			killProcessGroup(cmd)
			// Close the write end so the reader goroutine unblocks.
			pw.Close()
		case <-done:
		}
	}()
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if captured.Len() < maxBashBytes {
					room := maxBashBytes - captured.Len()
					if n > room {
						captured.Write(chunk[:room])
					} else {
						captured.Write(chunk)
					}
				}
				if progress != nil {
					progress(string(chunk))
				}
			}
			if err != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	pw.Close()
	<-done

	output := captured.String()
	truncBytes := captured.Len() >= maxBashBytes
	lines := strings.Split(output, "\n")
	truncLines := false
	if len(lines) > maxBashLines {
		lines = lines[:maxBashLines]
		truncLines = true
	}
	trimmed := strings.Join(lines, "\n")

	exitCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	elapsed := time.Since(start)

	// Terminal-log style: echo the command on the first line with
	// a shell-prompt prefix, a blank line, the captured output, and
	// a footer showing exit code + elapsed time. Matches the look
	// a human would see if they ran the command themselves, which
	// makes the model's reasoning about it more natural too.
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n", prompt, a.Command)
	if trimmed != "" {
		sb.WriteString("\n")
		sb.WriteString(trimmed)
		if !strings.HasSuffix(trimmed, "\n") {
			sb.WriteString("\n")
		}
	}
	if truncLines {
		fmt.Fprintf(&sb, "... [truncated at %d lines]\n", maxBashLines)
	}
	if truncBytes {
		fmt.Fprintf(&sb, "... [truncated at %d bytes]\n", maxBashBytes)
	}
	sb.WriteString("\n")
	if exitCode == 0 {
		fmt.Fprintf(&sb, "[exit 0]")
	} else {
		fmt.Fprintf(&sb, "[exit %d]", exitCode)
	}

	var fullPath string
	if truncBytes || truncLines {
		fullPath = writeFullOutput(output)
		if fullPath != "" {
			fmt.Fprintf(&sb, " (full output: %s)", fullPath)
		}
	}
	fmt.Fprintf(&sb, "  Took %s", humanDuration(elapsed))

	isErr := exitCode != 0 || runCtx.Err() != nil
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: sb.String()}},
		IsError: isErr,
		Details: map[string]any{
			"exit_code":        exitCode,
			"full_output_path": fullPath,
			"lines_truncated":  truncLines,
			"bytes_truncated":  truncBytes,
			"duration_ms":      elapsed.Milliseconds(),
		},
	}, nil
}

// humanDuration renders a duration in the "Took X.Ys" style used by
// the shell-log output: tenths of a second for sub-minute runs,
// whole seconds once we pass a minute. Trailing zeros dropped so
// "0.1s" instead of "0.10s".
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "0.0s"
	case d < time.Minute:
		s := d.Seconds()
		return fmt.Sprintf("%.1fs", s)
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

func writeFullOutput(s string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := filepath.Join(os.TempDir(), "zot-bash-"+hex.EncodeToString(b)+".log")
	if err := os.WriteFile(name, []byte(s), 0o600); err != nil {
		return ""
	}
	return name
}

type shellCommand struct {
	path   string
	flag   string
	isBash bool
}

func currentShell() shellCommand {
	return resolveShell(runtime.GOOS, isExecutableFile, exec.LookPath)
}

func resolveShell(goos string, executable func(string) bool, lookPath func(string) (string, error)) shellCommand {
	if goos == "windows" {
		return shellCommand{path: "cmd", flag: "/C"}
	}
	if executable("/bin/bash") {
		return shellCommand{path: "/bin/bash", flag: "-c", isBash: true}
	}
	if path, err := lookPath("bash"); err == nil {
		return shellCommand{path: path, flag: "-c", isBash: true}
	}
	return shellCommand{path: "/bin/sh", flag: "-c"}
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

func shellDescription(shell shellCommand) string {
	if shell.flag == "/C" {
		return "Run a Windows Command Prompt command via cmd /C. stdout+stderr merged."
	}
	if shell.isBash {
		return fmt.Sprintf("Run a Bash command via %s -c. stdout+stderr merged.", shell.path)
	}
	return "Bash is unavailable; run a POSIX sh command via /bin/sh -c. stdout+stderr merged."
}

func newShellCmd(ctx context.Context, command string) *exec.Cmd {
	shell := currentShell()
	return exec.CommandContext(ctx, shell.path, shell.flag, command)
}
