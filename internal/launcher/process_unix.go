//go:build !windows

package launcher

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// SetProcessGroup arranges for descendants of cmd to be tracked with the
// process so cancellation can stop the full command tree.
func SetProcessGroup(cmd *exec.Cmd) { setProcessGroup(cmd) }

// KillChildProcesses stops cmd and any descendants it started.
func KillChildProcesses(cmd *exec.Cmd) error { return killChildProcesses(cmd) }

func killChildProcesses(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
