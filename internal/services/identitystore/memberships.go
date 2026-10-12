package identitystore

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/identitystore"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func memberUser(member *api.MemberId) (*string, error) {
	if member == nil {
		return nil, awserror.Validation("MemberId.UserId is required")
	}

	id, err := resourceID(member.UserId, "user")
	return new(id), err
}

func membershipReferences(tx storage.ReadTx, scope request.Scope, groupID, userID *string) error {
	if _, _, err := load[api.Group](tx, scope, "group", groupID); err != nil {
		return err
	}

	_, _, err := load[api.User](tx, scope, "user", userID)
	return err
}

func findMembership(tx storage.ReadTx, scope request.Scope, groupID, userID string) (*api.GroupMembership, error) {
	records, err := storage.Records[api.GroupMembership](tx, scope.Prefix(serviceName, "membership"))
	if err != nil {
		return nil, err
	}

	for _, r := range records {
		if *r.GroupId == groupID && *r.MemberId.UserId == userID {
			return r, nil
		}
	}

	return nil, missing()
}

func (s *Service) CreateGroupMembership(ctx context.Context, in *api.CreateGroupMembershipRequest) (
	*api.CreateGroupMembershipResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	groupID, err := resourceID(in.GroupId, "group")
	if err != nil {
		return nil, err
	}

	userID, err := memberUser(in.MemberId)
	if err != nil {
		return nil, err
	}

	r := &api.GroupMembership{
		IdentityStoreId: new(*in.IdentityStoreId), MembershipId: new(newID()),
		GroupId: new(groupID), MemberId: &api.MemberId{UserId: userID},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := membershipReferences(tx, scope, r.GroupId, userID); e != nil {
			return e
		}

		records, e := storage.Records[api.GroupMembership](tx, scope.Prefix(serviceName, "membership"))
		if e != nil {
			return e
		}

		for _, other := range records {
			if *other.GroupId == groupID && *other.MemberId.UserId == *userID {
				return conflict()
			}
		}

		return storage.Write(tx, scope.Key(serviceName, "membership", *r.MembershipId), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateGroupMembershipResponse{IdentityStoreId: r.IdentityStoreId, MembershipId: r.MembershipId}, nil
}

func (s *Service) DeleteGroupMembership(ctx context.Context, in *api.DeleteGroupMembershipRequest) (
	*api.DeleteGroupMembershipResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, key, e := load[api.GroupMembership](tx, scope, "membership", in.MembershipId)
		if e != nil {
			return e
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteGroupMembershipResponse{}, nil
}

func (s *Service) GetGroupMembershipId(ctx context.Context, in *api.GetGroupMembershipIdRequest) (
	*api.GetGroupMembershipIdResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	groupID, err := resourceID(in.GroupId, "group")
	if err != nil {
		return nil, err
	}

	userID, err := memberUser(in.MemberId)
	if err != nil {
		return nil, err
	}

	var out *api.GetGroupMembershipIdResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := findMembership(tx, scope, groupID, *userID)
		if e != nil {
			return e
		}

		out = &api.GetGroupMembershipIdResponse{IdentityStoreId: r.IdentityStoreId, MembershipId: r.MembershipId}
		return nil
	})
	return out, err
}

func (s *Service) memberships(
	ctx context.Context,
	scope request.Scope,
	groupID, userID *string,
	maximum *int64,
	token *string,
) (
	[]api.GroupMembership, *string, error,
) {
	limit, err := pageLimit(maximum)
	if err != nil {
		return nil, nil, err
	}

	list := []api.GroupMembership{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if groupID != nil {
			if _, _, e := load[api.Group](tx, scope, "group", groupID); e != nil {
				return e
			}
		}

		if userID != nil {
			if _, _, e := load[api.User](tx, scope, "user", userID); e != nil {
				return e
			}
		}

		records, e := storage.Records[api.GroupMembership](tx, scope.Prefix(serviceName, "membership"))
		if e != nil {
			return e
		}

		for _, r := range records {
			if (groupID == nil || *r.GroupId == *groupID) && (userID == nil || *r.MemberId.UserId == *userID) {
				list = append(list, *r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(list, func(r api.GroupMembership) string { return *r.MembershipId }, token, limit,
		awserror.Validation("Invalid NextToken"))
}

func (s *Service) ListGroupMemberships(ctx context.Context, in *api.ListGroupMembershipsRequest) (
	*api.ListGroupMembershipsResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	id, err := resourceID(in.GroupId, "group")
	if err != nil {
		return nil, err
	}

	list, next, err := s.memberships(ctx, scope, new(id), nil, in.MaxResults, in.NextToken)
	return &api.ListGroupMembershipsResponse{GroupMemberships: list, NextToken: next}, err
}

func (s *Service) ListGroupMembershipsForMember(ctx context.Context, in *api.ListGroupMembershipsForMemberRequest) (
	*api.ListGroupMembershipsForMemberResponse, error,
) {
	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	id, err := memberUser(in.MemberId)
	if err != nil {
		return nil, err
	}

	list, next, err := s.memberships(ctx, scope, nil, id, in.MaxResults, in.NextToken)
	return &api.ListGroupMembershipsForMemberResponse{GroupMemberships: list, NextToken: next}, err
}

func (s *Service) IsMemberInGroups(ctx context.Context, in *api.IsMemberInGroupsRequest) (
	*api.IsMemberInGroupsResponse, error,
) {
	if in == nil || len(in.GroupIds) < 1 || len(in.GroupIds) > 100 {
		return nil, awserror.Validation("Specify 1 to 100 GroupIds")
	}

	scope, err := checkScope(ctx, in.IdentityStoreId)
	if err != nil {
		return nil, err
	}

	userID, err := memberUser(in.MemberId)
	if err != nil {
		return nil, err
	}

	out := &api.IsMemberInGroupsResponse{Results: []api.GroupMembershipExistenceResult{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, _, e := load[api.User](tx, scope, "user", userID); e != nil {
			return e
		}

		records, e := storage.Records[api.GroupMembership](tx, scope.Prefix(serviceName, "membership"))
		if e != nil {
			return e
		}

		for _, value := range in.GroupIds {
			groupID, e := resourceID(new(value), "group")
			if e != nil {
				return e
			}

			if _, _, e = load[api.Group](tx, scope, "group", new(groupID)); e != nil {
				return e
			}

			exists := false
			for _, r := range records {
				if *r.GroupId == groupID && *r.MemberId.UserId == *userID {
					exists = true
					break
				}
			}

			out.Results = append(out.Results, api.GroupMembershipExistenceResult{
				GroupId: new(groupID), MemberId: &api.MemberId{UserId: userID}, MembershipExists: new(exists),
			})
		}

		return nil
	})
	return out, err
}
