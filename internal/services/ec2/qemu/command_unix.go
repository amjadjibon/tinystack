//go:build !windows

package qemu

import "os/exec"

func detach(cmd *exec.Cmd) {}
