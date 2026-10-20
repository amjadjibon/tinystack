package opensearchserverless

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateSecurityPolicy(
	ctx context.Context,
	in *api.CreateSecurityPolicyRequest,
) (*api.CreateSecurityPolicyResponse, error) {
	r, err := s.writePolicy(
		ctx,
		"security",
		in.Type,
		in.Name,
		in.Description,
		in.Policy,
		nil,
		in.ClientToken,
		in,
		false,
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateSecurityPolicyResponse{SecurityPolicyDetail: securityPolicyDetail(r)}, nil
}

func (s *Service) UpdateSecurityPolicy(
	ctx context.Context,
	in *api.UpdateSecurityPolicyRequest,
) (*api.UpdateSecurityPolicyResponse, error) {
	r, err := s.writePolicy(
		ctx,
		"security",
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

	return &api.UpdateSecurityPolicyResponse{SecurityPolicyDetail: securityPolicyDetail(r)}, nil
}

func (s *Service) GetSecurityPolicy(
	ctx context.Context,
	in *api.GetSecurityPolicyRequest,
) (*api.GetSecurityPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *policy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readPolicy(tx, scope, "security", in.Type, in.Name)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.GetSecurityPolicyResponse{SecurityPolicyDetail: securityPolicyDetail(r)}, nil
}

func (s *Service) DeleteSecurityPolicy(
	ctx context.Context,
	in *api.DeleteSecurityPolicyRequest,
) (*api.DeleteSecurityPolicyResponse, error) {
	return transact(
		ctx,
		s,
		"DeleteSecurityPolicy",
		in.ClientToken,
		in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteSecurityPolicyResponse, error) {
			if _, err := readPolicy(tx, scope, "security", in.Type, in.Name); err != nil {
				return nil, err
			}

			tx.Delete(policyKey(scope, "security", common.Value(in.Type), common.Value(in.Name)))
			return &api.DeleteSecurityPolicyResponse{}, nil
		},
	)
}

func (s *Service) ListSecurityPolicies(
	ctx context.Context,
	in *api.ListSecurityPoliciesRequest,
) (*api.ListSecurityPoliciesResponse, error) {
	if err := policyType("security", in.Type); err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	rows := []api.SecurityPolicySummary{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[policy](tx, scope.Prefix(serviceName, "security")+common.Value(in.Type)+"/")
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
				rows = append(rows, *securityPolicySummary(r))
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
		func(r api.SecurityPolicySummary) string { return common.Value(r.Name) },
		in.NextToken,
		limit,
		awserror.Validation("Invalid nextToken."),
	)
	if err != nil {
		return nil, err
	}

	return &api.ListSecurityPoliciesResponse{SecurityPolicySummaries: page, NextToken: next}, nil
}
