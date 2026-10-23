package ecs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	healthHealthy   = "HEALTHY"
	healthUnhealthy = "UNHEALTHY"
)

// MountSpec mounts one task volume into a container.
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
			value, err := s.secret(ctx, scope, common.Value(secret.ValueFrom))
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

func (s *Service) secret(ctx context.Context, scope request.Scope, ref string) (string, error) {
	if s.caller == nil {
		return "", fmt.Errorf("secret resolver unavailable")
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
	if strings.HasPrefix(ref, "arn:") && strings.Contains(ref, ":secretsmanager:") {
		parts := strings.Split(ref, ":")
		if len(parts) < 7 || len(parts) > 8 {
			return "", fmt.Errorf("invalid secret ARN")
		}

		ctx = request.WithMetadata(ctx, request.Metadata{Scope: request.Scope{Region: parts[3], AccountID: parts[4]}})
		key := ""
		if len(parts) == 8 {
			key = parts[7]
			ref = strings.Join(parts[:7], ":")
		}

		out, err := s.caller.Base64Blobs().
			Call(ctx, "secretsmanager", "GetSecretValue", map[string]any{"SecretId": ref})
		if err != nil {
			return "", err
		}

		value, ok := out["SecretString"].(string)
		if !ok {
			if encoded, exists := out["SecretBinary"].(string); exists {
				data, decodeErr := base64.StdEncoding.DecodeString(encoded)
				if decodeErr != nil {
					return "", decodeErr
				}

				value = string(data)
			} else {
				return "", fmt.Errorf("secret has no string value")
			}
		}

		if key != "" {
			var object map[string]json.RawMessage
			if parseErr := json.Unmarshal([]byte(value), &object); parseErr != nil {
				return "", parseErr
			}

			raw, found := object[key]
			if !found {
				return "", fmt.Errorf("JSON key missing")
			}

			value = string(raw)
			if len(raw) > 0 && raw[0] == '"' {
				if decodeErr := json.Unmarshal(raw, &value); decodeErr != nil {
					return "", decodeErr
				}
			}
		}

		return value, nil
	}

	parameterARN := false
	if strings.HasPrefix(ref, "arn:") {
		parameterARN = true
		parts := strings.SplitN(ref, ":", 6)
		if len(parts) != 6 || parts[2] != "ssm" || !strings.HasPrefix(parts[5], "parameter/") {
			return "", fmt.Errorf("invalid SSM parameter ARN")
		}

		scope = request.Scope{AccountID: parts[4], Region: parts[3]}
		ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
		ref = "/" + strings.TrimPrefix(parts[5], "parameter/")
	}

	out, err := s.caller.Call(ctx, "ssm", "GetParameter", map[string]any{"Name": ref, "WithDecryption": true})
	if err != nil && parameterARN {
		var awsErr *awserror.Error
		if errors.As(err, &awsErr) && awsErr.Code == "ParameterNotFound" {
			out, err = s.caller.Call(ctx, "ssm", "GetParameter",
				map[string]any{"Name": strings.TrimPrefix(ref, "/"), "WithDecryption": true})
		}
	}

	if err != nil {
		return "", err
	}

	parameter, ok := out["Parameter"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("parameter has no value")
	}

	value, ok := parameter["Value"].(string)
	if !ok {
		return "", fmt.Errorf("parameter has no string value")
	}

	return value, nil
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
