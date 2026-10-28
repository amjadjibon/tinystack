// Package docker runs ECS tasks' containers through the docker CLI.
//
// Each container of a task is its own container, labeled with its task's ARN,
// with its ports published on 127.0.0.1. The daemon pulls images as it runs
// them, so an image from TinyStack's registry needs a "docker login" first.
package docker

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
)

// removeTimeout bounds removing a container, which outlives its caller.
const removeTimeout = 30 * time.Second

var _ ecs.Runtime = (*Runtime)(nil)

// Runtime runs task containers in Docker.
type Runtime struct {
	// tasks and started are guarded by mu; each task's resources by its own lock,
	// so a slow image pull blocks only the containers of its task.
	tasks   map[string]*taskResources
	started map[string]bool
	mu      sync.Mutex
	closed  bool
}

func New() *Runtime {
	return &Runtime{started: map[string]bool{}, tasks: map[string]*taskResources{}}
}

func (r *Runtime) Start(ctx context.Context, spec *ecs.ContainerSpec) (ecs.Started, error) {
	args := []string{"run", "-d"}
	networkID := ""
	var task *taskResources
	if spec.TaskID != "" {
		r.mu.Lock()
		task = r.tasks[spec.TaskID]
		r.mu.Unlock()
		if task == nil {
			return ecs.Started{}, fmt.Errorf("task infrastructure was removed")
		}

		task.mu.Lock()
		defer task.mu.Unlock()
		if task.removed {
			return ecs.Started{}, fmt.Errorf("task infrastructure was removed")
		}

		networkID = task.anchor
		args = append(args, "--network=container:"+networkID)
		for _, mount := range spec.Mounts {
			source := task.volumes[mount.Source]
			option := source + ":" + mount.Target
			if mount.ReadOnly {
				option += ":ro"
			}

			args = append(args, "-v", option)
		}

		for _, from := range spec.VolumesFrom {
			source := task.containers[common.Value(from.SourceContainer)]
			if source == "" {
				return ecs.Started{}, fmt.Errorf("volume source container has not started")
			}

			if common.Value(from.ReadOnly) {
				source += ":ro"
			}

			args = append(args, "--volumes-from", source)
		}
	} else {
		args = append(args, "--add-host=host.docker.internal:host-gateway")
	}

	args = append(args, resourceArgs(spec.CPU, spec.Memory, spec.MemoryReservation)...)
	args = append(args, healthArgs(spec.HealthCheck)...)
	for _, k := range slices.Sorted(maps.Keys(spec.Labels)) {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}

	for _, k := range slices.Sorted(maps.Keys(spec.Environment)) {
		args = append(args, "-e", k+"="+spec.Environment[k])
	}

	for _, p := range spec.Ports {
		if networkID != "" {
			break
		}

		host := ""
		if p.Host != 0 {
			host = strconv.FormatInt(p.Host, 10)
		}

		args = append(args, "-p", "127.0.0.1:"+host+":"+strconv.FormatInt(p.Container, 10)+"/"+p.Protocol)
	}

	if spec.WorkingDirectory != "" {
		args = append(args, "-w", spec.WorkingDirectory)
	}

	if spec.User != "" {
		args = append(args, "-u", spec.User)
	}

	// --entrypoint takes one word; the rest of the entry point goes before
	// the command.
	command := spec.Command
	if len(spec.EntryPoint) > 0 {
		args = append(args, "--entrypoint", spec.EntryPoint[0])
		command = append(append([]string{}, spec.EntryPoint[1:]...), command...)
	}

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ecs.Started{}, fmt.Errorf("runtime is closed")
	}

	name := "tinystack-ecs-" + common.RandomID(24)
	r.mu.Lock()
	r.started[name] = true
	r.mu.Unlock()
	args = append(args, "--name", name)
	id, err := dockercli.Run(ctx, nil, append(append(args, "--", spec.Image), command...)...)
	if err != nil {
		r.Remove(ctx, name)
		return ecs.Started{}, err
	}

	// A container started while the runtime closed is removed at once.
	r.mu.Lock()
	closed = r.closed
	delete(r.started, name)
	r.started[id] = true
	r.mu.Unlock()
	if closed {
		r.Remove(ctx, id)
		return ecs.Started{}, fmt.Errorf("runtime is closed")
	}

	if task != nil {
		task.containers[spec.Name] = id
	}

	started := ecs.Started{ID: id}
	portID := id
	if networkID != "" {
		portID = networkID
	}

	for _, p := range spec.Ports {
		host, portErr := hostPort(ctx, portID, p)
		if portErr != nil {
			r.Remove(ctx, id)
			return ecs.Started{}, portErr
		}

		started.Ports = append(started.Ports, ecs.PortSpec{Container: p.Container, Host: host, Protocol: p.Protocol})
	}

	return started, nil
}

// hostPort returns the host port a container port is published on.
func hostPort(ctx context.Context, id string, p ecs.PortSpec) (int64, error) {
	out, err := dockercli.Run(ctx, nil, "port", id, strconv.FormatInt(p.Container, 10)+"/"+p.Protocol)
	if err != nil {
		return 0, err
	}

	// "docker port" may list an IPv4 and an IPv6 binding; use the first.
	first, _, _ := strings.Cut(out, "\n")
	n, parseErr := strconv.ParseInt(first[strings.LastIndex(first, ":")+1:], 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("docker port %s: unexpected output %q", id, out)
	}

	return n, nil
}

func (r *Runtime) State(ctx context.Context, id string) (exited bool, code int64, err error) {
	out, err := dockercli.Run(ctx, nil, "inspect", "--format", "{{.State.Running}} {{.State.ExitCode}}", id)
	if err != nil {
		if dockercli.Gone(err) {
			return false, 0, ecs.ErrGone
		}

		return false, 0, err
	}

	running, exitCode, _ := strings.Cut(out, " ")
	if running == "true" {
		return false, 0, nil
	}

	code, err = strconv.ParseInt(exitCode, 10, 64)
	return true, code, err
}

// Logs reads a container's standard output and error after since, in time
// order.
func (r *Runtime) Logs(ctx context.Context, id string, since time.Time) ([]ecs.LogLine, error) {
	args := []string{"logs", "--timestamps"}
	if !since.IsZero() {
		args = append(args, "--since", since.Format(time.RFC3339Nano))
	}

	// #nosec G204 -- the container ID comes from docker run.
	cmd := exec.CommandContext(ctx, "docker", append(args, id)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker logs: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var lines []ecs.LogLine
	for _, out := range []string{stdout.String(), stderr.String()} {
		for raw := range strings.SplitSeq(out, "\n") {
			stamp, text, _ := strings.Cut(raw, " ")
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if err == nil && at.After(since) {
				lines = append(lines, ecs.LogLine{Time: at, Message: strings.TrimRight(text, "\r")})
			}
		}
	}

	slices.SortStableFunc(lines, func(a, b ecs.LogLine) int { return a.Time.Compare(b.Time) })
	return lines, nil
}

// Remove deletes a container. It outlives the caller's cancellation, since a
// container must go even when its task was abandoned.
func (r *Runtime) Remove(ctx context.Context, id string) {
	r.mu.Lock()
	owned := r.started[id]
	r.mu.Unlock()
	if !owned {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	_, _ = dockercli.Run(ctx, nil, "rm", "-f", "-v", id)
	r.mu.Lock()
	delete(r.started, id)
	r.mu.Unlock()
}

// Close removes every container the runtime started, refuses new ones, and
// waits for the removals.
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.Reset()
}

// Reset removes every container the runtime started and waits for the
// removals. Unlike Close, the runtime keeps serving.
func (r *Runtime) Reset() {
	r.mu.Lock()
	ids := slices.Collect(maps.Keys(r.started))
	owners := slices.Collect(maps.Keys(r.tasks))
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() { r.Remove(context.Background(), id) })
	}

	wg.Wait()
	for _, owner := range owners {
		r.RemoveTask(context.Background(), owner)
	}
}

// CleanOwned removes orphan containers with both labels matching one caller's store.
// It does not affect resources started by another service or store.
func (r *Runtime) CleanOwned(ctx context.Context, label, ownerLabel, owner string) error {
	containers, _, err := dockercli.ListOwned(ctx, label, ownerLabel, owner)
	if err != nil {
		return err
	}

	for id := range containers {
		if err := dockercli.RemoveContainerVolume(ctx, id, ""); err != nil {
			return err
		}
	}

	return nil
}

// StopContainer gives an owned container the same 30-second grace period Batch uses on AWS.
// Remove still performs final cleanup after logs have been read.
func (r *Runtime) StopContainer(ctx context.Context, id string) error {
	r.mu.Lock()
	owned := r.started[id]
	r.mu.Unlock()
	if !owned {
		return nil
	}

	_, err := dockercli.Run(ctx, nil, "stop", "--time", "30", id)
	return err
}
