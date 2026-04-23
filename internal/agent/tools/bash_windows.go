//go:build windows

package tools

import (
	"os/exec"
	"time"
)

func setProcessGroup(_ *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	time.AfterFunc(3*time.Second, func() {
		_ = cmd.Process.Kill()
	})
}
