// Package dockercli runs docker CLI commands for the runtimes that run Lambda
// functions, ECS tasks, and EKS clusters in containers.
package dockercli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
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

// ContainerEndpoint rewrites the endpoint a client reached TinyStack at so a container can reach it too: a loopback
// host becomes host.docker.internal, which the containers resolve with --add-host=host.docker.internal:host-gateway.
func ContainerEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}

	if u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1" {
		u.Host = "host.docker.internal:" + u.Port()
	}

	return u.String()
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

// Stream runs a CLI command with merged output, delivering each complete line as it arrives.
// A callback failure cancels the command; the owning runtime still removes its container.
func Stream(ctx context.Context, stdin io.Reader, emit func(string) error, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// #nosec G204 -- runtimes construct Docker arguments without a host shell.
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return err
	}

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = writer.Close()
		done <- err
	}()

	// ReadString avoids Scanner's arbitrary line limit for user build output.
	buffered := bufio.NewReader(reader)
	var callbackErr error
	for {
		line, err := buffered.ReadString('\n')
		if line != "" && callbackErr == nil {
			callbackErr = emit(strings.TrimRight(line, "\r\n"))
			if callbackErr != nil {
				cancel()
			}
		}

		if err != nil {
			break
		}
	}

	_ = reader.Close()
	err := <-done
	if callbackErr != nil {
		return callbackErr
	}

	return err
}

// RemoveContainerVolume removes an owned container and optionally its named data volume.
// Callers establish ownership before passing names; absent resources make cleanup idempotent.
func RemoveContainerVolume(ctx context.Context, name, volume string) error {
	if name != "" {
		if _, err := Run(ctx, nil, "rm", "-f", name); err != nil && !Gone(err) {
			return err
		}
	}

	if volume != "" {
		if _, err := Run(ctx, nil, "volume", "rm", volume); err != nil && !Gone(err) {
			return err
		}
	}

	return nil
}
