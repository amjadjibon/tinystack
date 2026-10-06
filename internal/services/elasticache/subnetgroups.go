package elasticache

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type subnetRecord struct {
	SubnetIdentifier       string
	SubnetAvailabilityZone string
}

type subnetGroupRecord struct {
	Name        string
	Description string
	VpcID       string
	Tags        map[string]string
	Subnets     []subnetRecord
}

func (s *Service) validateSubnets(
	ctx context.Context,
	scope request.Scope,
	subnetIDs []string,
) (string, []subnetRecord, error) {
	if len(subnetIDs) == 0 {
		return "", nil, invalidParameterValue("At least one subnet must be specified.")
	}

	if s.caller == nil {
		records := make([]subnetRecord, len(subnetIDs))
		for i, id := range subnetIDs {
			records[i] = subnetRecord{
				SubnetIdentifier:       id,
				SubnetAvailabilityZone: scope.Region + "a",
			}
		}

		return defaultVpcID, records, nil
	}

	out, err := s.call(ctx, "ec2", "DescribeSubnets", map[string]any{
		"SubnetIds": anyList(subnetIDs),
	})
	if err != nil {
		return "", nil, invalidSubnet("Some of the subnets are not found or invalid: %s", err.Error())
	}

	var (
		vpcID   string
		records []subnetRecord
	)

	for _, sn := range objects(out, "Subnets") {
		curVpc := text(sn, "VpcId")
		if vpcID != "" && curVpc != vpcID {
			return "", nil, invalidSubnet("All subnets must belong to the same VPC.")
		}

		vpcID = curVpc
		records = append(records, subnetRecord{
			SubnetIdentifier:       text(sn, "SubnetId"),
			SubnetAvailabilityZone: text(sn, "AvailabilityZone"),
		})
	}

	if len(records) < len(subnetIDs) {
		return "", nil, invalidSubnet("One or more subnets could not be found.")
	}

	return vpcID, records, nil
}

func (s *Service) getDefaultSubnetGroup(ctx context.Context, scope request.Scope) *api.CacheSubnetGroup {
	desc := "Default Subnet Group"
	defaultVpc := defaultVpcID
	subnets := []subnetRecord{
		{
			SubnetIdentifier:       "subnet-default",
			SubnetAvailabilityZone: scope.Region + "a",
		},
	}

	if s.caller != nil {
		out, err := s.call(ctx, "ec2", "DescribeSubnets", map[string]any{})
		if err == nil {
			sns := objects(out, "Subnets")
			if len(sns) > 0 {
				defaultVpc = text(sns[0], "VpcId")
				subnets = nil
				for _, sn := range sns {
					if text(sn, "VpcId") == defaultVpc {
						subnets = append(subnets, subnetRecord{
							SubnetIdentifier:       text(sn, "SubnetId"),
							SubnetAvailabilityZone: text(sn, "AvailabilityZone"),
						})
					}
				}
			}
		}
	}

	return toAPISubnetGroup(scope, &subnetGroupRecord{
		Name:        "default",
		Description: desc,
		VpcID:       defaultVpc,
		Subnets:     subnets,
	})
}

func toAPISubnetGroup(scope request.Scope, r *subnetGroupRecord) *api.CacheSubnetGroup {
	var subnets []api.Subnet
	for _, sn := range r.Subnets {
		subnets = append(subnets, api.Subnet{
			SubnetIdentifier: new(sn.SubnetIdentifier),
			SubnetAvailabilityZone: &api.AvailabilityZone{
				Name: new(sn.SubnetAvailabilityZone),
			},
		})
	}

	arn := subnetGroupARN(scope, r.Name)
	return &api.CacheSubnetGroup{
		CacheSubnetGroupName:        new(r.Name),
		CacheSubnetGroupDescription: new(r.Description),
		VpcId:                       new(r.VpcID),
		Subnets:                     subnets,
		ARN:                         new(arn),
		SupportedNetworkTypes:       []string{"ipv4"},
	}
}

func (s *Service) CreateCacheSubnetGroup(
	ctx context.Context,
	in *api.CreateCacheSubnetGroupMessage,
) (*api.CreateCacheSubnetGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheSubnetGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheSubnetGroupName is required.")
	}

	if strings.EqualFold(name, "default") {
		return nil, subnetGroupAlreadyExists(name)
	}

	desc := common.Value(in.CacheSubnetGroupDescription)
	if desc == "" {
		return nil, invalidParameterValue("CacheSubnetGroupDescription is required.")
	}

	tags, tagErr := tagging.Map(in.Tags, func(t api.Tag) (k, v *string) {
		return t.Key, t.Value
	}, invalidParameterValue("Tag key and value are required"), nil)
	if tagErr != nil {
		return nil, tagErr
	}

	if err = tagging.CheckSet(tags); err != nil {
		return nil, invalidParameterValue("%s", err.Error())
	}

	vpcID, subnets, err := s.validateSubnets(ctx, scope, in.SubnetIds)
	if err != nil {
		return nil, err
	}

	key := subnetGroupKey(scope, name)
	record := subnetGroupRecord{
		Name:        name,
		Description: desc,
		VpcID:       vpcID,
		Subnets:     subnets,
		Tags:        tags,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found, readErr := storage.Read[subnetGroupRecord](tx, key); readErr != nil {
			return readErr
		} else if found {
			return subnetGroupAlreadyExists(name)
		}

		return storage.Write(tx, key, record)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, sourceTypeCacheSubnetGroup, "Cache subnet group "+name+" created")

	return &api.CreateCacheSubnetGroupResult{
		CacheSubnetGroup: toAPISubnetGroup(scope, &record),
	}, nil
}

func (s *Service) DescribeCacheSubnetGroups(
	ctx context.Context,
	in *api.DescribeCacheSubnetGroupsMessage,
) (*api.CacheSubnetGroupMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	targetName := common.Value(in.CacheSubnetGroupName)
	if targetName != "" {
		if strings.EqualFold(targetName, "default") {
			return &api.CacheSubnetGroupMessage{
				CacheSubnetGroups: []api.CacheSubnetGroup{*s.getDefaultSubnetGroup(ctx, scope)},
			}, nil
		}

		var group api.CacheSubnetGroup
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			r, found, readErr := storage.Read[subnetGroupRecord](tx, subnetGroupKey(scope, targetName))
			if readErr != nil {
				return readErr
			}

			if !found {
				return subnetGroupNotFound(targetName)
			}

			group = *toAPISubnetGroup(scope, r)
			return nil
		})
		if err != nil {
			return nil, err
		}

		return &api.CacheSubnetGroupMessage{
			CacheSubnetGroups: []api.CacheSubnetGroup{group},
		}, nil
	}

	var groups []api.CacheSubnetGroup
	// Implicit default group is always included
	groups = append(groups, *s.getDefaultSubnetGroup(ctx, scope))

	prefix := scope.Prefix(serviceName, "subnet-group")
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[subnetGroupRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, r := range records {
			groups = append(groups, *toAPISubnetGroup(scope, r))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CacheSubnetGroupMessage{
		CacheSubnetGroups: groups,
	}, nil
}

func (s *Service) ModifyCacheSubnetGroup(
	ctx context.Context,
	in *api.ModifyCacheSubnetGroupMessage,
) (*api.ModifyCacheSubnetGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheSubnetGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheSubnetGroupName is required.")
	}

	if strings.EqualFold(name, "default") {
		return nil, invalidParameterValue("The default subnet group cannot be modified.")
	}

	key := subnetGroupKey(scope, name)
	var (
		existing *subnetGroupRecord
		found    bool
	)

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		existing, found, err = storage.Read[subnetGroupRecord](tx, key)
		if err != nil {
			return err
		}

		if !found {
			return subnetGroupNotFound(name)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if in.CacheSubnetGroupDescription != nil {
		existing.Description = common.Value(in.CacheSubnetGroupDescription)
	}

	if len(in.SubnetIds) > 0 {
		vpcID, subnets, valErr := s.validateSubnets(ctx, scope, in.SubnetIds)
		if valErr != nil {
			return nil, valErr
		}

		existing.VpcID = vpcID
		existing.Subnets = subnets
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key, existing)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, sourceTypeCacheSubnetGroup, "Cache subnet group "+name+" modified")

	return &api.ModifyCacheSubnetGroupResult{
		CacheSubnetGroup: toAPISubnetGroup(scope, existing),
	}, nil
}

func (s *Service) DeleteCacheSubnetGroup(
	ctx context.Context,
	in *api.DeleteCacheSubnetGroupMessage,
) (*api.DeleteCacheSubnetGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheSubnetGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheSubnetGroupName is required.")
	}

	if strings.EqualFold(name, "default") {
		return nil, invalidParameterValue("The default subnet group cannot be deleted.")
	}

	key := subnetGroupKey(scope, name)

	// Check if any cluster in scope uses this subnet group
	clusterPrefix := scope.Prefix(serviceName, "cluster")
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[subnetGroupRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return subnetGroupNotFound(name)
		}

		clusters, cErr := storage.Records[clusterRecord](tx, clusterPrefix)
		if cErr != nil {
			return cErr
		}

		for _, c := range clusters {
			if strings.EqualFold(c.SubnetGroupName, name) {
				return subnetGroupInUse(name)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, sourceTypeCacheSubnetGroup, "Cache subnet group "+name+" deleted")

	return &api.DeleteCacheSubnetGroupOutput{}, nil
}
