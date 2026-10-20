package databasemigrationservice

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) subnetGroup(tx storage.ReadTx, scope request.Scope, name, description string,
	ids []string,
) (*api.ReplicationSubnetGroup, error) {
	if description == "" {
		return nil, invalid("A subnet group description is required.")
	}

	out := &api.ReplicationSubnetGroup{
		ReplicationSubnetGroupIdentifier:  new(name),
		ReplicationSubnetGroupDescription: new(description), SubnetGroupStatus: new("Complete"),
		SupportedNetworkTypes: []string{"IPV4"},
	}
	zones := map[string]bool{}
	seen := map[string]bool{}
	for _, id := range ids {
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return nil, awserror.New("InvalidSubnet", "A specified EC2 subnet does not exist.")
		}

		if seen[id] {
			return nil, invalid("Subnet IDs must be unique.")
		}

		seen[id] = true
		if out.VpcId != nil && common.Value(out.VpcId) != common.Value(sn.VpcId) {
			return nil, awserror.New("InvalidSubnet", "Subnets must belong to the same VPC.")
		}

		out.VpcId = sn.VpcId
		zones[common.Value(sn.AvailabilityZone)] = true
		out.Subnets = append(out.Subnets, api.Subnet{
			SubnetIdentifier: new(id), SubnetStatus: new("Active"),
			SubnetAvailabilityZone: &api.AvailabilityZone{Name: sn.AvailabilityZone},
		})
	}

	if len(zones) < 2 {
		return nil, awserror.New(
			"ReplicationSubnetGroupDoesNotCoverEnoughAZs",
			"Subnets must cover at least two Availability Zones.",
		)
	}

	return out, nil
}

func (s *Service) CreateReplicationSubnetGroup(ctx context.Context, in *api.CreateReplicationSubnetGroupMessage,
) (*api.CreateReplicationSubnetGroupResponse, error) {
	r, err := s.create(ctx, kindSubnet, common.Value(in.ReplicationSubnetGroupIdentifier), in.Tags,
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			var e error
			r.Subnet, e = s.subnetGroup(
				tx,
				scope,
				r.Name,
				common.Value(in.ReplicationSubnetGroupDescription),
				in.SubnetIds,
			)
			return e
		})
	if err != nil {
		return nil, err
	}

	return &api.CreateReplicationSubnetGroupResponse{ReplicationSubnetGroup: r.Subnet}, nil
}

func (s *Service) ModifyReplicationSubnetGroup(ctx context.Context, in *api.ModifyReplicationSubnetGroupMessage,
) (*api.ModifyReplicationSubnetGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := resourceARN(scope, kindSubnet, common.Value(in.ReplicationSubnetGroupIdentifier))
	r, err := s.change(ctx, kindSubnet, arn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		ids := in.SubnetIds
		if ids == nil {
			for _, sn := range r.Subnet.Subnets {
				ids = append(ids, common.Value(sn.SubnetIdentifier))
			}
		}

		description := common.Value(
			common.OrElse(in.ReplicationSubnetGroupDescription, r.Subnet.ReplicationSubnetGroupDescription),
		)
		group, e := s.subnetGroup(tx, scope, r.Name, description, ids)
		if e != nil {
			return e
		}

		instances, e := storage.Records[record](tx, scope.Prefix(serviceName, kindInstance))
		if e != nil {
			return e
		}

		for _, i := range instances {
			if common.Value(i.Instance.ReplicationSubnetGroup.ReplicationSubnetGroupIdentifier) != r.Name {
				continue
			}

			if common.Value(group.VpcId) != common.Value(i.Instance.ReplicationSubnetGroup.VpcId) {
				return stateError()
			}

			i.Instance.ReplicationSubnetGroup = group
			if e := storage.Write(tx, recordKey(scope, i), i); e != nil {
				return e
			}
		}

		r.Subnet = group
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyReplicationSubnetGroupResponse{ReplicationSubnetGroup: r.Subnet}, nil
}

func (s *Service) DeleteReplicationSubnetGroup(ctx context.Context, in *api.DeleteReplicationSubnetGroupMessage,
) (*api.DeleteReplicationSubnetGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	_, err = s.remove(
		ctx,
		kindSubnet,
		resourceARN(scope, kindSubnet, common.Value(in.ReplicationSubnetGroupIdentifier)),
	)
	if err != nil {
		return nil, err
	}

	return &api.DeleteReplicationSubnetGroupResponse{}, nil
}
