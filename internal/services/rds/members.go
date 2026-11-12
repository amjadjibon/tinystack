package rds

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) createClusterInstance(
	ctx context.Context,
	in *api.CreateDBInstanceMessage,
) (*api.CreateDBInstanceResult, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	id, class := strings.ToLower(common.Value(in.DBInstanceIdentifier)), common.Value(in.DBInstanceClass)
	if !validName(id, 63) || !strings.HasPrefix(class, "db.") || len(class) < 4 {
		return nil, invalid("Invalid DBInstanceIdentifier or DBInstanceClass")
	}

	if in.MasterUsername != nil || in.MasterUserPassword != nil || in.DBName != nil || in.AllocatedStorage != nil ||
		in.DBSubnetGroupName != nil || in.EngineVersion != nil || in.CopyTagsToSnapshot != nil {
		return nil, invalid(
			"Cluster members inherit database settings; specify only identifier, class, engine, cluster and tags",
		)
	}

	if in.DBParameterGroupName != nil && common.Value(in.Engine) != EngineNeptune {
		return nil, invalid("Only Neptune cluster members take a DB parameter group")
	}

	if in.DBClusterIdentifier == nil || *in.DBClusterIdentifier == "" {
		return nil, invalid("DBClusterIdentifier is required")
	}

	tags, tagsErr := parseTags(in.Tags)
	if tagsErr != nil {
		return nil, tagsErr
	}

	var result *api.DBInstance
	s.mu.Lock()
	defer s.mu.Unlock()
	writeErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		parent, err := readRecord(tx, scope, kindCluster, *in.DBClusterIdentifier)
		if err != nil {
			return err
		}

		c := parent.Cluster
		if common.Value(c.Engine) != common.Value(in.Engine) {
			return invalid("Engine must match the DB cluster")
		}

		if _, found, err := storage.Read[record](tx, scope.Key(serviceName, kindDB, id)); err != nil {
			return err
		} else if found {
			return resourceError(kindDB, "AlreadyExists", id)
		}

		r := &record{Name: id, Kind: kindDB, Tags: tags, ReadyAt: parent.ReadyAt, Instance: &api.DBInstance{
			DBInstanceIdentifier: new(
				id,
			),
			DBInstanceClass:     in.DBInstanceClass,
			Engine:              c.Engine,
			EngineVersion:       c.EngineVersion,
			DBClusterIdentifier: c.DBClusterIdentifier,
			MasterUsername:      c.MasterUsername,
			DBInstanceStatus:    cluster(parent).Status,
			DBInstanceArn:       new(arn(scope, kindDB, id)),
			InstanceCreateTime:  new(time.Now().UTC()),
			Endpoint:            &api.Endpoint{Address: c.Endpoint, Port: c.Port},
		}}
		groups, groupErr := memberParameterGroup(tx, scope, c, in.DBParameterGroupName)
		if groupErr != nil {
			return groupErr
		}

		r.Instance.DBParameterGroups = groups

		c.DBClusterMembers = append(c.DBClusterMembers, api.DBClusterMember{
			DBInstanceIdentifier: new(id), IsClusterWriter: new(len(c.DBClusterMembers) == 0),
		})

		if err := writeRecord(tx, scope, parent); err != nil {
			return err
		}

		if err := writeRecord(tx, scope, r); err != nil {
			return err
		}

		result = instance(r)
		return nil
	})

	if writeErr != nil {
		return nil, writeErr
	}

	return &api.CreateDBInstanceResult{DBInstance: result}, nil
}

func deleteClusterMember(tx storage.WriteTx, scope request.Scope, r *record) error {
	parent, err := readRecord(tx, scope, kindCluster, common.Value(r.Instance.DBClusterIdentifier))
	if err != nil {
		return err
	}

	c := parent.Cluster
	c.DBClusterMembers = slices.DeleteFunc(c.DBClusterMembers, func(m api.DBClusterMember) bool {
		return common.Value(m.DBInstanceIdentifier) == r.Name
	})

	if len(c.DBClusterMembers) > 0 && !slices.ContainsFunc(c.DBClusterMembers, func(m api.DBClusterMember) bool {
		return common.Value(m.IsClusterWriter)
	}) {
		c.DBClusterMembers[0].IsClusterWriter = new(true)
	}

	if err := writeRecord(tx, scope, parent); err != nil {
		return err
	}

	tx.Delete(scope.Key(serviceName, kindDB, r.Name))
	return nil
}

func (s *Service) changeClusterMember(ctx context.Context, scope request.Scope, r *record,
	in *api.ModifyDBInstanceMessage,
) (*api.DBInstance, error) {
	if in == nil {
		return nil, awserror.New("InvalidDBInstanceStateFault", "Cluster member start/stop/reboot is unsupported")
	}

	if in.CopyTagsToSnapshot != nil {
		return nil, invalid("Cluster member snapshots are unsupported")
	}

	if in.MasterUserPassword != nil {
		return nil, invalid("Modify the cluster to change its password")
	}

	if in.DBInstanceClass != nil {
		if !strings.HasPrefix(*in.DBInstanceClass, "db.") || len(*in.DBInstanceClass) < 4 {
			return nil, invalid("Invalid DBInstanceClass")
		}

		r.Instance.DBInstanceClass = in.DBInstanceClass
	}

	if in.DBParameterGroupName != nil && common.Value(r.Instance.Engine) != EngineNeptune {
		return nil, invalid("Only Neptune cluster members take a DB parameter group")
	}

	if err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.DBParameterGroupName != nil {
			parent, err := readRecord(tx, scope, kindCluster, common.Value(r.Instance.DBClusterIdentifier))
			if err != nil {
				return err
			}

			if r.Instance.DBParameterGroups, err = memberParameterGroup(
				tx, scope, parent.Cluster, in.DBParameterGroupName,
			); err != nil {
				return err
			}
		}

		return writeRecord(tx, scope, r)
	}); err != nil {
		return nil, err
	}

	return instance(r), nil
}

// memberParameterGroup resolves a Neptune member's DB parameter group, defaulting to its cluster family's group. The
// member has no engine of its own, so the group is always in sync.
func memberParameterGroup(
	tx storage.ReadTx,
	scope request.Scope,
	c *api.DBCluster,
	name *string,
) ([]api.DBParameterGroupStatus, error) {
	if common.Value(c.Engine) != EngineNeptune {
		return nil, nil
	}

	family := clusterFamily(EngineNeptune, common.Value(c.EngineVersion))
	group, err := readRecord(tx, scope, kindParameter, common.Value(common.OrElse(name, new("default."+family))))
	if err != nil {
		return nil, err
	}

	if parameterFamily(group) != family {
		return nil, invalid("DBParameterGroupFamily does not match the cluster engine version")
	}

	return []api.DBParameterGroupStatus{
		{DBParameterGroupName: new(group.Name), ParameterApplyStatus: new(parameterInSync)},
	}, nil
}
