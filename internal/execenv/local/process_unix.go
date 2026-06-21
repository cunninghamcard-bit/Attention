//go:build unix

package local

import (
	"os/exec"
	"syscall"
)

func configureProcessTreeCancel(cmd *exec.Cmd) {
	// pi detaches the shell and kills the tree on abort/timeout:
	// .agents/references/pi/packages/coding-agent/src/core/tools/bash.ts:78-117.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
