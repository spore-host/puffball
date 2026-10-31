//go:build !windows

package local

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup runs cmd in its own process group and, on cancel, kills the
// whole group, so the Python uv started doesn't outlive it.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
