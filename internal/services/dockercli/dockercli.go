// Package dockercli runs docker CLI commands for the runtimes that run Lambda
// functions, ECS tasks, and EKS clusters in containers.
package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
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

// HostPort returns the loopback host port Docker allocated for a published TCP port.
func HostPort(ctx context.Context, id string, containerPort int64) (int64, error) {
	out, err := Run(ctx, nil, "port", id, strconv.FormatInt(containerPort, 10)+"/tcp")
	if err != nil {
		return 0, err
	}

	first, _, _ := strings.Cut(out, "\n")
	n, parseErr := strconv.ParseInt(first[strings.LastIndex(first, ":")+1:], 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("docker port %s: unexpected output %q", id, out)
	}

	return n, nil
}

// ListLabeled returns container IDs and the values of their ownership label.
func ListLabeled(ctx context.Context, label string) (map[string]string, error) {
	return listLabeled(ctx, label, "ps", "-a", "--format", "{{.ID}} {{.Label \""+label+"\"}}")
}

// ListLabeledVolumes returns volume names and the values of their ownership label.
func ListLabeledVolumes(ctx context.Context, label string) (map[string]string, error) {
	return listLabeled(ctx, label, "volume", "ls", "--format", "{{.Name}} {{.Label \""+label+"\"}}")
}

func listLabeled(ctx context.Context, label string, command ...string) (map[string]string, error) {
	out, err := Run(ctx, nil, append(command, "--filter", "label="+label)...)
	if err != nil {
		return nil, err
	}

	res := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			res[parts[0]] = parts[1]
		}
	}

	return res, nil
}
