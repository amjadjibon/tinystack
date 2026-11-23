package rds

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	pendingModificationAction = "system-update"
	nextMaintenance           = "next-maintenance"
	clusterFilter             = "db-cluster-id"
)

func maintenanceResource(scope request.Scope, resource string) (kind, name string, err error) {
	for _, kind := range []string{kindDB, kindCluster} {
		prefix := arn(scope, kind, "")
		if name, found := strings.CutPrefix(resource, prefix); found && validName(name, 63) {
			return kind, strings.ToLower(name), nil
		}
	}

	return "", "", awserror.New(
		"ResourceNotFoundFault",
		"A DB instance or cluster ARN in the request scope is required",
	)
}

func readMaintenanceResource(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	r, found, err := storage.Read[record](tx, scope.Key(serviceName, kind, name))
	if err == nil && !found {
		err = awserror.New("ResourceNotFoundFault", "The maintenance resource was not found")
	}

	return r, err
}

func maintenanceActions(scope request.Scope, r *record) api.ResourcePendingMaintenanceActions {
	out := api.ResourcePendingMaintenanceActions{ResourceIdentifier: new(arn(scope, r.Kind, r.Name))}
	if !r.MaintenanceAt.IsZero() {
		out.PendingMaintenanceActionDetails = []api.PendingMaintenanceAction{{
			Action: new(pendingModificationAction), Description: new("Apply pending DB modifications"),
			OptInStatus: r.MaintenanceOptIn, CurrentApplyDate: new(r.MaintenanceAt),
		}}
	}

	return out
}

func (s *Service) DescribePendingMaintenanceActions(
	ctx context.Context, in *api.DescribePendingMaintenanceActionsMessage,
) (*api.PendingMaintenanceActionsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	filters, err := maintenanceFilters(scope, in.Filters)
	if err != nil {
		return nil, err
	}

	var items []api.ResourcePendingMaintenanceActions
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.ResourceIdentifier != nil {
			kind, name, resourceErr := maintenanceResource(scope, *in.ResourceIdentifier)
			if resourceErr != nil {
				return resourceErr
			}

			r, readErr := readMaintenanceResource(tx, scope, kind, name)
			if readErr != nil {
				return readErr
			}

			items = appendMaintenanceActions(items, scope, r, filters)
			return nil
		}

		prefix := maintenanceIndex + scope.AccountID + "/" + scope.Region + "/"
		for _, key := range tx.Keys(prefix) {
			kind, name, ok := strings.Cut(strings.TrimPrefix(key, prefix), "/")
			if !ok {
				continue
			}

			r, readErr := readMaintenanceResource(tx, scope, kind, name)
			if readErr != nil {
				return readErr
			}

			items = appendMaintenanceActions(items, scope, r, filters)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, marker, err := pageDiscovery(items, func(v api.ResourcePendingMaintenanceActions) string {
		return common.Value(v.ResourceIdentifier)
	}, in.Marker, in.MaxRecords)
	return &api.PendingMaintenanceActionsMessage{PendingMaintenanceActions: page, Marker: marker}, err
}

func maintenanceFilters(scope request.Scope, filters []api.Filter) ([]api.Filter, error) {
	if err := validateFilters(filters, instanceFilter, clusterFilter); err != nil {
		return nil, err
	}

	out := make([]api.Filter, 0, len(filters))
	for _, filter := range filters {
		kind := kindDB
		if *filter.Name == clusterFilter {
			kind = kindCluster
		}

		normalized := api.Filter{Name: filter.Name}
		for _, value := range filter.Values {
			name, err := resourceID(scope, kind, value)
			if err != nil {
				return nil, err
			}

			normalized.Values = append(normalized.Values, name)
		}

		out = append(out, normalized)
	}

	return out, nil
}

func appendMaintenanceActions(
	items []api.ResourcePendingMaintenanceActions, scope request.Scope, r *record, filters []api.Filter,
) []api.ResourcePendingMaintenanceActions {
	key := instanceFilter
	if r.Kind == kindCluster {
		key = clusterFilter
	}

	if !r.MaintenanceAt.IsZero() && matchesFilters(filters, map[string]string{key: r.Name}) {
		return append(items, maintenanceActions(scope, r))
	}

	return items
}

func (s *Service) ApplyPendingMaintenanceAction(
	ctx context.Context, in *api.ApplyPendingMaintenanceActionMessage,
) (*api.ApplyPendingMaintenanceActionResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	kind, name, err := maintenanceResource(scope, common.Value(in.ResourceIdentifier))
	if err != nil {
		return nil, err
	}

	if common.Value(in.ApplyAction) != pendingModificationAction {
		return nil, invalid("Only system-update for pending DB modifications is supported")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		r, readErr = readMaintenanceResource(tx, scope, kind, name)
		return readErr
	})
	if err != nil {
		return nil, err
	}

	if r.MaintenanceAt.IsZero() {
		return nil, awserror.New("ResourceNotFoundFault", "No pending maintenance action for this resource")
	}

	switch common.Value(in.OptInType) {
	case "immediate":
		err = s.applyPendingMaintenance(ctx, scope, r)
	case nextMaintenance, "undo-opt-in":
		r.MaintenanceOptIn = nil
		if *in.OptInType == nextMaintenance {
			r.MaintenanceOptIn = new(nextMaintenance)
		}

		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			return saveModification(tx, scope, r, eventMaintenance, "Updated maintenance opt-in")
		})
	default:
		return nil, invalid("OptInType must be immediate, next-maintenance or undo-opt-in")
	}

	if err != nil {
		return nil, err
	}

	out := maintenanceActions(scope, r)
	return &api.ApplyPendingMaintenanceActionResult{ResourcePendingMaintenanceActions: &out}, nil
}

// applyPendingMaintenance is called with the lifecycle lock held.
func (s *Service) applyPendingMaintenance(ctx context.Context, scope request.Scope, r *record) error {
	if r.Kind == kindCluster {
		if common.Value(cluster(r).Status) != available {
			return awserror.New("InvalidDBClusterStateFault", "Cluster must be available")
		}

		applyClusterPending(r.Cluster)
	} else {
		if common.Value(instance(r).DBInstanceStatus) != available {
			return awserror.New("InvalidDBInstanceStateFault", "DB instance must be available")
		}

		if p := r.Instance.PendingModifiedValues; r.Instance.DBClusterIdentifier == nil &&
			p != nil && p.DBInstanceClass != nil {
			if err := s.instanceAction(ctx, scope, r, actionRestart); err != nil {
				return err
			}
		}

		applyInstancePending(r.Instance)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		return saveModification(tx, scope, r, eventMaintenance, "Applied pending modifications")
	})
}

func (s *Service) FailoverDBCluster(
	ctx context.Context, in *api.FailoverDBClusterMessage,
) (*api.FailoverDBClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := resourceID(scope, kindCluster, common.Value(in.DBClusterIdentifier))
	if err != nil {
		return nil, err
	}

	target, err := resourceID(scope, kindDB, common.Value(in.TargetDBInstanceIdentifier))
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	out := &api.FailoverDBClusterResult{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindCluster, name)
		if readErr != nil {
			return readErr
		}

		if !aurora(common.Value(r.Cluster.Engine)) || common.Value(cluster(r).Status) != available {
			return awserror.New("InvalidDBClusterStateFault", "Failover requires an available Aurora cluster")
		}

		index, selectErr := selectFailoverReader(tx, scope, r.Cluster.DBClusterMembers, target)
		if selectErr != nil {
			return selectErr
		}

		if eventErr := recordEvent(tx, scope, r, eventFailover, "DB cluster failover started"); eventErr != nil {
			return eventErr
		}

		for i := range r.Cluster.DBClusterMembers {
			r.Cluster.DBClusterMembers[i].IsClusterWriter = new(i == index)
		}

		if writeErr := writeRecord(tx, scope, r); writeErr != nil {
			return writeErr
		}

		out.DBCluster = cluster(r)
		return recordEvent(tx, scope, r, eventFailover, "DB cluster failover completed")
	})
	return out, err
}

func selectFailoverReader(
	tx storage.ReadTx,
	scope request.Scope,
	members []api.DBClusterMember,
	target string,
) (int, error) {
	for i, member := range members {
		name := common.Value(member.DBInstanceIdentifier)
		if (target != "" && name != target) || common.Value(member.IsClusterWriter) {
			continue
		}

		r, err := readRecord(tx, scope, kindDB, name)
		if err != nil {
			return -1, err
		}

		if common.Value(instance(r).DBInstanceStatus) != available {
			continue
		}

		return i, nil
	}

	return -1, awserror.New("InvalidDBInstanceStateFault", "Failover requires an available reader in the cluster")
}
