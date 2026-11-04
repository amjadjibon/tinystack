package servicediscovery

import (
	"context"
	"maps"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicediscovery"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func instance(r *record, id string) (*api.Instance, error) {
	x := r.Instances[id]
	if x == nil {
		return nil, awserror.New("InstanceNotFound", "Instance not found: "+id)
	}

	return x, nil
}

func (s *Service) RegisterInstance(
	ctx context.Context,
	in *api.RegisterInstanceRequest,
) (*api.RegisterInstanceResponse, error) {
	if common.Value(in.InstanceId) == "" || len(in.Attributes) == 0 {
		return nil, invalid("InstanceId and Attributes required")
	}

	status := in.Attributes["AWS_INIT_HEALTH_STATUS"]
	if status == "" {
		status = healthy
	}

	if status != healthy && status != unhealthy {
		return nil, invalid("Invalid initial health status")
	}

	out := &api.RegisterInstanceResponse{}
	err := s.change(
		ctx,
		kindService,
		common.Value(in.ServiceId),
		func(r *record, tx storage.WriteTx, scope request.Scope) error {
			previous, replayed, txErr := idempotency.Replay[string](
				tx,
				idempotency.Key(scope, serviceName, "RegisterInstance", in.CreatorRequestId),
				in,
				duplicateRequest(),
			)
			if txErr != nil {
				return txErr
			}

			if replayed {
				out.OperationId = new(previous)
				return nil
			}

			r.Instances[*in.InstanceId] = &api.Instance{
				Id:               in.InstanceId,
				CreatorRequestId: in.CreatorRequestId,
				Attributes:       in.Attributes,
			}

			r.Health[*in.InstanceId] = status
			r.Revision++
			r.Service.InstanceCount = new(int64(len(r.Instances)))
			out.OperationId, txErr = operation(
				tx,
				scope,
				"REGISTER_INSTANCE",
				map[string]string{targetService: r.ID, "INSTANCE": *in.InstanceId},
			)
			if txErr != nil {
				return txErr
			}
			return idempotency.Remember(
				tx,
				idempotency.Key(scope, serviceName, "RegisterInstance", in.CreatorRequestId),
				in,
				*out.OperationId,
			)
		},
	)
	return out, err
}

func (s *Service) DeregisterInstance(
	ctx context.Context,
	in *api.DeregisterInstanceRequest,
) (*api.DeregisterInstanceResponse, error) {
	out := &api.DeregisterInstanceResponse{}
	err := s.change(
		ctx,
		kindService,
		common.Value(in.ServiceId),
		func(r *record, tx storage.WriteTx, scope request.Scope) error {
			if _, txErr := instance(r, common.Value(in.InstanceId)); txErr != nil {
				return txErr
			}

			delete(r.Instances, *in.InstanceId)
			delete(r.Health, *in.InstanceId)
			r.Revision++
			r.Service.InstanceCount = new(int64(len(r.Instances)))
			var txErr error
			out.OperationId, txErr = operation(
				tx,
				scope,
				"DEREGISTER_INSTANCE",
				map[string]string{targetService: r.ID, "INSTANCE": *in.InstanceId},
			)
			return txErr
		},
	)
	return out, err
}

func (s *Service) GetInstance(ctx context.Context, in *api.GetInstanceRequest) (*api.GetInstanceResponse, error) {
	out := &api.GetInstanceResponse{}
	err := s.view(
		ctx,
		kindService,
		common.Value(in.ServiceId),
		func(r *record, _ storage.ReadTx, _ request.Scope) error {
			var txErr error
			out.Instance, txErr = instance(r, common.Value(in.InstanceId))
			return txErr
		},
	)
	return out, err
}

func (s *Service) ListInstances(ctx context.Context, in *api.ListInstancesRequest) (*api.ListInstancesResponse, error) {
	out := &api.ListInstancesResponse{Instances: []api.InstanceSummary{}}
	err := s.view(
		ctx,
		kindService,
		common.Value(in.ServiceId),
		func(r *record, _ storage.ReadTx, _ request.Scope) error {
			items, next, txErr := page(
				slices.Sorted(maps.Keys(r.Instances)),
				func(id string) string { return id },
				in.NextToken,
				in.MaxResults,
			)
			if txErr != nil {
				return txErr
			}

			out.NextToken = next
			for _, id := range items {
				x := r.Instances[id]
				out.Instances = append(out.Instances, api.InstanceSummary{Id: x.Id, Attributes: x.Attributes})
			}

			return nil
		},
	)
	return out, err
}

func (s *Service) GetInstancesHealthStatus(
	ctx context.Context,
	in *api.GetInstancesHealthStatusRequest,
) (*api.GetInstancesHealthStatusResponse, error) {
	out := &api.GetInstancesHealthStatusResponse{Status: map[string]string{}}
	err := s.view(
		ctx,
		kindService,
		common.Value(in.ServiceId),
		func(r *record, _ storage.ReadTx, _ request.Scope) error {
			ids := []string{}
			for _, id := range slices.Sorted(maps.Keys(r.Instances)) {
				if len(in.Instances) == 0 || slices.Contains(in.Instances, id) {
					ids = append(ids, id)
				}
			}

			ids, next, txErr := page(ids, func(id string) string { return id }, in.NextToken, in.MaxResults)
			if txErr != nil {
				return txErr
			}

			out.NextToken = next
			for _, id := range ids {
				out.Status[id] = r.Health[id]
			}

			return nil
		},
	)
	return out, err
}

func (s *Service) UpdateInstanceCustomHealthStatus(
	ctx context.Context,
	in *api.UpdateInstanceCustomHealthStatusRequest,
) (*api.UpdateInstanceCustomHealthStatusOutput, error) {
	status := common.Value(in.Status)
	if status != healthy && status != unhealthy {
		return nil, invalid("Status must be HEALTHY or UNHEALTHY")
	}

	err := s.change(
		ctx,
		kindService,
		common.Value(in.ServiceId),
		func(r *record, _ storage.WriteTx, _ request.Scope) error {
			if _, txErr := instance(r, common.Value(in.InstanceId)); txErr != nil {
				return txErr
			}

			if r.Service.HealthCheckCustomConfig == nil {
				return awserror.New("CustomHealthNotFound", "Service has no custom health check")
			}

			if r.Health[*in.InstanceId] != status {
				r.Health[*in.InstanceId] = status
				r.Revision++
			}

			return nil
		},
	)
	return &api.UpdateInstanceCustomHealthStatusOutput{}, err
}

func discover(tx storage.ReadTx, scope request.Scope, namespaceName, serviceName string) (*record, error) {
	items, err := storage.Records[record](tx, scope.Prefix("servicediscovery", kindNamespace))
	if err != nil {
		return nil, err
	}

	nsID := ""
	for _, n := range items {
		if common.Value(n.Namespace.Name) == namespaceName ||
			common.Value(n.Namespace.Properties.HttpProperties.HttpName) == namespaceName {
			nsID = n.ID
			break
		}
	}

	if nsID == "" {
		return nil, awserror.New("NamespaceNotFound", "Namespace not found")
	}

	services, err := storage.Records[record](tx, scope.Prefix("servicediscovery", kindService))
	if err != nil {
		return nil, err
	}

	for _, r := range services {
		if common.Value(r.Service.NamespaceId) == nsID && common.Value(r.Service.Name) == serviceName {
			return r, nil
		}
	}

	return nil, awserror.New("ServiceNotFound", "Service not found")
}

func attributesMatch(attributes, query map[string]string) bool {
	for k, v := range query {
		if got, ok := attributes[k]; !ok || got != v {
			return false
		}
	}

	return true
}

func (s *Service) DiscoverInstances(
	ctx context.Context,
	in *api.DiscoverInstancesRequest,
) (*api.DiscoverInstancesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	status := common.Value(common.OrElse(in.HealthStatus, new(healthy)))
	if !slices.Contains([]string{healthy, unhealthy, "ALL", healthyOrAll}, status) {
		return nil, invalid("Invalid HealthStatus")
	}

	limit := int(common.Value(common.OrElse(in.MaxResults, new(int64(100)))))
	if limit < 1 || limit > 1000 {
		return nil, invalid("MaxResults must be 1 through 1000")
	}

	out := &api.DiscoverInstancesResponse{Instances: []api.HttpInstanceSummary{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, txErr := discover(tx, scope, common.Value(in.NamespaceName), common.Value(in.ServiceName))
		if txErr != nil {
			return txErr
		}

		out.InstancesRevision = new(r.Revision)
		all := []string{}
		preferred := []string{}
		for _, id := range slices.Sorted(maps.Keys(r.Instances)) {
			if !attributesMatch(r.Instances[id].Attributes, in.QueryParameters) {
				continue
			}

			if status == "ALL" || status == healthyOrAll || r.Health[id] == status {
				all = append(all, id)
			}
		}

		if status == healthyOrAll {
			for _, id := range all {
				if r.Health[id] == healthy {
					preferred = append(preferred, id)
				}
			}

			if len(preferred) > 0 {
				all = preferred
			}
		}

		preferred = nil
		for _, id := range all {
			if attributesMatch(r.Instances[id].Attributes, in.OptionalParameters) {
				preferred = append(preferred, id)
			}
		}

		if len(preferred) > 0 {
			all = preferred
		}

		for _, id := range all[:min(len(all), limit)] {
			out.Instances = append(
				out.Instances,
				api.HttpInstanceSummary{
					InstanceId:    new(id),
					NamespaceName: in.NamespaceName,
					ServiceName:   in.ServiceName,
					HealthStatus:  new(r.Health[id]),
					Attributes:    r.Instances[id].Attributes,
				},
			)
		}

		return nil
	})
	return out, err
}

func (s *Service) DiscoverInstancesRevision(
	ctx context.Context,
	in *api.DiscoverInstancesRevisionRequest,
) (*api.DiscoverInstancesRevisionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.DiscoverInstancesRevisionResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, txErr := discover(tx, scope, common.Value(in.NamespaceName), common.Value(in.ServiceName))
		if txErr != nil {
			return txErr
		}

		out.InstancesRevision = new(r.Revision)
		return nil
	})
	return out, err
}
