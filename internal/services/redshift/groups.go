package redshift

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) subnetConfiguration(
	tx storage.ReadTx,
	scope request.Scope,
	g *api.ClusterSubnetGroup,
	ids []string,
) error {
	name := common.Value(g.ClusterSubnetGroupName)
	if len(name) > 255 || !subnetNamePattern.MatchString(name) || name == "default" ||
		common.Value(g.Description) == "" {
		return invalid("Invalid subnet group name or description")
	}

	if len(ids) > 20 {
		return awserror.New("ClusterSubnetQuotaExceededFault", "At most 20 subnets are supported")
	}

	if len(ids) == 0 || s.network == nil {
		return awserror.New("InvalidSubnet", "SubnetIds must identify existing EC2 subnets")
	}

	g.Subnets = []api.Subnet{}
	seen := map[string]bool{}
	vpc := ""
	for _, id := range ids {
		if seen[id] {
			return awserror.New("InvalidSubnet", "SubnetIds must be unique")
		}

		seen[id] = true
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return awserror.New("InvalidSubnet", "Subnet does not exist in this account and Region")
		}

		current := common.Value(sn.VpcId)
		if vpc != "" && vpc != current {
			return awserror.New("InvalidSubnet", "Subnets must share a VPC")
		}

		vpc = current
		g.Subnets = append(g.Subnets, api.Subnet{
			SubnetIdentifier: new(id), SubnetStatus: new("Active"),
			SubnetAvailabilityZone: &api.AvailabilityZone{Name: sn.AvailabilityZone},
		})
	}

	g.VpcId = new(vpc)
	g.SubnetGroupStatus = new("Complete")
	return nil
}

func (s *Service) CreateClusterSubnetGroup(
	ctx context.Context,
	in *api.CreateClusterSubnetGroupMessage,
) (*api.CreateClusterSubnetGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.ClusterSubnetGroupName))
	r := &record{Name: name, Kind: subnetKind, Tags: tags, Subnet: &api.ClusterSubnetGroup{
		ClusterSubnetGroupName: new(name), Description: in.Description,
	}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if validationErr := s.subnetConfiguration(tx, scope, r.Subnet, in.SubnetIds); validationErr != nil {
			return validationErr
		}

		return insert(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateClusterSubnetGroupResult{ClusterSubnetGroup: common.Clone(r.Subnet)}, nil
}

func (s *Service) ModifyClusterSubnetGroup(
	ctx context.Context,
	in *api.ModifyClusterSubnetGroupMessage,
) (*api.ModifyClusterSubnetGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	var out *api.ClusterSubnetGroup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, subnetKind, common.Value(in.ClusterSubnetGroupName))
		if e != nil {
			return e
		}

		if in.Description != nil {
			r.Subnet.Description = in.Description
		}

		if validationErr := s.subnetConfiguration(tx, scope, r.Subnet, in.SubnetIds); validationErr != nil {
			return validationErr
		}

		// An in-use subnet group cannot move the cluster to another VPC.
		old, e := read(tx, scope, subnetKind, r.Name)
		if e != nil {
			return e
		}

		if common.Value(old.Subnet.VpcId) != common.Value(r.Subnet.VpcId) {
			used, useErr := groupInUse(tx, scope, subnetKind, r.Name)
			if useErr != nil {
				return useErr
			}

			if used {
				return awserror.New("SubnetAlreadyInUse", "An in-use subnet group cannot change VPC")
			}
		}

		if e := save(tx, scope, r); e != nil {
			return e
		}

		out = common.Clone(r.Subnet)
		return nil
	})
	return &api.ModifyClusterSubnetGroupResult{ClusterSubnetGroup: out}, err
}

func (s *Service) CreateClusterParameterGroup(
	ctx context.Context,
	in *api.CreateClusterParameterGroupMessage,
) (*api.CreateClusterParameterGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalid("Input is required")
	}

	in = common.Clone(in)
	name := strings.ToLower(common.Value(in.ParameterGroupName))
	if !validName(name, 255) || common.Value(in.Description) == "" {
		return nil, invalid("Invalid parameter group name or description")
	}

	if common.Value(in.ParameterGroupFamily) != family {
		return nil, invalid("Only redshift-1.0 ParameterGroupFamily is supported")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &record{Name: name, Kind: parameterKind, Tags: tags, Parameter: &api.ClusterParameterGroup{
		ParameterGroupName: new(name), ParameterGroupFamily: new(family), Description: in.Description,
	}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error { return insert(tx, scope, r) })
	if err != nil {
		return nil, err
	}

	return &api.CreateClusterParameterGroupResult{ClusterParameterGroup: common.Clone(r.Parameter)}, nil
}

func groupInUse(tx storage.ReadTx, scope request.Scope, kind, name string) (bool, error) {
	clusters, err := storage.Records[record](tx, scope.Prefix(serviceName, clusterKind))
	if err != nil {
		return false, err
	}

	for _, r := range clusters {
		if kind == subnetKind && common.Value(r.Cluster.ClusterSubnetGroupName) == name {
			return true, nil
		}

		if kind == parameterKind && common.Value(r.Cluster.ClusterParameterGroups[0].ParameterGroupName) == name {
			return true, nil
		}
	}

	return false, nil
}

func (s *Service) deleteGroup(ctx context.Context, kind, name string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, kind, name); e != nil {
			return e
		}

		used, e := groupInUse(tx, scope, kind, name)
		if e != nil {
			return e
		}

		if used || kind == parameterKind && name == defaultParameter {
			code := "InvalidClusterSubnetGroupStateFault"
			if kind == parameterKind {
				code = "InvalidClusterParameterGroupState"
			}

			return awserror.New(code, "Group is in use or is a built-in default")
		}

		tx.Delete(scope.Key(serviceName, kind, name))
		return nil
	})
}

func (s *Service) DeleteClusterSubnetGroup(
	ctx context.Context,
	in *api.DeleteClusterSubnetGroupMessage,
) (*api.DeleteClusterSubnetGroupOutput, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	err := s.deleteGroup(ctx, subnetKind, common.Value(in.ClusterSubnetGroupName))
	return &api.DeleteClusterSubnetGroupOutput{}, err
}

func (s *Service) DeleteClusterParameterGroup(
	ctx context.Context,
	in *api.DeleteClusterParameterGroupMessage,
) (*api.DeleteClusterParameterGroupOutput, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	err := s.deleteGroup(ctx, parameterKind, common.Value(in.ParameterGroupName))
	return &api.DeleteClusterParameterGroupOutput{}, err
}

func (s *Service) DescribeClusterSubnetGroups(
	ctx context.Context,
	in *api.DescribeClusterSubnetGroupsMessage,
) (*api.ClusterSubnetGroupMessage, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	rows, marker, err := s.list(ctx, subnetKind, in.ClusterSubnetGroupName, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.ClusterSubnetGroupMessage{Marker: marker, ClusterSubnetGroups: []api.ClusterSubnetGroup{}}
	for _, r := range rows {
		out.ClusterSubnetGroups = append(out.ClusterSubnetGroups, *r.Subnet)
	}

	return out, nil
}

func (s *Service) DescribeClusterParameterGroups(
	ctx context.Context,
	in *api.DescribeClusterParameterGroupsMessage,
) (*api.ClusterParameterGroupsMessage, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	rows, marker, err := s.list(ctx, parameterKind, in.ParameterGroupName, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.ClusterParameterGroupsMessage{Marker: marker, ParameterGroups: []api.ClusterParameterGroup{}}
	for _, r := range rows {
		out.ParameterGroups = append(out.ParameterGroups, *r.Parameter)
	}

	return out, nil
}
