package ecs

import (
	"context"
	"fmt"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/containerenv"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	healthHealthy   = "HEALTHY"
	healthUnhealthy = "UNHEALTHY"
)

// MountSpec mounts a task volume by name. For standalone containers, Source is a host path
// or empty for an anonymous volume.
type MountSpec struct {
	Source   string
	Target   string
	ReadOnly bool
}

// TaskRuntime owns infrastructure shared by the containers of a task.
type TaskRuntime interface {
	PrepareTask(context.Context, string, []ContainerSpec, []api.Volume, string, string) error
	RemoveTask(context.Context, string)
}

// HealthRuntime reports Docker's health status, including UNKNOWN during startup.
type HealthRuntime interface {
	Health(context.Context, string) (string, error)
}

func (s *Service) resolveSecrets(ctx context.Context, scope request.Scope, specs []ContainerSpec, def *taskDef) error {
	if len(specs) != len(def.Def.ContainerDefinitions) {
		return fmt.Errorf("container specification count does not match definition")
	}

	for i := range specs {
		d := &def.Def.ContainerDefinitions[i]
		for _, secret := range d.Secrets {
			value, err := containerenv.ResolveSecret(ctx, s.caller, scope, common.Value(secret.ValueFrom))
			if err != nil {
				return fmt.Errorf(
					"ResourceInitializationError: unable to resolve secret for %s",
					common.Value(secret.Name),
				)
			}

			// #nosec G602 -- equal lengths are checked above; i ranges over specs.
			specs[i].Environment[common.Value(secret.Name)] = value
		}
	}

	return nil
}

// startOrdered waits only outside storage transactions. A bounded startup also
// catches unhealthy dependencies and jobs that never complete.
func (s *Service) startOrdered(ctx context.Context, specs []ContainerSpec, def *taskDef, started []Started) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	names := map[string]int{}
	for i := range specs {
		spec := &specs[i]
		names[spec.Name] = i
	}

	waitingSince := make([]time.Time, len(specs))
	left := len(specs)
	for left > 0 {
		progress := false
		for i := range specs {
			spec := &specs[i]
			if started[i].ID != "" {
				continue
			}

			ready := true
			for _, dep := range def.Def.ContainerDefinitions[i].DependsOn {
				j := names[common.Value(dep.ContainerName)]
				if started[j].ID == "" {
					ready = false
					continue
				}

				condition := common.Value(dep.Condition)
				if condition == "START" {
					continue
				}

				exited, code, err := s.runtime.State(ctx, started[j].ID)
				if err != nil {
					return err
				}

				if condition == healthHealthy {
					rt, ok := s.runtime.(HealthRuntime)
					if !ok {
						return fmt.Errorf("runtime does not support health dependencies")
					}

					health, healthErr := rt.Health(ctx, started[j].ID)
					if healthErr != nil {
						return healthErr
					}

					if exited || health == healthUnhealthy {
						return fmt.Errorf("dependency %s failed HEALTHY condition", specs[j].Name)
					}

					ready = ready && health == healthHealthy
				} else {
					if exited && condition == "SUCCESS" && code != 0 {
						return fmt.Errorf("dependency %s failed SUCCESS condition", specs[j].Name)
					}

					ready = ready && exited
				}
			}

			for _, source := range spec.VolumesFrom {
				ready = ready && started[names[common.Value(source.SourceContainer)]].ID != ""
			}

			if !ready {
				if waitingSince[i].IsZero() {
					waitingSince[i] = time.Now()
				}

				timeout := common.Value(common.OrElse(def.Def.ContainerDefinitions[i].StartTimeout, new(int64(300))))
				if time.Since(waitingSince[i]) > time.Duration(timeout)*time.Second {
					return fmt.Errorf("dependency conditions timed out for %s", spec.Name)
				}

				continue
			}

			st, err := s.runtime.Start(ctx, spec)
			if err != nil {
				return err
			}

			started[i] = st
			left--
			progress = true
		}

		if !progress {
			select {
			case <-ctx.Done():
				return fmt.Errorf("container dependency startup timed out: %w", ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}

	return nil
}

// StopTask can interrupt image pulls and dependency waits before launch commits.
func (s *Service) startupContext(
	ctx context.Context,
	scope request.Scope,
	t *task,
) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pendingTask := false
				err := s.store.View(ctx, func(tx storage.ReadTx) error {
					cur, found, readErr := storage.Read[task](tx, taskKey(scope, t.Cluster, t.ID))
					pendingTask = found && cur.Last == pending
					return readErr
				})
				if err != nil || !pendingTask {
					cancel()
					return
				}
			}
		}
	}()

	return ctx, cancel
}
