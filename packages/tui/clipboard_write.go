package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ClipboardCopySequence asks the terminal to copy text via OSC 52. Terminals
// may block this request, and the protocol provides no success acknowledgement.
func ClipboardCopySequence(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

// WriteClipboardText copies text using local clipboard clients. Caller can
// fall back to ClipboardCopySequence when no local clipboard is available.
func WriteClipboardText(ctx context.Context, text string) error {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return fmt.Errorf("local clipboard unavailable over SSH")
	}
	var commands []clipboardTextCommand
	switch runtime.GOOS {
	case "darwin":
		commands = []clipboardTextCommand{{name: "/usr/bin/pbcopy"}}
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			commands = append(commands, clipboardTextCommand{name: "wl-copy"})
		}
		if os.Getenv("DISPLAY") != "" {
			commands = append(commands,
				clipboardTextCommand{name: "xclip", args: []string{"-selection", "clipboard"}},
				clipboardTextCommand{name: "xsel", args: []string{"--clipboard", "--input"}})
		}
		if len(commands) == 0 {
			commands = []clipboardTextCommand{
				{name: "wl-copy"},
				{name: "xclip", args: []string{"-selection", "clipboard"}},
				{name: "xsel", args: []string{"--clipboard", "--input"}},
			}
		}
	case "windows":
		// Selected text travels on stdin, never in arguments or command source.
		script := "[Console]::InputEncoding = [System.Text.Encoding]::UTF8\nSet-Clipboard -Value ([Console]::In.ReadToEnd())"
		commands = []clipboardTextCommand{
			{name: "powershell.exe", args: []string{"-NoProfile", "-NonInteractive", "-Command", script}},
			{name: "pwsh.exe", args: []string{"-NoProfile", "-NonInteractive", "-Command", script}},
		}
	}
	return writeClipboardTextCommands(ctx, text, commands...)
}

func writeClipboardTextCommands(ctx context.Context, text string, commands ...clipboardTextCommand) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	found := false
	for _, candidate := range commands {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := exec.LookPath(candidate.name)
		if err != nil {
			continue
		}
		found = true
		cmd := exec.CommandContext(ctx, path, candidate.args...)
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !found {
		return errClipboardCommandUnavailable
	}
	// Command output can contain clipboard text. Do not include it in errors.
	return fmt.Errorf("clipboard copy command failed")
}
