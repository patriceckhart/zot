//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// detachChild spawns the bot in its own process group via the
// DETACHED_PROCESS + CREATE_NEW_PROCESS_GROUP creation flags. Windows
// has no direct setsid equivalent; these two flags combined give the
// child a clean console-less group that ctrl+c on the parent won't
// reach.
func init() {
	detachChild = func(cmd *exec.Cmd) {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
		}
	}
}
