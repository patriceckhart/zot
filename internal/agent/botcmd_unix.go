//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// detachChild runs the bot in a new session so tty signals sent to
// the parent shell (SIGINT when the terminal closes, SIGHUP on
// logout) don't propagate to the detached bot.
func init() {
	detachChild = func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}
