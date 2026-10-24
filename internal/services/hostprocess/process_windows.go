//go:build windows

// Package hostprocess starts and kills host processes that local runtimes run.
package hostprocess

import (
	"fmt"
	"os/exec"
)

// Configure needs no setup on Windows, where Kill ends the process tree.
func Configure(_ *exec.Cmd) {}

// Kill kills the process tree cmd started.
func Kill(cmd *exec.Cmd) {
	// #nosec G204 -- taskkill is given only the PID of the environment started by this runtime.
	_ = exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
}
