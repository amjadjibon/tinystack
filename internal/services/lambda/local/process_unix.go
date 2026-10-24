//go:build !windows

package local

import (
	"os/exec"

	"github.com/amjadjibon/tinystack/internal/services/hostprocess"
)

func configureProcess(cmd *exec.Cmd) { hostprocess.Configure(cmd) }
func killProcess(cmd *exec.Cmd)      { hostprocess.Kill(cmd) }
