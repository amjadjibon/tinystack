//go:build windows

package local

import (
	"fmt"
	"os/exec"
)

func configureProcess(_ *exec.Cmd) {}
func killProcess(cmd *exec.Cmd) {
	// #nosec G204 -- taskkill is given only the PID of the environment started by this runtime.
	_ = exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
}
