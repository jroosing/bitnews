//go:build unix

package tool

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the tool in its own process group and makes
// cancellation kill the whole group, so helper processes do not outlive it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
