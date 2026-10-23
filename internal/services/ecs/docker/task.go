package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
)

const unknownHealth = "UNKNOWN"

type taskResources struct {
	volumes      map[string]string
	containers   map[string]string
	network      string
	anchor       string
	ownedVolumes []string
	mu           sync.Mutex
	removed      bool
}

func resourceArgs(cpu, memory, reservation int64) []string {
	var args []string
	if cpu > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(cpu)/1024, 'f', -1, 64))
	}

	if memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(memory, 10)+"m")
	}

	if reservation > 0 {
		args = append(args, "--memory-reservation", strconv.FormatInt(reservation, 10)+"m")
	}

	return args
}

func healthArgs(h *api.HealthCheck) []string {
	if h == nil {
		return []string{"--no-healthcheck"}
	}

	command := strings.Join(h.Command[1:], " ")
	if h.Command[0] == "CMD" {
		words := make([]string, 0, len(h.Command)-1)
		for _, word := range h.Command[1:] {
			words = append(words, "'"+strings.ReplaceAll(word, "'", "'\\''")+"'")
		}

		command = "exec " + strings.Join(words, " ")
	}

	return []string{
		"--health-cmd", command,
		"--health-interval", strconv.FormatInt(common.Value(common.OrElse(h.Interval, new(int64(30)))), 10) + "s",
		"--health-timeout", strconv.FormatInt(common.Value(common.OrElse(h.Timeout, new(int64(5)))), 10) + "s",
		"--health-retries", strconv.FormatInt(common.Value(common.OrElse(h.Retries, new(int64(3)))), 10),
		"--health-start-period", strconv.FormatInt(common.Value(h.StartPeriod), 10) + "s",
	}
}

func (r *Runtime) Health(ctx context.Context, id string) (string, error) {
	out, err := dockercli.Run(
		ctx,
		nil,
		"inspect",
		"--format",
		"{{if .State.Health}}{{.State.Health.Status}}{{end}}",
		id,
	)
	if err != nil {
		if dockercli.Gone(err) {
			return unknownHealth, ecs.ErrGone
		}

		return unknownHealth, err
	}

	switch out {
	case "healthy":
		return "HEALTHY", nil
	case "unhealthy":
		return "UNHEALTHY", nil
	default:
		return unknownHealth, nil
	}
}

// PrepareTask creates a namespace holder and only the volumes this task needs.
// Shared volumes are deliberately never removed, including autoprovisioned ones.
func (r *Runtime) PrepareTask(
	ctx context.Context, owner string, specs []ecs.ContainerSpec, volumes []api.Volume, cpu, memory string,
) (err error) {
	task := &taskResources{volumes: map[string]string{}, containers: map[string]string{}}
	task.mu.Lock()
	defer task.mu.Unlock()
	r.mu.Lock()
	closed, exists := r.closed, r.tasks[owner] != nil
	if !closed && !exists {
		r.tasks[owner] = task
	}

	r.mu.Unlock()
	if closed {
		return fmt.Errorf("runtime is closed")
	}

	if exists {
		return fmt.Errorf("task infrastructure already exists")
	}

	defer func() {
		if err != nil {
			r.mu.Lock()
			if r.tasks[owner] == task {
				delete(r.tasks, owner)
			}

			r.mu.Unlock()
			r.cleanup(ctx, task)
		}
	}()
	for _, volume := range volumes {
		name := common.Value(volume.Name)
		if volume.Host != nil && common.Value(volume.Host.SourcePath) != "" {
			source := common.Value(volume.Host.SourcePath)
			if !filepath.IsAbs(source) {
				return fmt.Errorf("host sourcePath must be absolute")
			}

			task.volumes[name] = source
			continue
		}

		shared, auto := false, false
		if config := volume.DockerVolumeConfiguration; config != nil {
			shared = common.Value(config.Scope) == "shared"
			auto = common.Value(config.Autoprovision)
		}

		dockerName := "tinystack-ecs-" + common.RandomID(24)
		if shared {
			dockerName = name
			if driver, inspectErr := dockercli.Run(
				ctx,
				nil,
				"volume",
				"inspect",
				"--format",
				"{{.Driver}}",
				dockerName,
			); inspectErr == nil {
				if driver != "local" {
					return fmt.Errorf("shared volume %s must use the local driver", name)
				}

				task.volumes[name] = dockerName
				continue
			}

			if !auto {
				return fmt.Errorf("shared volume %s does not exist", name)
			}
		}

		if !shared {
			task.ownedVolumes = append(task.ownedVolumes, dockerName)
		}

		if _, err = dockercli.Run(ctx, nil, "volume", "create", "--driver", "local", dockerName); err != nil {
			return err
		}

		task.volumes[name] = dockerName
	}

	task.network = "tinystack-ecs-" + common.RandomID(24)
	if _, err = dockercli.Run(ctx, nil, "network", "create", "--label", "tinystack.ecs.task="+owner,
		task.network); err != nil {
		return err
	}

	args := []string{
		"run", "-d", "--network", task.network, "--label", "tinystack.ecs.task=" + owner,
		"--add-host=host.docker.internal:host-gateway",
	}
	// Divide task budgets into per-container ceilings: Docker CLI has no portable
	// task cgroup. The sum of these ceilings never exceeds the task budget.
	taskCPU, _ := strconv.ParseInt(cpu, 10, 64)
	taskMemory, _ := strconv.ParseInt(memory, 10, 64)
	if limitErr := allocateLimits(specs, taskCPU, true); limitErr != nil {
		return limitErr
	}

	if limitErr := allocateLimits(specs, taskMemory, false); limitErr != nil {
		return limitErr
	}

	for i := range specs {
		for _, port := range specs[i].Ports {
			host := ""
			if port.Host != 0 {
				host = strconv.FormatInt(port.Host, 10)
			}

			args = append(args, "-p", "127.0.0.1:"+host+":"+strconv.FormatInt(port.Container, 10)+"/"+port.Protocol)
		}
	}

	task.anchor = "tinystack-ecs-" + common.RandomID(24)
	args = append(args, "--name", task.anchor)
	_, err = dockercli.Run(ctx, nil, append(args, "busybox", "sleep", "2147483647")...)
	return err
}

// RemoveTask waits for a container of the task that is starting, then removes
// the task's containers, network anchor, network and task-scoped volumes.
func (r *Runtime) RemoveTask(ctx context.Context, owner string) {
	r.mu.Lock()
	task := r.tasks[owner]
	delete(r.tasks, owner)
	r.mu.Unlock()
	if task == nil {
		return
	}

	task.mu.Lock()
	defer task.mu.Unlock()
	r.cleanup(ctx, task)
}

// cleanup runs with task.mu held, after the task left r.tasks.
func (r *Runtime) cleanup(ctx context.Context, task *taskResources) {
	task.removed = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	for _, id := range task.containers {
		r.Remove(ctx, id)
	}

	if task.anchor != "" {
		_, _ = dockercli.Run(ctx, nil, "rm", "-f", task.anchor)
	}

	if task.network != "" {
		_, _ = dockercli.Run(ctx, nil, "network", "rm", task.network)
	}

	for _, name := range task.ownedVolumes {
		_, _ = dockercli.Run(ctx, nil, "volume", "rm", name)
	}
}

func allocateLimits(specs []ecs.ContainerSpec, budget int64, cpu bool) error {
	if budget == 0 {
		return nil
	}

	used, unspecified := int64(0), int64(0)
	for i := range specs {
		spec := &specs[i]
		value := spec.Memory
		if cpu {
			value = spec.CPU
		}

		used += value
		if value == 0 {
			unspecified++
		}
	}

	if used > budget || unspecified > 0 && budget-used < unspecified {
		return fmt.Errorf("container resource limits exceed the task budget")
	}

	for i := range specs {
		value := &specs[i].Memory
		if cpu {
			value = &specs[i].CPU
		}

		if *value == 0 {
			*value = (budget - used) / unspecified
			used += *value
			unspecified--
		}

		if !cpu && specs[i].MemoryReservation > specs[i].Memory {
			return fmt.Errorf("memoryReservation exceeds allocated task memory")
		}
	}

	return nil
}
