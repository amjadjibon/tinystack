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

func (s *Service) CreateDBSubnetGroup(
	ctx context.Context,
	in *api.CreateDBSubnetGroupMessage,
) (*api.CreateDBSubnetGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.DBSubnetGroupName))
	if !validName(name, 255) || common.Value(in.DBSubnetGroupDescription) == "" || len(in.SubnetIds) == 0 {
		return nil, invalid("Valid name, description and SubnetIds are required")
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	if s.caller == nil {
		return nil, awserror.New("InvalidSubnet", "EC2 subnet validation is unavailable")
	}

	seen := make(map[string]bool, len(in.SubnetIds))
	ids := make([]any, len(in.SubnetIds))
	for i, id := range in.SubnetIds {
		if seen[id] {
			return nil, awserror.New("InvalidSubnet", "SubnetIds must be unique")
		}
		seen[id] = true
		ids[i] = id
	}

	out, err := s.caller.Call(ctx, "ec2", "DescribeSubnets", map[string]any{"SubnetIds": ids})
	if err != nil {
		return nil, awserror.New("InvalidSubnet", err.Error())
	}

	group := &api.DBSubnetGroup{
		DBSubnetGroupName: new(name), DBSubnetGroupDescription: in.DBSubnetGroupDescription,
		DBSubnetGroupArn: new(arn(scope, kindSubnet, name)), SubnetGroupStatus: new("Complete"),
	}

	subnets, _ := out["Subnets"].([]any)
	vpc := ""
	for _, raw := range subnets {
		subnet, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		subnetID, _ := subnet["SubnetId"].(string)
		currentVpc, _ := subnet["VpcId"].(string)
		zone, _ := subnet["AvailabilityZone"].(string)
		if vpc != "" && currentVpc != vpc {
			return nil, awserror.New("InvalidSubnet", "Subnets must share a VPC")
		}

		vpc = currentVpc
		group.Subnets = append(group.Subnets, api.Subnet{
			SubnetIdentifier: new(subnetID), SubnetStatus: new("Active"),
			SubnetAvailabilityZone: &api.AvailabilityZone{Name: new(zone)},
		})
	}

	if len(group.Subnets) != len(in.SubnetIds) {
		return nil, awserror.New("InvalidSubnet", "Subnets must exist and be unique")
	}

	group.VpcId = new(vpc)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.insert(ctx, scope, &record{Name: name, Kind: kindSubnet, Subnet: group, Tags: tags}); err != nil {
		return nil, err
	}

	return &api.CreateDBSubnetGroupResult{DBSubnetGroup: group}, nil
}

func (s *Service) DescribeDBSubnetGroups(
	ctx context.Context,
	in *api.DescribeDBSubnetGroupsMessage,
) (*api.DBSubnetGroupMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items, err := s.list(ctx, scope, kindSubnet, common.Value(in.DBSubnetGroupName))
	if err != nil {
		return nil, err
	}

	items, marker, err := pageRecords(items, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DBSubnetGroupMessage{Marker: marker}
	for _, r := range items {
		out.DBSubnetGroups = append(out.DBSubnetGroups, *r.Subnet)
	}

	return out, nil
}

func (s *Service) CreateDBParameterGroup(
	ctx context.Context,
	in *api.CreateDBParameterGroupMessage,
) (*api.CreateDBParameterGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.DBParameterGroupName))
	family := common.Value(in.DBParameterGroupFamily)
	engine := ""
	version := ""
	for _, e := range []string{EnginePostgres, EngineMySQL, EngineMariaDB} {
		if strings.HasPrefix(family, e) {
			engine = e
			version = strings.TrimPrefix(family, e)
		}
	}

	if engine == "" || version == "" {
		return nil, invalid("Unsupported DBParameterGroupFamily")
	}

	if _, _, _, err = Engine(engine, version); err != nil {
		return nil, err
	}

	if !validName(name, 255) || common.Value(in.Description) == "" {
		return nil, invalid("Valid name and description are required")
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	group := &api.DBParameterGroup{
		DBParameterGroupName: new(name), DBParameterGroupFamily: new(family),
		Description: in.Description, DBParameterGroupArn: new(arn(scope, kindParameter, name)),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.insert(ctx, scope, &record{Name: name, Kind: kindParameter, Parameter: group, Tags: tags}); err != nil {
		return nil, err
	}

	return &api.CreateDBParameterGroupResult{DBParameterGroup: group}, nil
}

func (s *Service) DescribeDBParameterGroups(
	ctx context.Context,
	in *api.DescribeDBParameterGroupsMessage,
) (*api.DBParameterGroupsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	items, err := s.list(ctx, scope, kindParameter, common.Value(in.DBParameterGroupName))
	if err != nil {
		return nil, err
	}

	items, marker, err := pageRecords(items, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DBParameterGroupsMessage{Marker: marker}
	for _, r := range items {
		out.DBParameterGroups = append(out.DBParameterGroups, *r.Parameter)
	}

	return out, nil
}

func (s *Service) deleteGroup(ctx context.Context, kind, name string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		group, e := readRecord(tx, scope, kind, name)
		if e != nil {
			return e
		}

		instances, e := storage.Records[record](tx, scope.Prefix(serviceName, kindDB))
		if e != nil {
			return e
		}

		for _, r := range instances {
			used := false
			if kind == kindSubnet && r.Instance.DBSubnetGroup != nil {
				used = common.Value(r.Instance.DBSubnetGroup.DBSubnetGroupName) == group.Name
			}

			if kind == kindParameter {
				for _, p := range r.Instance.DBParameterGroups {
					if common.Value(p.DBParameterGroupName) == group.Name {
						used = true
					}
				}
			}

			if used {
				shape := "InvalidDBSubnetGroupStateFault"
				if kind == kindParameter {
					shape = "InvalidDBParameterGroupStateFault"
				}

				return awserror.New(shape, "Group is in use")
			}
		}

		if kind == kindSubnet {
			clusters, err := storage.Records[record](tx, scope.Prefix(serviceName, kindCluster))
			if err != nil {
				return err
			}

			for _, r := range clusters {
				if common.Value(r.Cluster.DBSubnetGroup) == group.Name {
					return awserror.New("InvalidDBSubnetGroupStateFault", "Group is in use by a cluster")
				}
			}
		}

		tx.Delete(scope.Key(serviceName, kind, group.Name))
		return nil
	})
}

func (s *Service) DeleteDBSubnetGroup(
	ctx context.Context,
	in *api.DeleteDBSubnetGroupMessage,
) (*api.DeleteDBSubnetGroupOutput, error) {
	err := s.deleteGroup(ctx, kindSubnet, common.Value(in.DBSubnetGroupName))
	return &api.DeleteDBSubnetGroupOutput{}, err
}

func (s *Service) DeleteDBParameterGroup(
	ctx context.Context,
	in *api.DeleteDBParameterGroupMessage,
) (*api.DeleteDBParameterGroupOutput, error) {
	err := s.deleteGroup(ctx, kindParameter, common.Value(in.DBParameterGroupName))
	return &api.DeleteDBParameterGroupOutput{}, err
}
