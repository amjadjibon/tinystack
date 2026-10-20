package databasemigrationservice

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func checkInstance(i *api.ReplicationInstance) error {
	if !strings.HasPrefix(common.Value(i.ReplicationInstanceClass), "dms.") ||
		len(common.Value(i.ReplicationInstanceClass)) < 7 {
		return invalid("ReplicationInstanceClass must be a DMS instance class.")
	}

	if common.Value(i.AllocatedStorage) < 5 || common.Value(i.AllocatedStorage) > 6144 {
		return invalid("AllocatedStorage must be between 5 and 6144 GiB.")
	}

	if strings.TrimSpace(common.Value(i.EngineVersion)) == "" {
		return invalid("EngineVersion cannot be empty.")
	}

	if common.Value(i.MultiAZ) && i.AvailabilityZone != nil {
		return invalid("AvailabilityZone cannot be specified with MultiAZ.")
	}

	if i.AvailabilityZone != nil {
		found := false
		for _, sn := range i.ReplicationSubnetGroup.Subnets {
			found = found || common.Value(sn.SubnetAvailabilityZone.Name) == *i.AvailabilityZone
		}

		if !found {
			return invalid("AvailabilityZone must be covered by the subnet group.")
		}
	}

	return nil
}

func (s *Service) instanceGroups(tx storage.ReadTx, scope request.Scope, i *api.ReplicationInstance,
	ids []string,
) error {
	groups, err := s.network.MountGroups(tx, scope, common.Value(i.ReplicationSubnetGroup.VpcId), ids)
	if err != nil {
		return invalid("VPC security groups must exist in the subnet group's VPC.")
	}

	i.VpcSecurityGroups = []api.VpcSecurityGroupMembership{}
	for _, id := range groups {
		i.VpcSecurityGroups = append(i.VpcSecurityGroups, api.VpcSecurityGroupMembership{
			VpcSecurityGroupId: new(id), Status: new("active"),
		})
	}

	return nil
}

func (s *Service) CreateReplicationInstance(ctx context.Context, in *api.CreateReplicationInstanceMessage,
) (*api.CreateReplicationInstanceResponse, error) {
	r, err := s.create(ctx, kindInstance, common.Value(in.ReplicationInstanceIdentifier), in.Tags,
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			group, e := byName(tx, scope, kindSubnet, common.Value(in.ReplicationSubnetGroupIdentifier))
			if e != nil {
				return e
			}

			i := &api.ReplicationInstance{
				ReplicationInstanceIdentifier: new(r.Name),
				ReplicationInstanceArn:        new(r.ARN),
				ReplicationInstanceStatus:     new(stateCreating),
				InstanceCreateTime:            common.SecondsMilli(time.Now()),
				ReplicationSubnetGroup:        group.Subnet,
				ReplicationInstanceClass:      in.ReplicationInstanceClass,
				AllocatedStorage: common.OrElse(
					in.AllocatedStorage,
					new(int64(50)),
				),
				AvailabilityZone: in.AvailabilityZone,
				EngineVersion: common.OrElse(
					in.EngineVersion,
					new("3.5.3"),
				),
				MultiAZ:                    common.OrElse(in.MultiAZ, new(false)),
				PubliclyAccessible:         common.OrElse(in.PubliclyAccessible, new(true)),
				AutoMinorVersionUpgrade:    common.OrElse(in.AutoMinorVersionUpgrade, new(true)),
				KmsKeyId:                   in.KmsKeyId,
				PreferredMaintenanceWindow: common.OrElse(in.PreferredMaintenanceWindow, new("sun:00:00-sun:00:30")),
			}
			if e := checkInstance(i); e != nil {
				return e
			}

			if e := s.instanceGroups(tx, scope, i, in.VpcSecurityGroupIds); e != nil {
				return e
			}

			r.Instance = i
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.CreateReplicationInstanceResponse{ReplicationInstance: r.Instance}, nil
}

func (s *Service) ModifyReplicationInstance(ctx context.Context, in *api.ModifyReplicationInstanceMessage,
) (*api.ModifyReplicationInstanceResponse, error) {
	r, err := s.change(ctx, kindInstance, common.Value(in.ReplicationInstanceArn),
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			i := r.Instance
			if common.Value(i.ReplicationInstanceStatus) != stateAvailable {
				return stateError()
			}

			if in.AllocatedStorage != nil && *in.AllocatedStorage < common.Value(i.AllocatedStorage) {
				return invalid("AllocatedStorage cannot be decreased.")
			}

			// Changes are applied synchronously to this metadata record, including deferred requests.
			i.ReplicationInstanceIdentifier = common.OrElse(
				in.ReplicationInstanceIdentifier,
				i.ReplicationInstanceIdentifier,
			)
			r.Name = common.Value(i.ReplicationInstanceIdentifier)
			i.ReplicationInstanceClass = common.OrElse(in.ReplicationInstanceClass, i.ReplicationInstanceClass)
			i.AllocatedStorage = common.OrElse(in.AllocatedStorage, i.AllocatedStorage)
			i.PreferredMaintenanceWindow = common.OrElse(in.PreferredMaintenanceWindow, i.PreferredMaintenanceWindow)
			i.MultiAZ = common.OrElse(in.MultiAZ, i.MultiAZ)
			if in.EngineVersion != nil &&
				strings.Split(*in.EngineVersion, ".")[0] != strings.Split(*i.EngineVersion, ".")[0] &&
				!common.Value(in.AllowMajorVersionUpgrade) {
				return invalid("AllowMajorVersionUpgrade is required.")
			}

			i.EngineVersion = common.OrElse(in.EngineVersion, i.EngineVersion)
			i.AutoMinorVersionUpgrade = common.OrElse(in.AutoMinorVersionUpgrade, i.AutoMinorVersionUpgrade)
			if e := checkInstance(i); e != nil {
				return e
			}

			if in.VpcSecurityGroupIds != nil {
				return s.instanceGroups(tx, scope, i, in.VpcSecurityGroupIds)
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.ModifyReplicationInstanceResponse{ReplicationInstance: r.Instance}, nil
}

func (s *Service) RebootReplicationInstance(ctx context.Context, in *api.RebootReplicationInstanceMessage,
) (*api.RebootReplicationInstanceResponse, error) {
	r, err := s.change(ctx, kindInstance, common.Value(in.ReplicationInstanceArn),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			if common.Value(r.Instance.ReplicationInstanceStatus) != stateAvailable {
				return stateError()
			}

			if common.Value(in.ForcePlannedFailover) && !common.Value(r.Instance.MultiAZ) {
				return invalid("ForcePlannedFailover requires a MultiAZ instance.")
			}

			r.Instance.ReplicationInstanceStatus = new("rebooting")
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.RebootReplicationInstanceResponse{ReplicationInstance: r.Instance}, nil
}

func (s *Service) DeleteReplicationInstance(ctx context.Context, in *api.DeleteReplicationInstanceMessage,
) (*api.DeleteReplicationInstanceResponse, error) {
	r, err := s.remove(ctx, kindInstance, common.Value(in.ReplicationInstanceArn))
	if err != nil {
		return nil, err
	}

	r.Instance.ReplicationInstanceStatus = new("deleting")
	return &api.DeleteReplicationInstanceResponse{ReplicationInstance: r.Instance}, nil
}
