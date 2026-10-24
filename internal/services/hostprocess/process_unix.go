//go:build !windows

// Package hostprocess starts and kills host processes that local runtimes run.
package hostprocess

import (
	"os/exec"
	"syscall"
)

// Configure starts cmd in its own process group so Kill reaches its children.
func Configure(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// Kill kills the process group cmd started.
func Kill(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
