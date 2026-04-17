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
	"syscall"
	"time"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
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

const bashSchema = `{
  "type":"object",
  "properties":{
    "command":{"type":"string","description":"Shell command to execute."},
    "timeout":{"type":"integer","description":"Timeout in seconds. No default timeout."}
  },
  "required":["command"],
  "additionalProperties":false
}`

func (t *BashTool) Name() string            { return "bash" }
func (t *BashTool) Description() string     { return "Run a shell command. stdout and stderr are merged." }
func (t *BashTool) Schema() json.RawMessage { return json.RawMessage(bashSchema) }

func (t *BashTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a bashArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return core.ToolResult{}, fmt.Errorf("command is required")
	}
	if err := t.Sandbox.CheckCommand(a.Command); err != nil {
		return core.ToolResult{}, err
	}
	cwd := t.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if a.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
		defer cancel()
	}

	cmd := newShellCmd(runCtx, a.Command)
	cmd.Dir = cwd
	cmd.Env = os.Environ()

	// Capture merged stdout+stderr with line-by-line streaming.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return core.ToolResult{}, fmt.Errorf("start: %w", err)
	}

	// Writer to both the buffer (trimmed) and progress callback.
	captured := &bytes.Buffer{}
	done := make(chan struct{})
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

	// On hard cancel, try to kill the process group.
	if ctx.Err() != nil && cmd.Process != nil {
		killProcessGroup(cmd)
	}

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

	var sb strings.Builder
	if trimmed != "" {
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
	sb.WriteString(fmt.Sprintf("[exit %d]", exitCode))

	var fullPath string
	if truncBytes || truncLines {
		fullPath = writeFullOutput(output)
		if fullPath != "" {
			fmt.Fprintf(&sb, " (full output: %s)", fullPath)
		}
	}

	isErr := exitCode != 0 || ctx.Err() != nil
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: sb.String()}},
		IsError: isErr,
		Details: map[string]any{
			"exit_code":        exitCode,
			"full_output_path": fullPath,
			"lines_truncated":  truncLines,
			"bytes_truncated":  truncBytes,
		},
	}, nil
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

func newShellCmd(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

// killProcessGroup best-effort SIGTERM then SIGKILL.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	time.AfterFunc(3*time.Second, func() {
		_ = cmd.Process.Kill()
	})
}
