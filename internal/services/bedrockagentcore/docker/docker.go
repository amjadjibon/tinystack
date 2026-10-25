// Package docker runs AgentCore runtime sessions in containers through the docker CLI.
//
// Each session gets its own container, started from the image of the runtime version and published on a loopback
// port. AWS requires linux/arm64 images, so containers run with --platform linux/arm64; on an x86_64 host Docker
// emulates it, which is slow.
package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrockagentcore"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

const (
	ownerLabel   = "tinystack.agentcore.owner"
	sessionLabel = "tinystack.agentcore"
	// readyTimeout is how long a started container has to answer before the session is failed.
	readyTimeout = 2 * time.Minute
	// logLines is how many lines of a failed container's log the error carries.
	logLines = "20"
)

var _ bedrockagentcore.Runtime = (*Runtime)(nil)

// Runtime starts session containers with the docker CLI.
type Runtime struct{ client *http.Client }

func New() *Runtime { return &Runtime{client: &http.Client{Timeout: 5 * time.Second}} }

// Start runs the container and waits until it is ready.
func (r *Runtime) Start(ctx context.Context, spec *bedrockagentcore.Spec) (bedrockagentcore.Started, error) {
	name := "tinystack-agentcore-" + common.RandomID(24)
	args := []string{
		"run", "-d", "--name", name, "--platform", "linux/arm64",
		"--label", ownerLabel + "=" + spec.Owner, "--label", sessionLabel + "=" + spec.Session,
		"-p", "127.0.0.1::" + strconv.Itoa(spec.Port), "--add-host=host.docker.internal:host-gateway",
	}

	keys := make([]string, 0, len(spec.Environment))
	for k := range spec.Environment {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+spec.Environment[k])
	}

	id, err := dockercli.Run(ctx, nil, append(args, "--", spec.Image)...)
	if err != nil {
		// A failed run may still have created the container.
		_ = r.Remove(context.WithoutCancel(ctx), name)

		return bedrockagentcore.Started{}, err
	}

	port, err := dockercli.HostPort(ctx, id, int64(spec.Port))
	if err != nil {
		_ = r.Remove(context.WithoutCancel(ctx), id)

		return bedrockagentcore.Started{}, err
	}

	addr := "127.0.0.1:" + strconv.FormatInt(port, 10)
	if err = r.wait(ctx, addr, spec); err != nil {
		logs, _ := dockercli.Run(context.WithoutCancel(ctx), nil, "logs", "--tail", logLines, id)
		_ = r.Remove(context.WithoutCancel(ctx), id)

		return bedrockagentcore.Started{}, fmt.Errorf("%w; the last lines of its log:\n%s", err, logs)
	}

	return bedrockagentcore.Started{ID: id, Addr: addr}, nil
}

// wait polls the probe path until the container answers it. Docker's port forwarder accepts connections before
// the container listens, so only an HTTP answer counts, and for /ping only a 200.
func (r *Runtime) wait(ctx context.Context, addr string, spec *bedrockagentcore.Spec) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+spec.Probe, http.NoBody)
		if err != nil {
			return err
		}

		// #nosec G704 -- the address is a loopback port Docker published for the container.
		resp, err := r.client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if !spec.StrictProbe || resp.StatusCode == http.StatusOK {
				return nil
			}

			err = fmt.Errorf("GET %s answered %d", spec.Probe, resp.StatusCode)
		}

		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("the runtime container was not ready on port %d: %w", spec.Port, last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Remove removes the container and its anonymous volumes; one that is gone counts as removed.
func (r *Runtime) Remove(ctx context.Context, id string) error {
	if _, err := dockercli.Run(ctx, nil, "rm", "-f", "-v", id); err != nil && !dockercli.Gone(err) {
		return err
	}

	return nil
}

// Clean removes the containers that earlier runs of the owner left behind.
func (r *Runtime) Clean(ctx context.Context, owner string) error {
	containers, err := dockercli.ListLabeled(ctx, ownerLabel)
	if err != nil {
		return err
	}

	for id, value := range containers {
		if strings.TrimSpace(value) != owner {
			continue
		}

		err = r.Remove(ctx, id)
		if err != nil {
			return err
		}
	}

	return nil
}
