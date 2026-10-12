package route53resolver

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
)

func (s *Service) GetResolverEndpoint(
	ctx context.Context,
	in *api.GetResolverEndpointRequest,
) (*api.GetResolverEndpointResponse, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	r, err := s.get(ctx, endpointKind, in.ResolverEndpointId)
	if err != nil {
		return nil, err
	}

	return &api.GetResolverEndpointResponse{ResolverEndpoint: r.Endpoint}, nil
}

func (s *Service) ListResolverEndpoints(
	ctx context.Context,
	in *api.ListResolverEndpointsRequest,
) (*api.ListResolverEndpointsResponse, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	records, next, err := s.list(ctx, endpointKind, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := make([]api.ResolverEndpoint, 0, len(records))
	for _, r := range records {
		out = append(out, *r.Endpoint)
	}

	return &api.ListResolverEndpointsResponse{ResolverEndpoints: out, NextToken: next, MaxResults: in.MaxResults}, nil
}

func (s *Service) GetResolverRule(
	ctx context.Context,
	in *api.GetResolverRuleRequest,
) (*api.GetResolverRuleResponse, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	r, err := s.get(ctx, ruleKind, in.ResolverRuleId)
	if err != nil {
		return nil, err
	}

	return &api.GetResolverRuleResponse{ResolverRule: r.Rule}, nil
}

func (s *Service) ListResolverRules(
	ctx context.Context,
	in *api.ListResolverRulesRequest,
) (*api.ListResolverRulesResponse, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	records, next, err := s.list(ctx, ruleKind, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := make([]api.ResolverRule, 0, len(records))
	for _, r := range records {
		out = append(out, *r.Rule)
	}

	return &api.ListResolverRulesResponse{ResolverRules: out, NextToken: next, MaxResults: in.MaxResults}, nil
}

func (s *Service) GetResolverRuleAssociation(
	ctx context.Context,
	in *api.GetResolverRuleAssociationRequest,
) (*api.GetResolverRuleAssociationResponse, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	r, err := s.get(ctx, associationKind, in.ResolverRuleAssociationId)
	if err != nil {
		return nil, err
	}

	return &api.GetResolverRuleAssociationResponse{ResolverRuleAssociation: r.Association}, nil
}

func (s *Service) ListResolverRuleAssociations(
	ctx context.Context,
	in *api.ListResolverRuleAssociationsRequest,
) (*api.ListResolverRuleAssociationsResponse, error) {
	if in == nil {
		return nil, invalid("Input is required")
	}

	records, next, err := s.list(ctx, associationKind, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := make([]api.ResolverRuleAssociation, 0, len(records))
	for _, r := range records {
		out = append(out, *r.Association)
	}

	return &api.ListResolverRuleAssociationsResponse{
		ResolverRuleAssociations: out,
		NextToken:                next,
		MaxResults:               in.MaxResults,
	}, nil
}
