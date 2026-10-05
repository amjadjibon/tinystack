package ecs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	indexPrefix = "aws/ecs/index/"
	// tick is how often the runner visits the tasks it owns.
	tick = time.Second
	// maxReason is how much of a start error a task's stopped reason keeps.
	maxReason = 255
)

// ErrGone is the error Runtime.State returns for a container that no longer
// exists.
var ErrGone = errors.New("container is gone")

// Runtime runs the containers of tasks. The runner calls it outside
// transactions.
type Runtime interface {
	// Start runs a container and returns its ID and the host ports its
	// container ports are published on.
	Start(ctx context.Context, spec *ContainerSpec) (Started, error)
	// State reports whether a container exited, and its exit code.
	State(ctx context.Context, id string) (exited bool, code int64, err error)
	// Logs returns what a container wrote after since, in time order.
	Logs(ctx context.Context, id string, since time.Time) ([]LogLine, error)
	// Remove deletes a container, running or not.
	Remove(ctx context.Context, id string)
	// Close removes every container the runtime started.
	Close()
}

// ContainerSpec is a container of a task, from its definition and overrides.
type ContainerSpec struct {
	Environment      map[string]string
	Labels           map[string]string
	Name             string
	Image            string
	WorkingDirectory string
	User             string
	EntryPoint       []string
	Command          []string
	Ports            []PortSpec
}

// PortSpec publishes a container port on a host port, any free one when Host
// is 0.
type PortSpec struct {
	Protocol  string
	Container int64
	Host      int64
}

// Started is a container a runtime started.
type Started struct {
	ID    string
	Ports []PortSpec
}

// LogLine is a line a container wrote.
type LogLine struct {
	Time    time.Time
	Message string
}

// LogWriter appends messages to a CloudWatch Logs stream, creating the group
// and stream on first use (logs.Service.Append).
type LogWriter func(ctx context.Context, scope request.Scope, group, stream string, messages []string) error

// UseRuntime runs tasks' containers in rt and ships the output of containers
// with the awslogs driver through writeLogs. Call it before serving requests;
// Stop ends the runner and closes rt.
func (s *Service) UseRuntime(rt Runtime, writeLogs LogWriter, logger *slog.Logger) {
	s.runtime, s.writeLogs, s.logger = rt, writeLogs, logger
	s.wg.Go(s.run)
}

func indexKey(scope request.Scope, clusterName, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + clusterName + "/" + id
}

func (s *Service) run() {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.visit()
		}
	}
}

// visit advances each task the runner owns that is not already advancing, so
// a slow image pull holds up only its own task.
func (s *Service) visit() {
	var keys []string
	if err := s.store.View(s.ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(indexPrefix)
		return nil
	}); err != nil {
		return
	}

	for _, k := range keys {
		s.mu.Lock()
		if s.busy[k] {
			s.mu.Unlock()
			continue
		}

		s.busy[k] = true
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() {
				s.mu.Lock()
				delete(s.busy, k)
				s.mu.Unlock()
			}()

			if err := s.advance(k); err != nil && s.ctx.Err() == nil {
				s.logger.Error("ecs task", "task", k, "error", err)
			}
		})
	}
}

// advance moves the task an index key names one step: it starts a pending
// task's containers, watches a running task's containers, and removes a stopped
// task's containers.
func (s *Service) advance(k string) error {
	parts := strings.Split(strings.TrimPrefix(k, indexPrefix), "/")
	if len(parts) != 4 {
		return s.store.Update(s.ctx, func(tx storage.WriteTx) error {
			tx.Delete(k)
			return nil
		})
	}

	scope := request.Scope{AccountID: parts[0], Region: parts[1]}
	var t *task
	var def *taskDef
	err := s.store.View(s.ctx, func(tx storage.ReadTx) error {
		var found bool
		var readErr error
		if t, found, readErr = storage.Read[task](tx, taskKey(scope, parts[2], parts[3])); readErr != nil || !found {
			t = nil
			return readErr
		}

		def, readErr = resolveDef(tx, scope, &t.Definition)
		return readErr
	})
	if err != nil {
		return err
	}

	switch {
	case t == nil:
		return s.store.Update(s.ctx, func(tx storage.WriteTx) error {
			tx.Delete(k)
			return nil
		})
	case t.Last == stopped:
		return s.finish(scope, k, t, def)
	case t.Last == pending:
		return s.launch(scope, t, def)
	default:
		return s.watch(scope, t, def)
	}
}

// launch starts a pending task's containers and records them, or stops the task
// when one fails to start.
func (s *Service) launch(scope request.Scope, t *task, def *taskDef) error {
	specs, err := containerSpecs(scope, t, def)
	var started []Started
	for i := 0; err == nil && i < len(specs); i++ {
		var st Started
		if st, err = s.runtime.Start(s.ctx, &specs[i]); err == nil {
			started = append(started, st)
		}
	}

	kept := false
	updateErr := s.store.Update(s.ctx, func(tx storage.WriteTx) error {
		cur, found, readErr := storage.Read[task](tx, taskKey(scope, t.Cluster, t.ID))
		// A task stopped while its containers started keeps none of them.
		if readErr != nil || !found || cur.Last != pending {
			return readErr
		}

		c, loadErr := loadCluster(tx, scope, &cur.Cluster)
		if loadErr != nil {
			return loadErr
		}

		if err != nil {
			reason := "CannotStartContainerError: " + err.Error()
			reason = reason[:min(len(reason), maxReason)]
			for i := range cur.Containers {
				cur.Containers[i].Reason = new(reason)
			}

			if stopErr := s.stop(tx, scope, cur, "TaskFailedToStart", reason); stopErr != nil {
				return stopErr
			}

			return s.replace(tx, scope, c, cur)
		}

		cur.Last = running
		cur.Version++
		for i := range cur.Containers {
			ct := &cur.Containers[i]
			ct.RuntimeId, ct.LastStatus = new(started[i].ID), new(running)
			ct.NetworkBindings = []api.NetworkBinding{}
			for _, p := range started[i].Ports {
				ct.NetworkBindings = append(ct.NetworkBindings, api.NetworkBinding{
					BindIP: new("127.0.0.1"), ContainerPort: new(p.Container), HostPort: new(p.Host),
					Protocol: new(p.Protocol),
				})
			}
		}

		kept = true
		return storage.Write(tx, taskKey(scope, cur.Cluster, cur.ID), cur)
	})
	if !kept {
		for _, st := range started {
			s.runtime.Remove(context.WithoutCancel(s.ctx), st.ID)
		}
	}

	return updateErr
}

// containerSpecs describes the containers of t, failing for the definition
// features the runtime does not provide.
func containerSpecs(scope request.Scope, t *task, def *taskDef) ([]ContainerSpec, error) {
	specs := make([]ContainerSpec, 0, len(def.Def.ContainerDefinitions))
	for i := range def.Def.ContainerDefinitions {
		d := &def.Def.ContainerDefinitions[i]
		if len(d.Secrets) > 0 || len(d.MountPoints) > 0 || len(d.VolumesFrom) > 0 || len(d.DependsOn) > 0 ||
			len(d.Links) > 0 {
			return nil, errors.New(
				"container " + common.Value(d.Name) + " uses secrets, volumes, dependencies, or links, " +
					"which TinyStack does not run",
			)
		}

		spec := ContainerSpec{
			Name: common.Value(
				d.Name,
			),
			Image:            common.Value(d.Image),
			WorkingDirectory: common.Value(d.WorkingDirectory),
			User:             common.Value(d.User),
			EntryPoint:       d.EntryPoint,
			Command:          d.Command,
			Environment:      map[string]string{"AWS_REGION": scope.Region, "AWS_DEFAULT_REGION": scope.Region},
			Labels: map[string]string{
				"tinystack.ecs.task": taskARN(scope, t.Cluster, t.ID), "tinystack.ecs.container": common.Value(d.Name),
			},
		}
		for _, kv := range d.Environment {
			spec.Environment[common.Value(kv.Name)] = common.Value(kv.Value)
		}

		if t.Overrides != nil {
			for _, o := range t.Overrides.ContainerOverrides {
				if common.Value(o.Name) != spec.Name {
					continue
				}

				if o.Command != nil {
					spec.Command = o.Command
				}

				for _, kv := range o.Environment {
					spec.Environment[common.Value(kv.Name)] = common.Value(kv.Value)
				}
			}
		}

		for _, p := range d.PortMappings {
			protocol := common.Value(p.Protocol)
			if protocol == "" {
				protocol = "tcp"
			}

			// awsvpc tasks get their own address in AWS; here every task
			// shares the host, so their ports go to free host ports.
			host := common.Value(p.HostPort)
			if t.Network != nil {
				host = 0
			}

			spec.Ports = append(
				spec.Ports,
				PortSpec{Container: common.Value(p.ContainerPort), Host: host, Protocol: protocol},
			)
		}

		specs = append(specs, spec)
	}

	return specs, nil
}

// watch ships a running task's logs and records the containers that exited,
// stopping the task when an essential one did.
func (s *Service) watch(scope request.Scope, t *task, def *taskDef) error {
	type exit struct {
		code *int64
		gone bool
	}

	exits := map[int]exit{}
	for i, c := range t.Containers {
		if common.Value(c.LastStatus) != running || c.RuntimeId == nil {
			continue
		}

		s.ship(scope, t, &def.Def.ContainerDefinitions[i], *c.RuntimeId)
		exited, code, err := s.runtime.State(s.ctx, *c.RuntimeId)
		switch {
		case errors.Is(err, ErrGone):
			exits[i] = exit{gone: true}
		case err != nil:
			return err
		case exited:
			exits[i] = exit{code: new(code)}
		}
	}

	if len(exits) == 0 {
		return nil
	}

	return s.store.Update(s.ctx, func(tx storage.WriteTx) error {
		cur, found, err := storage.Read[task](tx, taskKey(scope, t.Cluster, t.ID))
		if err != nil || !found || cur.Last != running {
			return err
		}

		essential, left := false, 0
		for i := range cur.Containers {
			e, ok := exits[i]
			if !ok {
				if common.Value(cur.Containers[i].LastStatus) == running {
					left++
				}

				continue
			}

			c := &cur.Containers[i]
			c.LastStatus, c.ExitCode = new(stopped), e.code
			if e.gone {
				c.Reason = new("The container no longer exists.")
			}

			essential = essential || common.Value(def.Def.ContainerDefinitions[i].Essential)
		}

		if !essential && left > 0 {
			cur.Version++
			return storage.Write(tx, taskKey(scope, cur.Cluster, cur.ID), cur)
		}

		c, loadErr := loadCluster(tx, scope, &cur.Cluster)
		if loadErr != nil {
			return loadErr
		}

		if stopErr := s.stop(tx, scope, cur, "EssentialContainerExited",
			"Essential container in task exited"); stopErr != nil {
			return stopErr
		}

		return s.replace(tx, scope, c, cur)
	})
}

// finish ships the last logs of a stopped task's containers, removes them, and
// lets the task go.
func (s *Service) finish(scope request.Scope, k string, t *task, def *taskDef) error {
	for i, c := range t.Containers {
		if c.RuntimeId == nil {
			continue
		}

		s.ship(scope, t, &def.Def.ContainerDefinitions[i], *c.RuntimeId)
		s.runtime.Remove(s.ctx, *c.RuntimeId)
		s.mu.Lock()
		delete(s.cursors, *c.RuntimeId)
		s.mu.Unlock()
	}

	return s.store.Update(s.ctx, func(tx storage.WriteTx) error {
		tx.Delete(k)
		return nil
	})
}

// ship appends what a container with the awslogs driver wrote since the last
// call to its log stream: <prefix>/<container>/<task ID> with a stream prefix,
// and the container's runtime ID without one.
func (s *Service) ship(scope request.Scope, t *task, d *api.ContainerDefinition, id string) {
	lc := d.LogConfiguration
	if lc == nil || common.Value(lc.LogDriver) != "awslogs" || lc.Options["awslogs-group"] == "" || s.writeLogs == nil {
		return
	}

	s.mu.Lock()
	since := s.cursors[id]
	s.mu.Unlock()
	lines, err := s.runtime.Logs(s.ctx, id, since)
	if err != nil || len(lines) == 0 {
		return
	}

	stream := id
	if prefix := lc.Options["awslogs-stream-prefix"]; prefix != "" {
		stream = prefix + "/" + common.Value(d.Name) + "/" + t.ID
	}

	if region := lc.Options["awslogs-region"]; region != "" {
		scope.Region = region
	}

	messages := make([]string, 0, len(lines))
	for _, l := range lines {
		messages = append(messages, l.Message)
	}

	if err = s.writeLogs(s.ctx, scope, lc.Options["awslogs-group"], stream, messages); err != nil {
		s.logger.Error("ecs logs", "group", lc.Options["awslogs-group"], "error", err)
		return
	}

	s.mu.Lock()
	s.cursors[id] = lines[len(lines)-1].Time
	s.mu.Unlock()
}
