package shield

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func groupRules(tx storage.ReadTx, scope request.Scope, g *api.ProtectionGroup) error {
	pattern := common.Value(g.Pattern)
	if (pattern == "BY_RESOURCE_TYPE") != (g.ResourceType != nil) {
		return invalid("ResourceType is required only for BY_RESOURCE_TYPE")
	}

	if (pattern == patternArbitrary) != (g.Members != nil) {
		return invalid("Members is required only for ARBITRARY")
	}

	if pattern != patternArbitrary {
		return nil
	}

	protections, err := storage.Records[record](tx, scope.Prefix(serviceName, kindProtection))
	if err != nil {
		return err
	}

	for _, member := range g.Members {
		found := false
		for _, p := range protections {
			if common.Value(p.Protection.ResourceArn) == member {
				found = true
				break
			}
		}

		if !found {
			return missing()
		}
	}

	return nil
}

func groupMembers(tx storage.ReadTx, scope request.Scope, r *record) error {
	if common.Value(r.Group.Pattern) == patternArbitrary {
		return nil
	}

	protections, err := storage.Records[record](tx, scope.Prefix(serviceName, kindProtection))
	if err != nil {
		return err
	}

	r.Group.Members = []string{}
	for _, p := range protections {
		if common.Value(r.Group.Pattern) == "ALL" || p.ResourceType == common.Value(r.Group.ResourceType) {
			r.Group.Members = append(r.Group.Members, common.Value(p.Protection.ResourceArn))
		}
	}

	return nil
}

func (s *Service) CreateProtectionGroup(
	ctx context.Context,
	in *api.CreateProtectionGroupRequest,
) (*api.CreateProtectionGroupResponse, error) {
	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	operationErr := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		key := scope.Key(serviceName, kindGroup, common.Value(in.ProtectionGroupId))
		if _, ok, err := storage.Read[record](tx, key); err != nil {
			return err
		} else if ok {
			return already()
		}

		g := &api.ProtectionGroup{
			ProtectionGroupId:  in.ProtectionGroupId,
			Aggregation:        in.Aggregation,
			Pattern:            in.Pattern,
			ResourceType:       in.ResourceType,
			Members:            in.Members,
			ProtectionGroupArn: new(arn(scope, kindGroup, common.Value(in.ProtectionGroupId))),
		}

		if err := groupRules(tx, scope, g); err != nil {
			return err
		}

		if len(tx.Keys(scope.Prefix(serviceName, kindGroup))) >= 100 {
			return awserror.New("LimitsExceededException", "Protection group limit exceeded")
		}

		return storage.Write(tx, key, record{Group: g, Tags: tags})
	})
	if operationErr != nil {
		return nil, operationErr
	}

	return &api.CreateProtectionGroupResponse{}, nil
}

func (s *Service) UpdateProtectionGroup(
	ctx context.Context,
	in *api.UpdateProtectionGroupRequest,
) (*api.UpdateProtectionGroupResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		r, err := read(tx, scope, kindGroup, common.Value(in.ProtectionGroupId))
		if err != nil {
			return err
		}

		r.Group.Aggregation = in.Aggregation
		r.Group.Pattern = in.Pattern
		r.Group.ResourceType = in.ResourceType
		r.Group.Members = in.Members
		if err := groupRules(tx, scope, r.Group); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key(serviceName, kindGroup, common.Value(in.ProtectionGroupId)), r)
	})
	return &api.UpdateProtectionGroupResponse{}, err
}

func (s *Service) DescribeProtectionGroup(
	ctx context.Context,
	in *api.DescribeProtectionGroupRequest,
) (*api.DescribeProtectionGroupResponse, error) {
	out := &api.DescribeProtectionGroupResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		r, err := read(tx, scope, kindGroup, common.Value(in.ProtectionGroupId))
		if err != nil {
			return err
		}

		if memberErr := groupMembers(tx, scope, r); memberErr != nil {
			return memberErr
		}

		out.ProtectionGroup = r.Group
		return nil
	})
	return out, err
}

func (s *Service) DeleteProtectionGroup(
	ctx context.Context,
	in *api.DeleteProtectionGroupRequest,
) (*api.DeleteProtectionGroupResponse, error) {
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		if _, err := read(tx, scope, kindGroup, common.Value(in.ProtectionGroupId)); err != nil {
			return err
		}

		tx.Delete(scope.Key(serviceName, kindGroup, common.Value(in.ProtectionGroupId)))
		return nil
	})
	return &api.DeleteProtectionGroupResponse{}, err
}

func (s *Service) ListProtectionGroups(
	ctx context.Context,
	in *api.ListProtectionGroupsRequest,
) (*api.ListProtectionGroupsResponse, error) {
	out := &api.ListProtectionGroupsResponse{ProtectionGroups: []api.ProtectionGroup{}}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		records, err := storage.Records[record](tx, scope.Prefix(serviceName, kindGroup))
		if err != nil {
			return err
		}

		items := []api.ProtectionGroup{}
		for _, r := range records {
			g, f := r.Group, in.InclusionFilters
			if f != nil &&
				(!matches(f.ProtectionGroupIds, g.ProtectionGroupId) ||
					!matches(f.Patterns, g.Pattern) ||
					!matches(f.ResourceTypes, g.ResourceType) ||
					!matches(f.Aggregations, g.Aggregation)) {
				continue
			}

			if memberErr := groupMembers(tx, scope, r); memberErr != nil {
				return memberErr
			}

			items = append(items, *g)
		}

		out.ProtectionGroups, out.NextToken, err = common.Page(
			items,
			func(g api.ProtectionGroup) string { return common.Value(g.ProtectionGroupId) },
			in.NextToken,
			pageSize(in.MaxResults),
			invalid("Invalid NextToken"),
		)
		return err
	})
	return out, err
}

func (s *Service) ListResourcesInProtectionGroup(
	ctx context.Context,
	in *api.ListResourcesInProtectionGroupRequest,
) (*api.ListResourcesInProtectionGroupResponse, error) {
	out := &api.ListResourcesInProtectionGroupResponse{}
	err := s.transaction(ctx, func(tx storage.WriteTx, scope request.Scope, _ *subscriptionRecord) error {
		r, err := read(tx, scope, kindGroup, common.Value(in.ProtectionGroupId))
		if err != nil {
			return err
		}

		if memberErr := groupMembers(tx, scope, r); memberErr != nil {
			return memberErr
		}

		// Arbitrary input is a set; enumerate it in protection storage order too.
		protections, err := storage.Records[record](tx, scope.Prefix(serviceName, kindProtection))
		if err != nil {
			return err
		}

		members := []string{}
		for _, p := range protections {
			value := common.Value(p.Protection.ResourceArn)
			if slices.Contains(r.Group.Members, value) {
				members = append(members, value)
			}
		}

		out.ResourceArns, out.NextToken, err = common.Page(
			members,
			func(v string) string { return v },
			in.NextToken,
			pageSize(in.MaxResults),
			invalid("Invalid NextToken"),
		)
		return err
	})
	return out, err
}
