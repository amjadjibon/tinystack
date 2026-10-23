package ecs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
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
	// Metadata is committed with the started container.
	Metadata          *ContainerMetadata
	HealthCheck       *api.HealthCheck
	Labels            map[string]string
	Environment       map[string]string
	Image             string
	Name              string
	User              string
	WorkingDirectory  string
	MetadataToken     string
	TaskID            string
	Mounts            []MountSpec
	VolumesFrom       []api.VolumeFrom
	EntryPoint        []string
	Command           []string
	Ports             []PortSpec
	CPU               int64
	Memory            int64
	MemoryReservation int64
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
	s.bg.Every(tick, s.visit)
}

func indexKey(scope request.Scope, clusterName, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + clusterName + "/" + id
}

// visit advances each task the runner owns that is not already advancing, so
// a slow image pull holds up only its own task.
func (s *Service) visit(ctx context.Context) {
	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
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
		s.bg.Go(func(ctx context.Context) {
			defer func() {
				s.mu.Lock()
				delete(s.busy, k)
				s.mu.Unlock()
			}()

			if err := s.advance(ctx, k); err != nil && ctx.Err() == nil {
				s.logger.Error("ecs task", "task", k, "error", err)
			}
		})
	}
}

// advance moves the task an index key names one step: it starts a pending
// task's containers, watches a running task's containers, and removes a stopped
// task's containers.
func (s *Service) advance(ctx context.Context, k string) error {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	s.mu.Lock()
	runContext := s.runContext
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Reset cancellation belongs to the service, while ctx inherits the runner cancellation.
	//nolint:contextcheck // Combine reset and runner lifetimes.
	stopCancel := context.AfterFunc(runContext, cancel)
	defer stopCancel()
	if runContext.Err() != nil {
		return nil
	}

	parts := strings.Split(strings.TrimPrefix(k, indexPrefix), "/")
	if len(parts) != 4 {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(k)
			return nil
		})
	}

	scope := request.Scope{AccountID: parts[0], Region: parts[1]}
	var t *task
	var def *taskDef
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, exists := tx.Get(k); !exists {
			return nil
		}

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
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(k)
			return nil
		})
	case t.Last == stopped:
		return s.finish(ctx, scope, k, t, def)
	case t.Last == pending:
		return s.launch(ctx, scope, t, def)
	default:
		return s.watch(ctx, scope, t, def)
	}
}

// launch starts a pending task's containers and records them, or stops the task
// when one fails to start.
func (s *Service) launch(ctx context.Context, scope request.Scope, t *task, def *taskDef) error {
	ctx, cancel := s.startupContext(ctx, scope, t)
	defer cancel()
	specs, err := s.containerSpecs(scope, t, def)
	started := make([]Started, len(specs))
	if err == nil {
		err = s.resolveSecrets(ctx, scope, specs, def)
	}

	owner := taskARN(scope, t.Cluster, t.ID)
	if rt, ok := s.runtime.(TaskRuntime); ok && err == nil {
		err = rt.PrepareTask(ctx, owner, specs, def.Def.Volumes, t.Cpu, t.Memory)
	}

	if err == nil {
		err = s.startOrdered(ctx, specs, def, started)
	}

	kept := false
	updateErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

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
			if strings.HasPrefix(err.Error(), "ResourceInitializationError:") {
				reason = err.Error()
			}

			reason = reason[:min(len(reason), maxReason)]
			for i := range cur.Containers {
				cur.Containers[i].Reason = new(reason)
			}

			if stopErr := s.stop(tx, scope, cur, "TaskFailedToStart", reason); stopErr != nil {
				return stopErr
			}

			return s.replace(tx, scope, c, cur)
		}

		cur.Metadata = make(map[string]ContainerMetadata)
		cur.Last = running
		cur.Version++
		for i := range cur.Containers {
			ct := &cur.Containers[i]
			ct.RuntimeId, ct.LastStatus = new(started[i].ID), new(running)
			if specs[i].Metadata != nil {
				rec := *specs[i].Metadata
				rec.DockerID = started[i].ID
				rec.Ports = nil
				for _, p := range started[i].Ports {
					rec.Ports = append(
						rec.Ports,
						MetadataPortMapping{ContainerPort: p.Container, HostPort: p.Host, Protocol: p.Protocol},
					)
				}

				cur.Metadata[specs[i].MetadataToken] = rec
				if writeErr := storage.Write(tx, "aws/ecs/metadata/"+specs[i].MetadataToken,
					taskKey(scope, cur.Cluster, cur.ID)); writeErr != nil {
					return writeErr
				}
			}

			ct.NetworkBindings = []api.NetworkBinding{}
			for _, p := range started[i].Ports {
				ct.NetworkBindings = append(ct.NetworkBindings, api.NetworkBinding{
					BindIP: new("127.0.0.1"), ContainerPort: new(p.Container), HostPort: new(p.Host),
					Protocol: new(p.Protocol),
				})
			}
		}

		if writeErr := storage.Write(tx, taskKey(scope, cur.Cluster, cur.ID), cur); writeErr != nil {
			return writeErr
		}

		kept = true
		return nil
	})

	kept = kept && updateErr == nil
	if !kept {
		for _, st := range started {
			if st.ID != "" {
				s.runtime.Remove(context.WithoutCancel(ctx), st.ID)
			}
		}
	}

	if !kept {
		if rt, ok := s.runtime.(TaskRuntime); ok {
			rt.RemoveTask(ctx, owner)
		}
	}

	return updateErr
}

// containerSpecs describes the containers of t, failing for the definition
// features the runtime does not provide.
func (s *Service) containerSpecs(
	scope request.Scope,
	t *task,
	def *taskDef,
) ([]ContainerSpec, error) {
	if err := checkExecution(&def.Def); err != nil {
		return nil, err
	}

	specs := make([]ContainerSpec, 0, len(def.Def.ContainerDefinitions))
	for i := range def.Def.ContainerDefinitions {
		d := &def.Def.ContainerDefinitions[i]

		spec := ContainerSpec{
			TaskID: taskARN(scope, t.Cluster, t.ID), HealthCheck: d.HealthCheck,
			CPU: common.Value(d.Cpu), Memory: common.Value(d.Memory),
			MemoryReservation: common.Value(d.MemoryReservation), VolumesFrom: d.VolumesFrom,
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
		for key, value := range d.DockerLabels {
			if !strings.HasPrefix(key, "tinystack.") {
				spec.Labels[key] = value
			}
		}

		for _, kv := range d.Environment {
			spec.Environment[common.Value(kv.Name)] = common.Value(kv.Value)
		}

		if t.Overrides != nil {
			for _, o := range t.Overrides.ContainerOverrides {
				if common.Value(o.Name) != spec.Name {
					continue
				}

				spec.CPU = common.Value(common.OrElse(o.Cpu, new(spec.CPU)))
				spec.Memory = common.Value(common.OrElse(o.Memory, new(spec.Memory)))
				spec.MemoryReservation = common.Value(common.OrElse(o.MemoryReservation, new(spec.MemoryReservation)))

				if o.Command != nil {
					spec.Command = o.Command
				}

				for _, kv := range o.Environment {
					spec.Environment[common.Value(kv.Name)] = common.Value(kv.Value)
				}
			}
		}

		for _, m := range d.MountPoints {
			spec.Mounts = append(spec.Mounts, MountSpec{
				Source: common.Value(m.SourceVolume),
				Target: common.Value(m.ContainerPath), ReadOnly: common.Value(m.ReadOnly),
			})
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

		if s.endpoint != "" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}

			token := hex.EncodeToString(b)

			spec.Environment["ECS_CONTAINER_METADATA_URI_V4"] = s.endpoint + "/_tinystack/ecs/v4/" + token
			spec.Environment["AWS_CONTAINER_CREDENTIALS_FULL_URI"] = s.endpoint + "/_tinystack/ecs/credentials"
			spec.Environment["AWS_CONTAINER_AUTHORIZATION_TOKEN"] = token

			var revision string
			if def != nil && def.Def.Revision != nil {
				revision = strconv.FormatInt(*def.Def.Revision, 10)
			}

			var ports []MetadataPortMapping
			for _, p := range spec.Ports {
				ports = append(ports, MetadataPortMapping{
					ContainerPort: p.Container,
					HostPort:      p.Host,
					Protocol:      p.Protocol,
				})
			}

			metaRec := ContainerMetadata{
				Scope:         scope,
				Cluster:       t.Cluster,
				TaskID:        t.ID,
				TaskARN:       taskARN(scope, t.Cluster, t.ID),
				Family:        t.Family,
				Revision:      revision,
				ContainerName: spec.Name,
				Image:         spec.Image,
				TaskRoleARN:   common.Value(def.Def.TaskRoleArn),
				CreatedAt:     t.Created,
				StartedAt:     s.now().UTC(),
				CPU:           d.Cpu,
				Memory:        d.Memory,
				Ports:         ports,
				Labels:        spec.Labels,
			}

			spec.Metadata = &metaRec
			spec.MetadataToken = token
		}

		specs = append(specs, spec)
	}

	return specs, nil
}

// watch ships a running task's logs and records the containers that exited,
// stopping the task when an essential one did.
func (s *Service) watch(ctx context.Context, scope request.Scope, t *task, def *taskDef) error {
	type exit struct {
		code *int64
		gone bool
	}

	exits := map[int]exit{}
	health := map[int]string{}
	unhealthy := false
	for i, c := range t.Containers {
		if common.Value(c.LastStatus) != running || c.RuntimeId == nil {
			continue
		}

		if rt, ok := s.runtime.(HealthRuntime); ok && def.Def.ContainerDefinitions[i].HealthCheck != nil {
			status, err := rt.Health(ctx, *c.RuntimeId)
			if errors.Is(err, ErrGone) {
				exits[i] = exit{gone: true}
				continue
			}

			if err != nil {
				return err
			}

			health[i] = status
			unhealthy = unhealthy ||
				status == healthUnhealthy && common.Value(def.Def.ContainerDefinitions[i].Essential)
		}

		s.ship(ctx, scope, t, &def.Def.ContainerDefinitions[i], *c.RuntimeId)
		exited, code, err := s.runtime.State(ctx, *c.RuntimeId)
		switch {
		case errors.Is(err, ErrGone):
			exits[i] = exit{gone: true}
		case err != nil:
			return err
		case exited:
			exits[i] = exit{code: new(code)}
		}
	}

	if len(exits) == 0 && len(health) == 0 {
		return nil
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		cur, found, err := storage.Read[task](tx, taskKey(scope, t.Cluster, t.ID))
		if err != nil || !found || cur.Last != running {
			return err
		}

		for i, status := range health {
			cur.Containers[i].HealthStatus = new(status)
		}

		essential, left := unhealthy, 0
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

		reason := "Essential container in task exited"
		if unhealthy {
			reason = "Essential container failed health checks"
		}

		if stopErr := s.stop(tx, scope, cur, "EssentialContainerExited", reason); stopErr != nil {
			return stopErr
		}

		return s.replace(tx, scope, c, cur)
	})
}

// finish ships the last logs of a stopped task's containers, removes them, and
// lets the task go.
func (s *Service) finish(ctx context.Context, scope request.Scope, k string, t *task, def *taskDef) error {
	for i, c := range t.Containers {
		if c.RuntimeId == nil {
			continue
		}

		s.ship(ctx, scope, t, &def.Def.ContainerDefinitions[i], *c.RuntimeId)
		s.runtime.Remove(ctx, *c.RuntimeId)
		s.mu.Lock()
		delete(s.cursors, *c.RuntimeId)
		s.mu.Unlock()
	}

	if rt, ok := s.runtime.(TaskRuntime); ok {
		rt.RemoveTask(ctx, taskARN(scope, t.Cluster, t.ID))
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(k)
		return nil
	})
}

// ship appends what a container with the awslogs driver wrote since the last
// call to its log stream: <prefix>/<container>/<task ID> with a stream prefix,
// and the container's runtime ID without one.
func (s *Service) ship(ctx context.Context, scope request.Scope, t *task, d *api.ContainerDefinition, id string) {
	lc := d.LogConfiguration
	if lc == nil || common.Value(lc.LogDriver) != logAWS || lc.Options["awslogs-group"] == "" || s.writeLogs == nil {
		return
	}

	s.mu.Lock()
	since := s.cursors[id]
	s.mu.Unlock()
	lines, err := s.runtime.Logs(ctx, id, since)
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

	if err = s.writeLogs(ctx, scope, lc.Options["awslogs-group"], stream, messages); err != nil {
		s.logger.Error("ecs logs", "group", lc.Options["awslogs-group"], "error", err)
		return
	}

	s.mu.Lock()
	s.cursors[id] = lines[len(lines)-1].Time
	s.mu.Unlock()
}
