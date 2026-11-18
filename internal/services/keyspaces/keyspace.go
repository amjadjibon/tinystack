package keyspaces

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var regionName = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+-\d+$`)

func replication(region string, spec *api.ReplicationSpecification) (*api.ReplicationSpecification, error) {
	if spec == nil {
		return &api.ReplicationSpecification{ReplicationStrategy: new(singleRegion)}, nil
	}

	switch common.Value(spec.ReplicationStrategy) {
	case singleRegion:
		if spec.RegionList != nil {
			return nil, awserror.Validation("RegionList is only valid for MULTI_REGION keyspaces")
		}
	case multiRegion:
		if len(spec.RegionList) < 2 || !slices.Contains(spec.RegionList, region) {
			return nil, awserror.Validation("MULTI_REGION requires at least two Regions including %s", region)
		}

		seen := make(map[string]bool, len(spec.RegionList))
		for _, replica := range spec.RegionList {
			if !regionName.MatchString(replica) || seen[replica] {
				return nil, awserror.Validation("Invalid or duplicate replication Region %q", replica)
			}

			seen[replica] = true
		}
	default:
		return nil, awserror.Validation("ReplicationStrategy must be SINGLE_REGION or MULTI_REGION")
	}

	return spec, nil
}

func (s *Service) CreateKeyspace(
	ctx context.Context, in *api.CreateKeyspaceRequest,
) (*api.CreateKeyspaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.KeyspaceName)
	if writableErr := writable(name); writableErr != nil {
		return nil, writableErr
	}

	spec, err := replication(scope.Region, in.ReplicationSpecification)
	if err != nil {
		return nil, err
	}

	tags, err := checkTags(in.Tags)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindKeyspace, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := createRecord(tx, key); e != nil {
			return e
		}

		r := &record{Keyspace: name, Name: name, Tags: tags, Replication: spec}
		return s.writeChange(tx, scope, key, r, creating, createKeyspaceCQL(name))
	})
	return &api.CreateKeyspaceResponse{ResourceArn: new(resourceARN(scope, name, ""))}, err
}

func (s *Service) UpdateKeyspace(
	ctx context.Context, in *api.UpdateKeyspaceRequest,
) (*api.UpdateKeyspaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.KeyspaceName)
	if writableErr := writable(name); writableErr != nil {
		return nil, writableErr
	}

	spec, err := replication(scope.Region, in.ReplicationSpecification)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindKeyspace, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord(tx, key)
		if e != nil {
			return e
		}

		if e := updateReplication(tx, scope, r, spec, in.ClientSideTimestamps); e != nil {
			return e
		}

		r.Replication = spec
		// A local Cassandra node always has one replica. The Region list is control-plane metadata.
		return s.writeChange(tx, scope, key, r, updating, "ALTER KEYSPACE "+identifier(name)+localReplication)
	})
	return &api.UpdateKeyspaceResponse{ResourceArn: new(resourceARN(scope, name, ""))}, err
}

// updateReplication validates region expansion and updates timestamps in the same transaction as the keyspace.
func updateReplication(
	tx storage.WriteTx, scope request.Scope, r *record, spec *api.ReplicationSpecification,
	timestamps *api.ClientSideTimestamps,
) error {
	for _, region := range r.Replication.RegionList {
		if !slices.Contains(spec.RegionList, region) {
			return awserror.Validation("Removing Regions from a multi-Region keyspace is unsupported")
		}
	}

	if timestamps != nil && (common.Value(timestamps.Status) != enabled || len(spec.RegionList) == 0) {
		return awserror.Validation("Client-side timestamps must be ENABLED and require multi-Region replication")
	}

	if len(spec.RegionList) <= len(r.Replication.RegionList) && timestamps == nil {
		return nil
	}

	for _, key := range tx.Keys(childKey(scope, kindTable, r.Name, "")) {
		table, _, err := storage.Read[record](tx, key)
		if err != nil {
			return err
		}

		if table.Table.ClientSideTimestamps != nil {
			continue
		}

		if timestamps == nil {
			return awserror.Validation("All tables require client-side timestamps before adding a Region")
		}

		table.Table.ClientSideTimestamps = timestamps
		if err := storage.Write(tx, key, table); err != nil {
			return err
		}
	}

	return nil
}

func (s *Service) DeleteKeyspace(
	ctx context.Context, in *api.DeleteKeyspaceRequest,
) (*api.DeleteKeyspaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.KeyspaceName)
	if writableErr := writable(name); writableErr != nil {
		return nil, writableErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := requireKeyspace(tx, scope, name); e != nil {
			return e
		}

		if len(tx.Keys(childKey(scope, kindTable, name, ""))) > 0 ||
			len(tx.Keys(childKey(scope, kindType, name, ""))) > 0 {
			return conflict("Delete the keyspace's tables and user-defined types first")
		}

		return s.writeChange(tx, scope, scope.Key(serviceName, kindKeyspace, name), nil, "",
			"DROP KEYSPACE IF EXISTS "+identifier(name))
	})
	return &api.DeleteKeyspaceResponse{}, err
}

func (s *Service) GetKeyspace(ctx context.Context, in *api.GetKeyspaceRequest) (*api.GetKeyspaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.KeyspaceName)
	if systemKeyspace(name) {
		return s.keyspaceOutput(scope, &record{Name: name, Replication: &api.ReplicationSpecification{
			ReplicationStrategy: new(singleRegion),
		}}), nil
	}

	var out *api.GetKeyspaceResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := readRecord(tx, scope.Key(serviceName, kindKeyspace, name))
		if e != nil {
			return e
		}

		out = s.keyspaceOutput(scope, r)
		return nil
	})
	return out, err
}

func (s *Service) keyspaceOutput(scope request.Scope, r *record) *api.GetKeyspaceResponse {
	out := &api.GetKeyspaceResponse{
		KeyspaceName: new(r.Name), ResourceArn: new(resourceARN(scope, r.Name, "")),
		ReplicationStrategy: r.Replication.ReplicationStrategy, ReplicationRegions: r.Replication.RegionList,
	}
	status := s.status(r)
	for _, region := range out.ReplicationRegions {
		progress := "0%"
		if status == active {
			progress = "100%"
		}

		out.ReplicationGroupStatuses = append(out.ReplicationGroupStatuses, api.ReplicationGroupStatus{
			Region: new(region), KeyspaceStatus: new(status), TablesReplicationProgress: new(progress),
		})
	}

	return out
}

func (s *Service) ListKeyspaces(
	ctx context.Context, in *api.ListKeyspacesRequest,
) (*api.ListKeyspacesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	systems := []string{"system", "system_schema", "system_schema_mcs", "system_multiregion_info"}
	keyspaces := make([]api.KeyspaceSummary, 0, len(systems))
	for _, name := range systems {
		keyspaces = append(keyspaces, api.KeyspaceSummary{
			KeyspaceName: new(
				name,
			),
			ResourceArn:         new(resourceARN(scope, name, "")),
			ReplicationStrategy: new(singleRegion),
		})
	}

	id := func(k api.KeyspaceSummary) string { return common.Value(k.KeyspaceName) }
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[record](tx, scope.Prefix(serviceName, kindKeyspace))
		if e != nil {
			return e
		}

		for _, r := range records {
			keyspaces = append(keyspaces, api.KeyspaceSummary{
				KeyspaceName: new(r.Name), ResourceArn: new(resourceARN(scope, r.Name, "")),
				ReplicationStrategy: r.Replication.ReplicationStrategy, ReplicationRegions: r.Replication.RegionList,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Built-in keyspaces have no records. Merge their names into the page order.
	slices.SortFunc(keyspaces, func(a, b api.KeyspaceSummary) int { return strings.Compare(id(a), id(b)) })
	page, next, err := resourcePage(keyspaces, id, in.NextToken, in.MaxResults)
	return &api.ListKeyspacesResponse{Keyspaces: page, NextToken: next}, err
}
