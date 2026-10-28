// Package valkeydocker shares Redis and Valkey container execution across services.
//
// Each node of a cache cluster is its own container, labeled with its resource's
// ARN, with its port published on 127.0.0.1.
package valkeydocker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

const removeTimeout = 30 * time.Second

// ErrGone reports a removed container.
var ErrGone = errors.New("container is gone")

// NodeSpec describes a cache container.
type NodeSpec struct {
	Labels    map[string]string
	Image     string
	Alias     string
	Network   string
	Name      string
	Volume    string
	ConfigDir string
	Args      []string
	Port      int64
	HostPort  int64
}

// Started describes a published cache container.
type Started struct {
	ID   string
	Port int64
}

type Runtime struct {
	started map[string]bool
	mu      sync.Mutex
	closed  bool
}

func New() *Runtime {
	return &Runtime{started: map[string]bool{}}
}

func (r *Runtime) Start(ctx context.Context, spec *NodeSpec) (Started, error) {
	args := []string{"run", "-d"}
	if spec.ConfigDir != "" {
		args = []string{"create"}
	}

	for _, k := range slices.Sorted(maps.Keys(spec.Labels)) {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}

	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}

	host := ""
	if spec.HostPort != 0 {
		host = fmt.Sprint(spec.HostPort)
	}

	args = append(args, "-p", fmt.Sprintf("127.0.0.1:%s:%d/tcp", host, spec.Port))
	if spec.Volume != "" {
		volumeArgs := []string{"volume", "create"}
		for _, k := range slices.Sorted(maps.Keys(spec.Labels)) {
			volumeArgs = append(volumeArgs, "--label", k+"="+spec.Labels[k])
		}

		if _, err := dockercli.Run(ctx, nil, append(volumeArgs, spec.Volume)...); err != nil {
			return Started{}, err
		}

		args = append(args, "-v", spec.Volume+":/data")
	}

	if spec.Network != "" {
		_, _ = dockercli.Run(ctx, nil, "network", "create", spec.Network)
		args = append(args, "--network", spec.Network)
	}

	if spec.Alias != "" {
		args = append(args, "--network-alias", spec.Alias)
	}

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return Started{}, fmt.Errorf("runtime is closed")
	}

	cmdArgs := append(append(args, "--", spec.Image), spec.Args...)
	id, err := dockercli.Run(ctx, nil, cmdArgs...)
	if err != nil {
		return Started{}, err
	}

	r.mu.Lock()
	closed = r.closed
	r.started[id] = true
	r.mu.Unlock()
	if closed {
		r.Remove(ctx, id)
		return Started{}, fmt.Errorf("runtime is closed")
	}

	if spec.ConfigDir != "" {
		if _, copyErr := dockercli.Run(ctx, nil, "cp", spec.ConfigDir, id+":/tinystack"); copyErr != nil {
			r.Remove(ctx, id)
			return Started{}, copyErr
		}

		if _, startErr := dockercli.Run(ctx, nil, "start", id); startErr != nil {
			r.Remove(ctx, id)
			return Started{}, startErr
		}
	}

	port, portErr := dockercli.HostPort(ctx, id, spec.Port)
	if portErr != nil {
		logs, _ := dockercli.Run(ctx, nil, "logs", "--tail", "20", id)
		r.Remove(ctx, id)
		return Started{}, fmt.Errorf("%w: %s", portErr, logs)
	}

	return Started{ID: id, Port: port}, nil
}

func (r *Runtime) Running(ctx context.Context, id string) (bool, error) {
	out, err := dockercli.Run(ctx, nil, "inspect", "--format", "{{.State.Running}}", id)
	if err != nil {
		if dockercli.Gone(err) {
			return false, ErrGone
		}

		return false, err
	}

	return strings.TrimSpace(out) == "true", nil
}

func (r *Runtime) ListLabeled(ctx context.Context, label string) (map[string]string, error) {
	return dockercli.ListLabeled(ctx, label)
}

func (r *Runtime) Remove(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()

	// Docker removes anonymous image volumes with -v while retaining named data volumes.
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
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() { r.Remove(context.Background(), id) })
	}

	wg.Wait()
}
