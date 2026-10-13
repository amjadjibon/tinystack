package memorydb

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxSubnets is the documented quota of subnets in one subnet group.
const maxSubnets = 20

type subnet struct {
	ID               string
	AvailabilityZone string
}

type subnetGroupRecord struct {
	tagSet
	Name        string
	Description string
	VpcID       string
	Subnets     []subnet
}

func (r *subnetGroupRecord) output(scope request.Scope) *api.SubnetGroup {
	out := &api.SubnetGroup{
		Name:        new(r.Name),
		Description: optional(r.Description),
		VpcId:       new(r.VpcID),
		ARN:         new(arn(scope, kindSubnetGroup, r.Name)),
		Subnets:     []api.Subnet{},
	}
	for _, sn := range r.Subnets {
		out.Subnets = append(out.Subnets, api.Subnet{
			Identifier:       new(sn.ID),
			AvailabilityZone: &api.AvailabilityZone{Name: new(sn.AvailabilityZone)},
		})
	}

	return out
}

func subnetGroupNotFound(name string) error {
	return fault("SubnetGroupNotFoundFault", "Subnet group %s not found.", name)
}

func checkSubnetGroupName(name string) error {
	valid := name != "" && name[0] >= 'a' && name[0] <= 'z'
	for _, c := range name {
		valid = valid && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-')
	}

	if !valid {
		return invalidValue("SubnetGroupName must begin with a letter and contain only letters, digits and hyphens.")
	}

	return nil
}

// resolveSubnets reads the EC2 subnets of a subnet group in the caller's transaction and returns their VPC.
func (s *Service) resolveSubnets(tx storage.ReadTx, scope request.Scope, ids []string) (string, []subnet, error) {
	if len(ids) == 0 {
		return "", nil, invalidValue("SubnetIds is required.")
	}

	if len(ids) > maxSubnets {
		return "", nil, fault("SubnetQuotaExceededFault", "A subnet group can have at most %d subnets.", maxSubnets)
	}

	vpc := ""
	subnets := make([]subnet, 0, len(ids))
	for _, id := range ids {
		if slices.ContainsFunc(subnets, func(sn subnet) bool { return sn.ID == id }) {
			return "", nil, fault("InvalidSubnet", "SubnetIds must be unique.")
		}

		found, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return "", nil, fault("InvalidSubnet", "Subnet %s does not exist in this account and Region.", id)
		}

		current := common.Value(found.VpcId)
		if vpc != "" && vpc != current {
			return "", nil, fault("InvalidSubnet", "All subnets must belong to the same VPC.")
		}

		vpc = current

		subnets = append(subnets, subnet{ID: id, AvailabilityZone: common.Value(found.AvailabilityZone)})
	}

	return vpc, subnets, nil
}

func (s *Service) CreateSubnetGroup(
	ctx context.Context,
	in *api.CreateSubnetGroupRequest,
) (*api.CreateSubnetGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.SubnetGroupName)
	err = checkSubnetGroupName(name)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &subnetGroupRecord{Name: name, Description: common.Value(in.Description), tagSet: tagSet{Tags: tags}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key(scope, kindSubnetGroup, name)); exists {
			return fault("SubnetGroupAlreadyExistsFault", "Subnet group %s already exists.", name)
		}

		var e error
		if r.VpcID, r.Subnets, e = s.resolveSubnets(tx, scope, in.SubnetIds); e != nil {
			return e
		}

		return storage.Write(tx, key(scope, kindSubnetGroup, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateSubnetGroupResponse{SubnetGroup: r.output(scope)}, nil
}

func (s *Service) UpdateSubnetGroup(
	ctx context.Context,
	in *api.UpdateSubnetGroupRequest,
) (*api.UpdateSubnetGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.SubnetGroupName)
	var out *subnetGroupRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[subnetGroupRecord](tx, key(scope, kindSubnetGroup, name))
		if e != nil {
			return e
		}

		if !found {
			return subnetGroupNotFound(name)
		}

		if in.Description != nil {
			r.Description = *in.Description
		}

		if in.SubnetIds != nil {
			vpc, subnets, e := s.resolveSubnets(tx, scope, in.SubnetIds)
			if e != nil {
				return e
			}

			if vpc != r.VpcID {
				used, e := subnetGroupInUse(tx, scope, name)
				if e != nil {
					return e
				}

				if used {
					return fault("SubnetInUse", "Subnet group %s is in use and cannot move to another VPC.", name)
				}
			}

			r.VpcID, r.Subnets = vpc, subnets
		}

		out = r
		return storage.Write(tx, key(scope, kindSubnetGroup, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateSubnetGroupResponse{SubnetGroup: out.output(scope)}, nil
}

func subnetGroupInUse(tx storage.ReadTx, scope request.Scope, name string) (bool, error) {
	clusters, err := storage.Records[clusterRecord](tx, scope.Prefix(serviceName, kindCluster))
	if err != nil {
		return false, err
	}

	return slices.ContainsFunc(clusters, func(c *clusterRecord) bool { return c.SubnetGroupName == name }), nil
}

func (s *Service) DeleteSubnetGroup(
	ctx context.Context,
	in *api.DeleteSubnetGroupRequest,
) (*api.DeleteSubnetGroupResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := normalize(in.SubnetGroupName)
	var out *subnetGroupRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[subnetGroupRecord](tx, key(scope, kindSubnetGroup, name))
		if e != nil {
			return e
		}

		if !found {
			return subnetGroupNotFound(name)
		}

		used, e := subnetGroupInUse(tx, scope, name)
		if e != nil {
			return e
		}

		if used {
			return fault("SubnetGroupInUseFault", "Subnet group %s is used by a cluster.", name)
		}

		out = r
		tx.Delete(key(scope, kindSubnetGroup, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSubnetGroupResponse{SubnetGroup: out.output(scope)}, nil
}

func (s *Service) DescribeSubnetGroups(
	ctx context.Context,
	in *api.DescribeSubnetGroupsRequest,
) (*api.DescribeSubnetGroupsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var items []*subnetGroupRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.SubnetGroupName == nil {
			var e error
			items, e = storage.Records[subnetGroupRecord](tx, scope.Prefix(serviceName, kindSubnetGroup))
			return e
		}

		name := normalize(in.SubnetGroupName)
		r, found, e := storage.Read[subnetGroupRecord](tx, key(scope, kindSubnetGroup, name))
		if e != nil {
			return e
		}

		if !found {
			return subnetGroupNotFound(name)
		}

		items = []*subnetGroupRecord{r}
		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items, func(r *subnetGroupRecord) string { return r.Name }, in.NextToken, limit, invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeSubnetGroupsResponse{NextToken: next, SubnetGroups: []api.SubnetGroup{}}
	for _, r := range page {
		out.SubnetGroups = append(out.SubnetGroups, *r.output(scope))
	}

	return out, nil
}
