package modes

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

func TestShellEscapeAddsOutputToAgentContext(t *testing.T) {
	agent := core.NewAgent(nil, "", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: agent, CWD: t.TempDir()})
	cmd := "printf zot-shell-context"
	if runtime.GOOS == "windows" {
		cmd = "echo zot-shell-context"
	}

	i.startShellEscape(context.Background(), cmd)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages := agent.Messages()
		i.mu.Lock()
		running := i.shellRunning
		i.mu.Unlock()
		if len(messages) == 1 && !running {
			text := userMessageText(messages[0])
			if !strings.Contains(text, "zot-shell-context") {
				t.Fatalf("shell context = %q, want command output", text)
			}
			if messages[0].Meta[shellEscapeMetaKey] != "true" {
				t.Fatalf("shell context metadata = %v", messages[0].Meta)
			}
			history := i.inputHistory()
			if len(history) != 1 || history[0] != "!"+cmd {
				t.Fatalf("input history = %q, want shell command with bang", history)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shell output was not appended to agent context")
}

func TestShellEscapeConfiguredShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	agent := core.NewAgent(nil, "", "", nil)
	i := NewInteractive(InteractiveConfig{
		Agent: agent,
		CWD:   t.TempDir(),
		Shell: []string{"/bin/sh", "-c", "printf configured-shell-ok"},
	})
	i.startShellEscape(context.Background(), "ignored-command")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		running := i.shellRunning
		i.mu.Unlock()
		if messages := agent.Messages(); len(messages) == 1 && !running {
			if text := userMessageText(messages[0]); !strings.Contains(text, "configured-shell-ok") {
				t.Fatalf("shell context = %q", text)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("configured shell did not finish")
}

func TestShellEscapeCommand(t *testing.T) {
	cases := []struct {
		in      string
		wantCmd string
		wantOK  bool
	}{
		{"!ls -la", "ls -la", true},
		{"  !pwd", "pwd", true},
		{"!  go test ./...  ", "go test ./...", true},
		{"!", "", false},
		{"!   ", "", false},
		{"ls -la", "", false},
		{"/help", "", false},
		{"hello !world", "", false},
	}
	for _, c := range cases {
		cmd, ok := shellEscapeCommand(c.in)
		if ok != c.wantOK || cmd != c.wantCmd {
			t.Errorf("shellEscapeCommand(%q) = (%q,%v); want (%q,%v)",
				c.in, cmd, ok, c.wantCmd, c.wantOK)
		}
	}
}
