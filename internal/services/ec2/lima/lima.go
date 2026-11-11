// Package lima implements the EC2 VM runtime through the configurable limactl CLI.
package lima

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/amjadjibon/tinystack/internal/services/ec2"
)

var _ ec2.Runtime = (*Runtime)(nil)

type Runtime struct{ command string }

func New(command string) *Runtime { return &Runtime{command: command} }

// run is the only command boundary; no shell, Lima Go dependency or host configuration.
func (r *Runtime) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	// #nosec G204 -- explicitly configured executable, with arguments passed without a shell.
	cmd := exec.CommandContext(ctx, r.command, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("limactl %s: %w: %s", args[0], err, stderr.String())
	}

	return output, nil
}

type machine struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	SSHLocalPort int    `json:"sshLocalPort"`
}

func (r *Runtime) machines(ctx context.Context) ([]machine, error) {
	output, err := r.run(ctx, nil, "list", "--json")
	if err != nil {
		return nil, err
	}

	result := []machine{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var m machine
		if err := decoder.Decode(&m); err != nil {
			if err == io.EOF {
				return result, nil
			}

			return nil, fmt.Errorf("decode limactl list: %w", err)
		}

		result = append(result, m)
	}
}

func (r *Runtime) Resources(ctx context.Context, owner string) ([]string, error) {
	machines, err := r.machines(ctx)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, m := range machines {
		if strings.HasPrefix(m.Name, owner) {
			names = append(names, m.Name)
		}
	}

	return names, nil
}

func (r *Runtime) Ensure(ctx context.Context, spec *ec2.VMSpec) error {
	machines, err := r.machines(ctx)
	if err != nil {
		return err
	}

	exists := false
	for _, m := range machines {
		if m.Name == spec.Name {
			exists = true
		}
	}

	if !exists {
		data, err := ec2.LimaConfig(spec)
		if err != nil {
			return err
		}

		if _, err := r.run(ctx, data, "create", "--tty=false", "--name="+spec.Name, "-"); err != nil {
			return err
		}
	}

	return r.Action(ctx, spec.Name, "start")
}

func (r *Runtime) Action(ctx context.Context, name, action string) error {
	args := []string{action, "--tty=false", name}
	switch action {
	case "delete":
		args = []string{"delete", "--force", name}
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("unsupported Lima action %q", action)
	}

	_, err := r.run(ctx, nil, args...)
	return err
}

func (r *Runtime) Ready(ctx context.Context, name string) (port int, ready bool, err error) {
	machines, err := r.machines(ctx)
	if err != nil {
		return 0, false, err
	}

	for _, m := range machines {
		if m.Name != name {
			continue
		}

		if m.Status != "Running" {
			return 0, false, nil
		}

		// limactl shell proves authenticated guest SSH works, rather than just an open TCP socket.
		_, probeErr := r.run(ctx, nil, "shell", name, "true")
		return m.SSHLocalPort, probeErr == nil, nil
	}

	return 0, false, fmt.Errorf("lima instance %s disappeared", name)
}
