package modes

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

func TestShellEscapeTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	agent := core.NewAgent(nil, "", "", nil)
	i := NewInteractive(InteractiveConfig{
		Agent:               agent,
		CWD:                 t.TempDir(),
		Shell:               []string{"/bin/sh", "-c"},
		ShellTimeoutSeconds: 1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	i.startShellEscape(ctx, "printf ready\nexec sleep 60")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		running := i.shellRunning
		i.mu.Unlock()
		messages := agent.Messages()
		if !running && len(messages) == 1 {
			text := userMessageText(messages[0])
			if !strings.Contains(text, "ready") || !strings.Contains(text, "Command timed out.") {
				t.Fatalf("shell context = %q", text)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shell timeout did not finish")
}
