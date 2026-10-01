// Package dockercli runs docker CLI commands for the runtimes that run Lambda
// functions, ECS tasks, and EKS clusters in containers.
package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Run runs one docker CLI command with stdin, if any, and returns its trimmed
// standard output.
func Run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	// #nosec G204 -- callers build arguments from validated configuration.
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

// Gone reports whether err is Run's error for a container that does not exist.
func Gone(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such")
}
