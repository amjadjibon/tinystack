package redshift

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkReferences(tx storage.ReadTx, scope request.Scope, c *api.Cluster) error {
	if c.ClusterSubnetGroupName != nil {
		if _, err := read(tx, scope, subnetKind, *c.ClusterSubnetGroupName); err != nil {
			return err
		}
	}

	_, err := read(tx, scope, parameterKind, common.Value(c.ClusterParameterGroups[0].ParameterGroupName))
	return err
}

func (s *Service) CreateCluster(ctx context.Context, in *api.CreateClusterMessage) (*api.CreateClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	if validationErr := checkPassword(in.MasterUserPassword); validationErr != nil {
		return nil, validationErr
	}

	if in.Encrypted != nil && !*in.Encrypted {
		return nil, invalid("CreateCluster requires Encrypted=true")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.ClusterIdentifier)
	clusterType := common.Value(common.OrElse(in.ClusterType, new(multiNode)))
	if clusterType == multiNode && in.NumberOfNodes == nil {
		return nil, invalid("NumberOfNodes is required for multi-node clusters")
	}

	r := &record{Name: id, Kind: clusterKind, ClusterType: clusterType, Tags: tags, Cluster: &api.Cluster{
		ClusterIdentifier: new(id),
		NodeType:          in.NodeType,
		NumberOfNodes:     common.OrElse(in.NumberOfNodes, new(int64(1))),
		MasterUsername:    in.MasterUsername,
		DBName:            common.OrElse(in.DBName, new("dev")),
		Endpoint: &api.Endpoint{
			Address: new(fmt.Sprintf("%s.%s.%s.redshift.amazonaws.com", id, common.RandomID(12), scope.Region)),
			Port:    common.OrElse(in.Port, new(int64(5439))),
		},
		ClusterStatus:          new(available),
		ClusterCreateTime:      new(time.Now().UTC()),
		ClusterNamespaceArn:    new(arn(scope, "namespace", uuid.NewV4().String())),
		ClusterSubnetGroupName: in.ClusterSubnetGroupName,
		ClusterParameterGroups: []api.ClusterParameterGroupStatus{
			{
				ParameterGroupName:   common.OrElse(in.ClusterParameterGroupName, new(defaultParameter)),
				ParameterApplyStatus: new("in-sync"),
			},
		},
		VpcSecurityGroups:                memberships(in.VpcSecurityGroupIds),
		PubliclyAccessible:               common.OrElse(in.PubliclyAccessible, new(false)),
		Encrypted:                        common.OrElse(in.Encrypted, new(true)),
		AutomatedSnapshotRetentionPeriod: common.OrElse(in.AutomatedSnapshotRetentionPeriod, new(int64(1))),
	}}
	if validationErr := checkCluster(r); validationErr != nil {
		return nil, validationErr
	}

	// Passwords are validated and omitted: this records-only service has no authentication runtime.
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkReferences(tx, scope, r.Cluster); e != nil {
			return e
		}

		return insert(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateClusterResult{Cluster: common.Clone(r.Cluster)}, nil
}

func (s *Service) ModifyCluster(ctx context.Context, in *api.ModifyClusterMessage) (*api.ModifyClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	if in.MasterUserPassword != nil {
		if validationErr := checkPassword(in.MasterUserPassword); validationErr != nil {
			return nil, validationErr
		}
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, clusterKind, common.Value(in.ClusterIdentifier))
		if e != nil {
			return e
		}

		c := r.Cluster
		if common.Value(c.ClusterStatus) != available {
			return awserror.New("InvalidClusterState", "Cluster must be available")
		}

		if in.Port != nil {
			c.Endpoint.Port = in.Port
		}

		if in.ClusterParameterGroupName != nil {
			c.ClusterParameterGroups[0].ParameterGroupName = in.ClusterParameterGroupName
		}

		if in.VpcSecurityGroupIds != nil {
			c.VpcSecurityGroups = memberships(in.VpcSecurityGroupIds)
		}

		if in.PubliclyAccessible != nil {
			c.PubliclyAccessible = in.PubliclyAccessible
		}

		if in.Encrypted != nil {
			c.Encrypted = in.Encrypted
		}

		if in.AutomatedSnapshotRetentionPeriod != nil {
			c.AutomatedSnapshotRetentionPeriod = in.AutomatedSnapshotRetentionPeriod
		}

		if e := checkCluster(r); e != nil {
			return e
		}

		if e := checkReferences(tx, scope, c); e != nil {
			return e
		}

		if e := save(tx, scope, r); e != nil {
			return e
		}

		out = common.Clone(c)
		return nil
	})
	return &api.ModifyClusterResult{Cluster: out}, err
}

func (s *Service) DescribeClusters(ctx context.Context, in *api.DescribeClustersMessage) (*api.ClustersMessage, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	rows, marker, err := s.list(ctx, clusterKind, in.ClusterIdentifier, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.ClustersMessage{Marker: marker, Clusters: []api.Cluster{}}
	for _, r := range rows {
		out.Clusters = append(out.Clusters, *r.Cluster)
	}

	return out, nil
}

func (s *Service) DeleteCluster(ctx context.Context, in *api.DeleteClusterMessage) (*api.DeleteClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	if in.FinalClusterSnapshotIdentifier != nil {
		if common.Value(in.SkipFinalClusterSnapshot) {
			return nil, invalid("FinalClusterSnapshotIdentifier conflicts with SkipFinalClusterSnapshot=true")
		}

		return nil, awserror.New("NotImplemented", "Final cluster snapshots are unsupported")
	}

	if !common.Value(in.SkipFinalClusterSnapshot) {
		return nil, invalid("Set SkipFinalClusterSnapshot=true; final snapshots are unsupported")
	}

	var out *api.Cluster
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, clusterKind, common.Value(in.ClusterIdentifier))
		if e != nil {
			return e
		}

		if common.Value(r.Cluster.ClusterStatus) != available {
			return awserror.New("InvalidClusterState", "Cluster must be available")
		}

		out = r.Cluster
		out.ClusterStatus = new("deleting")
		tx.Delete(scope.Key(serviceName, clusterKind, r.Name))
		return nil
	})
	return &api.DeleteClusterResult{Cluster: out}, err
}
