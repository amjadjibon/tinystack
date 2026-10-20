package opensearchserverless

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateAccessPolicy(
	ctx context.Context,
	in *api.CreateAccessPolicyRequest,
) (*api.CreateAccessPolicyResponse, error) {
	r, err := s.writePolicy(ctx, "access", in.Type, in.Name, in.Description, in.Policy, nil, in.ClientToken, in, false)
	if err != nil {
		return nil, err
	}

	return &api.CreateAccessPolicyResponse{AccessPolicyDetail: accessPolicyDetail(r)}, nil
}

func (s *Service) UpdateAccessPolicy(
	ctx context.Context,
	in *api.UpdateAccessPolicyRequest,
) (*api.UpdateAccessPolicyResponse, error) {
	r, err := s.writePolicy(
		ctx,
		"access",
		in.Type,
		in.Name,
		in.Description,
		in.Policy,
		in.PolicyVersion,
		in.ClientToken,
		in,
		true,
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateAccessPolicyResponse{AccessPolicyDetail: accessPolicyDetail(r)}, nil
}

func (s *Service) GetAccessPolicy(
	ctx context.Context,
	in *api.GetAccessPolicyRequest,
) (*api.GetAccessPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *policy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readPolicy(tx, scope, "access", in.Type, in.Name)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.GetAccessPolicyResponse{AccessPolicyDetail: accessPolicyDetail(r)}, nil
}

func (s *Service) DeleteAccessPolicy(
	ctx context.Context,
	in *api.DeleteAccessPolicyRequest,
) (*api.DeleteAccessPolicyResponse, error) {
	return transact(
		ctx,
		s,
		"DeleteAccessPolicy",
		in.ClientToken,
		in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteAccessPolicyResponse, error) {
			if _, err := readPolicy(tx, scope, "access", in.Type, in.Name); err != nil {
				return nil, err
			}

			tx.Delete(policyKey(scope, "access", common.Value(in.Type), common.Value(in.Name)))
			return &api.DeleteAccessPolicyResponse{}, nil
		},
	)
}

func (s *Service) ListAccessPolicies(
	ctx context.Context,
	in *api.ListAccessPoliciesRequest,
) (*api.ListAccessPoliciesResponse, error) {
	if err := policyType("access", in.Type); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	rows := []api.AccessPolicySummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[policy](tx, scope.Prefix(serviceName, "access")+common.Value(in.Type)+"/")
		if e != nil {
			return e
		}

		for _, r := range records {
			matched := len(in.Resource) == 0
			for _, resource := range in.Resource {
				if matchesPolicy(r, resource) {
					matched = true
				}
			}

			if matched {
				rows = append(rows, *accessPolicySummary(r))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(r api.AccessPolicySummary) string { return common.Value(r.Name) },
		in.NextToken,
		limit,
		awserror.Validation("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListAccessPoliciesResponse{AccessPolicySummaries: page, NextToken: next}, nil
}
