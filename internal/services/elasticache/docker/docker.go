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
	"strconv"
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

	port, portErr := hostPort(ctx, id, spec.Port)
	if portErr != nil {
		r.Remove(ctx, id)
		return elasticache.Started{}, portErr
	}

	return elasticache.Started{ID: id, Port: port}, nil
}

func hostPort(ctx context.Context, id string, containerPort int64) (int64, error) {
	out, err := dockercli.Run(ctx, nil, "port", id, strconv.FormatInt(containerPort, 10)+"/tcp")
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
	out, err := dockercli.Run(
		ctx,
		nil,
		"ps",
		"-a",
		"--filter",
		"label="+label,
		"--format",
		"{{.ID}} {{.Label \""+label+"\"}}",
	)
	if err != nil {
		return nil, err
	}

	res := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			res[parts[0]] = parts[1]
		}
	}

	return res, nil
}

func (r *Runtime) Remove(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()

	_, _ = dockercli.Run(ctx, nil, "rm", "-f", id)
	r.mu.Lock()
	delete(r.started, id)
	r.mu.Unlock()
}

func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	ids := slices.Collect(maps.Keys(r.started))
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() { r.Remove(context.Background(), id) })
	}

	wg.Wait()
}
