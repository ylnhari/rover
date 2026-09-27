package launcher

import (
	"fmt"
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// SetProcessGroup arranges for descendants of cmd to be tracked with the
// process so cancellation can stop the full command tree.
func SetProcessGroup(cmd *exec.Cmd) { setProcessGroup(cmd) }

// KillChildProcesses stops cmd and any descendants it started.
func KillChildProcesses(cmd *exec.Cmd) error { return killChildProcesses(cmd) }

func killChildProcesses(cmd *exec.Cmd) error {
	kill := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", cmd.Process.Pid))
	err := kill.Run()
	if err != nil {
		if exe, ok := err.(*exec.ExitError); ok {
			if exe.ExitCode() == 128 || exe.ExitCode() == 1 {
				return nil
			}
		}
		return err
	}
	return nil
}
