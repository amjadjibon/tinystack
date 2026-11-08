// Package docker runs OpenSearch domains' engines through the docker CLI.
//
// Each domain is one single-node container, labeled with the domain's ARN, with its HTTP port published on
// 127.0.0.1 and its data in a named volume.
package docker

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/opensearch"
)

const (
	label         = "tinystack.opensearch"
	labelFlag     = "--label"
	containerPort = 9200
	removeTimeout = 30 * time.Second
)

var _ opensearch.Runtime = (*Runtime)(nil)

type Runtime struct {
	specs  map[string]*opensearch.Spec
	mu     sync.Mutex
	closed bool
}

func New() *Runtime { return &Runtime{specs: map[string]*opensearch.Spec{}} }

// Resources finds containers and retained volumes left by earlier runtimes.
func (r *Runtime) Resources(ctx context.Context, owner string) ([]*opensearch.Spec, error) {
	containers, volumes, err := dockercli.ListOwned(ctx, label, "tinystack.opensearch.owner", owner)
	if err != nil {
		return nil, err
	}

	specs := make([]*opensearch.Spec, 0, len(containers)+len(volumes))
	for id, arn := range containers {
		specs = append(specs, &opensearch.Spec{Name: id, ARN: arn})
	}

	for name, arn := range volumes {
		specs = append(specs, &opensearch.Spec{Volume: name, ARN: arn})
	}

	return specs, nil
}

// runArgs builds the docker run command of a domain's container.
func runArgs(spec *opensearch.Spec) []string {
	args := []string{
		"run", "-d", "--name", spec.Name, labelFlag, label + "=" + spec.ARN,
		labelFlag, "tinystack.opensearch.owner=" + spec.Owner,
		"-p", fmt.Sprintf("127.0.0.1::%d/tcp", containerPort),
	}

	for _, env := range spec.Env {
		args = append(args, "-e", env)
	}

	return append(args, "-v", spec.Volume+":"+spec.DataPath, "--", spec.Image)
}

func (r *Runtime) Ensure(ctx context.Context, spec *opensearch.Spec) (opensearch.Started, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return opensearch.Started{}, fmt.Errorf("runtime is closed")
	}

	r.specs[spec.Name] = spec
	id, err := dockercli.Run(ctx, nil, "inspect", "--format", "{{.Id}}", spec.Name)
	if err != nil && !dockercli.Gone(err) {
		return opensearch.Started{}, err
	}

	if err == nil {
		_, err = dockercli.Run(ctx, nil, "start", spec.Name)
	} else {
		args := []string{
			"volume", "create", labelFlag, label + "=" + spec.ARN,
			labelFlag, "tinystack.opensearch.owner=" + spec.Owner, spec.Volume,
		}
		if _, err = dockercli.Run(ctx, nil, args...); err != nil {
			return opensearch.Started{}, err
		}

		id, err = dockercli.Run(ctx, nil, runArgs(spec)...)
	}

	if err != nil {
		return opensearch.Started{}, err
	}

	port, err := dockercli.HostPort(ctx, spec.Name, containerPort)
	if err != nil {
		return opensearch.Started{}, err
	}

	return opensearch.Started{ID: id, Port: port}, nil
}

// Probe asks the engine for a yellow cluster, which a single node reaches once it has started.
func (r *Runtime) Probe(ctx context.Context, port int64) error {
	url := "http://127.0.0.1:" + strconv.FormatInt(port, 10) + "/_cluster/health?wait_for_status=yellow&timeout=1s"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("cluster health: HTTP %d", res.StatusCode)
	}

	return nil
}

func (r *Runtime) Remove(ctx context.Context, spec *opensearch.Spec, volume bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
	defer cancel()
	data := ""
	if volume {
		data = spec.Volume
	}

	if err := dockercli.RemoveContainerVolume(ctx, spec.Name, data); err != nil {
		return err
	}

	r.mu.Lock()
	delete(r.specs, spec.Name)
	r.mu.Unlock()

	return nil
}

// Close removes every container the runtime started, keeps the volumes, and refuses new containers.
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	specs := slices.Collect(maps.Values(r.specs))
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, spec := range specs {
		wg.Go(func() { _ = r.Remove(context.Background(), &opensearch.Spec{Name: spec.Name}, false) })
	}

	wg.Wait()
}
