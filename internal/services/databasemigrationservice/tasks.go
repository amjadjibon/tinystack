package databasemigrationservice

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkTask(t *api.ReplicationTask) error {
	if !slices.Contains([]string{"full-load", "cdc", "full-load-and-cdc"}, common.Value(t.MigrationType)) {
		return invalid("MigrationType must be full-load, cdc, or full-load-and-cdc.")
	}

	var mappings struct {
		Rules []map[string]json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal([]byte(common.Value(t.TableMappings)), &mappings); err != nil || len(mappings.Rules) == 0 {
		return invalid("TableMappings must be a JSON object with a nonempty rules array.")
	}

	ids := map[string]bool{}
	names := map[string]bool{}
	for _, rule := range mappings.Rules {
		var kind, id, name string
		if json.Unmarshal(rule["rule-type"], &kind) != nil ||
			!slices.Contains(
				[]string{"selection", "transformation", "table-settings", "post-processing", "object-mapping"},
				kind,
			) ||
			json.Unmarshal(rule["rule-id"], &id) != nil || id == "" ||
			json.Unmarshal(rule["rule-name"], &name) != nil || name == "" || ids[id] || names[name] {
			return invalid("Each mapping rule requires a supported rule-type and unique string rule-id and rule-name.")
		}

		ids[id] = true
		names[name] = true
		if kind == "selection" {
			var action string
			var locator map[string]string
			if json.Unmarshal(rule["rule-action"], &action) != nil ||
				!slices.Contains([]string{"include", "exclude", "explicit"}, action) ||
				json.Unmarshal(rule["object-locator"], &locator) != nil || locator["schema-name"] == "" ||
				locator["table-name"] == "" {
				return invalid(
					"Selection rules require an include/exclude/explicit action and schema-name/table-name locator.",
				)
			}
		}
	}

	if t.ReplicationTaskSettings != nil {
		var settings map[string]json.RawMessage
		if json.Unmarshal([]byte(*t.ReplicationTaskSettings), &settings) != nil || settings == nil {
			return invalid("ReplicationTaskSettings must be a JSON object.")
		}
	}

	return nil
}

func (s *Service) CreateReplicationTask(ctx context.Context, in *api.CreateReplicationTaskMessage,
) (*api.CreateReplicationTaskResponse, error) {
	r, err := s.create(ctx, kindTask, common.Value(in.ReplicationTaskIdentifier), in.Tags,
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			source, e := read(tx, scope, kindEndpoint, common.Value(in.SourceEndpointArn))
			if e != nil {
				return e
			}

			target, e := read(tx, scope, kindEndpoint, common.Value(in.TargetEndpointArn))
			if e != nil {
				return e
			}

			if common.Value(source.Endpoint.EndpointType) != "source" ||
				common.Value(target.Endpoint.EndpointType) != "target" {
				return invalid("Endpoints must have the corresponding source and target types.")
			}

			instance, e := read(tx, scope, kindInstance, common.Value(in.ReplicationInstanceArn))
			if e != nil {
				return e
			}

			if common.Value(instance.Instance.ReplicationInstanceStatus) != stateAvailable {
				return stateError()
			}

			t := &api.ReplicationTask{
				ReplicationTaskIdentifier:   new(r.Name),
				ReplicationTaskArn:          new(r.ARN),
				SourceEndpointArn:           in.SourceEndpointArn,
				TargetEndpointArn:           in.TargetEndpointArn,
				ReplicationInstanceArn:      in.ReplicationInstanceArn,
				MigrationType:               in.MigrationType,
				TableMappings:               in.TableMappings,
				ReplicationTaskSettings:     in.ReplicationTaskSettings,
				Status:                      new(stateCreating),
				ReplicationTaskCreationDate: common.SecondsMilli(time.Now()),
			}
			if e := checkTask(t); e != nil {
				return e
			}

			r.Task = t
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.CreateReplicationTaskResponse{ReplicationTask: r.Task}, nil
}

func (s *Service) ModifyReplicationTask(ctx context.Context, in *api.ModifyReplicationTaskMessage,
) (*api.ModifyReplicationTaskResponse, error) {
	r, err := s.change(ctx, kindTask, common.Value(in.ReplicationTaskArn),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			t := r.Task
			if !slices.Contains([]string{stateReady, stateStopped}, common.Value(t.Status)) {
				return stateError()
			}

			t.ReplicationTaskIdentifier = common.OrElse(in.ReplicationTaskIdentifier, t.ReplicationTaskIdentifier)
			r.Name = common.Value(t.ReplicationTaskIdentifier)
			t.MigrationType = common.OrElse(in.MigrationType, t.MigrationType)
			t.TableMappings = common.OrElse(in.TableMappings, t.TableMappings)
			t.ReplicationTaskSettings = common.OrElse(in.ReplicationTaskSettings, t.ReplicationTaskSettings)
			return checkTask(t)
		})
	if err != nil {
		return nil, err
	}

	return &api.ModifyReplicationTaskResponse{ReplicationTask: r.Task}, nil
}

func (s *Service) StartReplicationTask(ctx context.Context, in *api.StartReplicationTaskMessage,
) (*api.StartReplicationTaskResponse, error) {
	r, err := s.change(ctx, kindTask, common.Value(in.ReplicationTaskArn),
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			t := r.Task
			mode := common.Value(in.StartReplicationTaskType)
			if !slices.Contains([]string{"start-replication", "resume-processing", "reload-target"}, mode) {
				return invalid("Invalid StartReplicationTaskType.")
			}

			if !slices.Contains([]string{stateReady, stateStopped}, common.Value(t.Status)) {
				return stateError()
			}

			if (mode == "start-replication") != (t.ReplicationTaskStartDate == nil) {
				return stateError()
			}

			instance, e := read(tx, scope, kindInstance, common.Value(t.ReplicationInstanceArn))
			if e != nil {
				return e
			}

			if common.Value(instance.Instance.ReplicationInstanceStatus) != stateAvailable {
				return stateError()
			}

			t.Status = new(stateStarting)
			t.StopReason = nil
			t.ReplicationTaskStartDate = common.SecondsMilli(time.Now())
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.StartReplicationTaskResponse{ReplicationTask: r.Task}, nil
}

func (s *Service) StopReplicationTask(ctx context.Context, in *api.StopReplicationTaskMessage,
) (*api.StopReplicationTaskResponse, error) {
	r, err := s.change(ctx, kindTask, common.Value(in.ReplicationTaskArn),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			if !slices.Contains([]string{stateStarting, stateRunning}, common.Value(r.Task.Status)) {
				return stateError()
			}

			r.Task.Status = new(stateStopped)
			r.Task.StopReason = new("Stop requested; simulated records only, no data migrated")
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.StopReplicationTaskResponse{ReplicationTask: r.Task}, nil
}

func (s *Service) DeleteReplicationTask(ctx context.Context, in *api.DeleteReplicationTaskMessage,
) (*api.DeleteReplicationTaskResponse, error) {
	r, err := s.remove(ctx, kindTask, common.Value(in.ReplicationTaskArn))
	if err != nil {
		return nil, err
	}

	r.Task.Status = new("deleting")
	return &api.DeleteReplicationTaskResponse{ReplicationTask: r.Task}, nil
}
