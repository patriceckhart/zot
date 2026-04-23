//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup puts the command in its own process group so
// killProcessGroup can target the entire tree including background
// children spawned with &.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGTERM then SIGKILL to the entire process
// group so backgrounded children (cmd &) are also cleaned up.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(3*time.Second, func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
}
