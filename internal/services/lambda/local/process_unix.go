//go:build !windows

package local

import (
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func killProcess(cmd *exec.Cmd)      { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
