// Package docker runs ElastiCache nodes' containers through the docker CLI.
//
// Each node of a cache cluster is its own container, labeled with its resource's
// ARN, with its port published on 127.0.0.1.
package docker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/elasticache"
)

const removeTimeout = 30 * time.Second

var _ elasticache.Runtime = (*Runtime)(nil)

type Runtime struct {
	started map[string]bool
	mu      sync.Mutex
	closed  bool
}

func New() *Runtime {
	return &Runtime{started: map[string]bool{}}
}

func (r *Runtime) Start(ctx context.Context, spec *elasticache.NodeSpec) (elasticache.Started, error) {
	args := []string{"run", "-d"}
	for _, k := range slices.Sorted(maps.Keys(spec.Labels)) {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}

	args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d/tcp", spec.Port))

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
		return elasticache.Started{}, fmt.Errorf("runtime is closed")
	}

	cmdArgs := append(append(args, "--", spec.Image), spec.Args...)
	id, err := dockercli.Run(ctx, nil, cmdArgs...)
	if err != nil {
		return elasticache.Started{}, err
	}

	r.mu.Lock()
	closed = r.closed
	r.started[id] = true
	r.mu.Unlock()
	if closed {
		r.Remove(ctx, id)
		return elasticache.Started{}, fmt.Errorf("runtime is closed")
	}

	port, portErr := dockercli.HostPort(ctx, id, spec.Port)
	if portErr != nil {
		r.Remove(ctx, id)
		return elasticache.Started{}, portErr
	}

	return elasticache.Started{ID: id, Port: port}, nil
}

func (r *Runtime) Running(ctx context.Context, id string) (bool, error) {
	out, err := dockercli.Run(ctx, nil, "inspect", "--format", "{{.State.Running}}", id)
	if err != nil {
		if dockercli.Gone(err) {
			return false, elasticache.ErrGone
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

	_, _ = dockercli.Run(ctx, nil, "rm", "-f", id)
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
