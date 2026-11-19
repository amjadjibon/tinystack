package rds

import (
	"context"
	"slices"
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

	subnets, vpc, err := s.validateSubnets(ctx, in.SubnetIds)
	if err != nil {
		return nil, err
	}

	group := &api.DBSubnetGroup{
		DBSubnetGroupName: new(name), DBSubnetGroupDescription: in.DBSubnetGroupDescription,
		DBSubnetGroupArn: new(arn(scope, kindSubnet, name)), SubnetGroupStatus: new("Complete"),
		VpcId: new(vpc), Subnets: subnets,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.insert(ctx, scope, &record{Name: name, Kind: kindSubnet, Subnet: group, Tags: tags}); err != nil {
		return nil, err
	}

	return &api.CreateDBSubnetGroupResult{DBSubnetGroup: group}, nil
}

func (s *Service) validateSubnets(ctx context.Context, subnetIDs []string) ([]api.Subnet, string, error) {
	if s.caller == nil {
		return nil, "", awserror.New("InvalidSubnet", "EC2 subnet validation is unavailable")
	}

	seen := make(map[string]bool, len(subnetIDs))
	ids := make([]any, len(subnetIDs))
	for i, id := range subnetIDs {
		if seen[id] {
			return nil, "", awserror.New("InvalidSubnet", "SubnetIds must be unique")
		}

		seen[id] = true
		ids[i] = id
	}

	out, err := s.caller.Call(ctx, "ec2", "DescribeSubnets", map[string]any{"SubnetIds": ids})
	if err != nil {
		return nil, "", awserror.New("InvalidSubnet", err.Error())
	}

	subnets, _ := out["Subnets"].([]any)
	var result []api.Subnet
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
			return nil, "", awserror.New("InvalidSubnet", "Subnets must share a VPC")
		}

		vpc = currentVpc
		result = append(result, api.Subnet{
			SubnetIdentifier: new(subnetID), SubnetStatus: new("Active"),
			SubnetAvailabilityZone: &api.AvailabilityZone{Name: new(zone)},
		})
	}

	if len(result) != len(subnetIDs) {
		return nil, "", awserror.New("InvalidSubnet", "Subnets must exist and be unique")
	}

	return result, vpc, nil
}

func (s *Service) ModifyDBSubnetGroup(
	ctx context.Context,
	in *api.ModifyDBSubnetGroupMessage,
) (*api.ModifyDBSubnetGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.DBSubnetGroupName))
	if name == "default" {
		return nil, awserror.New("InvalidDBSubnetGroupStateFault", "The default subnet group cannot be modified")
	}

	if in.DBSubnetGroupDescription != nil && *in.DBSubnetGroupDescription == "" {
		return nil, invalid("DBSubnetGroupDescription must not be empty")
	}

	if len(in.SubnetIds) == 0 {
		return nil, awserror.New("InvalidSubnet", "At least one subnet is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	subnets, vpc, err := s.validateSubnets(ctx, in.SubnetIds)
	if err != nil {
		return nil, err
	}

	var group *api.DBSubnetGroup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindSubnet, name)
		if readErr != nil {
			return readErr
		}

		group = r.Subnet
		if common.Value(group.VpcId) != vpc {
			return awserror.New("InvalidSubnet", "Subnets must belong to the group's existing VPC")
		}

		removed := removedSubnet(group.Subnets, subnets)
		group.Subnets = subnets
		group.DBSubnetGroupDescription = common.OrElse(in.DBSubnetGroupDescription, group.DBSubnetGroupDescription)
		if updateErr := updateSubnetInstances(tx, scope, group, removed); updateErr != nil {
			return updateErr
		}

		if removed {
			if checkErr := checkClusterSubnetRemoval(tx, scope, name); checkErr != nil {
				return checkErr
			}
		}

		return writeRecord(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.ModifyDBSubnetGroupResult{DBSubnetGroup: group}, nil
}

func removedSubnet(before, after []api.Subnet) bool {
	ids := make(map[string]bool, len(after))
	for _, subnet := range after {
		ids[common.Value(subnet.SubnetIdentifier)] = true
	}

	return slices.ContainsFunc(before, func(subnet api.Subnet) bool {
		return !ids[common.Value(subnet.SubnetIdentifier)]
	})
}

// TinyStack tracks group membership without assigning databases to individual subnets.
func updateSubnetInstances(tx storage.WriteTx, scope request.Scope, group *api.DBSubnetGroup, removed bool) error {
	instances, err := storage.Records[record](tx, scope.Prefix(serviceName, kindDB))
	if err != nil {
		return err
	}

	for _, instance := range instances {
		current := instance.Instance.DBSubnetGroup
		if current == nil || common.Value(current.DBSubnetGroupName) != common.Value(group.DBSubnetGroupName) {
			continue
		}

		if removed {
			return awserror.New("SubnetAlreadyInUse", "A DB instance uses the subnet group")
		}

		instance.Instance.DBSubnetGroup = group
		if err := writeRecord(tx, scope, instance); err != nil {
			return err
		}
	}

	return nil
}

func checkClusterSubnetRemoval(tx storage.ReadTx, scope request.Scope, name string) error {
	clusters, err := storage.Records[record](tx, scope.Prefix(serviceName, kindCluster))
	if err != nil {
		return err
	}

	for _, r := range clusters {
		if common.Value(r.Cluster.DBSubnetGroup) == name {
			return awserror.New("SubnetAlreadyInUse", "A DB cluster uses the subnet group")
		}
	}

	return nil
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
	if _, err = parameterCatalog(family, false); err != nil {
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
	if strings.HasPrefix(strings.ToLower(name), "default.") {
		return invalid("Default parameter groups are immutable")
	}

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

		if kind == kindSubnet || kind == kindClusterParameter {
			clusters, err := storage.Records[record](tx, scope.Prefix(serviceName, kindCluster))
			if err != nil {
				return err
			}

			for _, r := range clusters {
				if kind == kindClusterParameter && common.Value(r.Cluster.DBClusterParameterGroup) == group.Name {
					return awserror.New("InvalidDBParameterGroupStateFault", "Group is in use by a cluster")
				}

				if kind == kindSubnet && common.Value(r.Cluster.DBSubnetGroup) == group.Name {
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
