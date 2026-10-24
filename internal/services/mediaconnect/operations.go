package mediaconnect

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconnect"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AddFlowSources(
	ctx context.Context,
	in *api.AddFlowSourcesRequest,
) (*api.AddFlowSourcesResponse, error) {
	if checkErr := s.references(ctx, in.Sources, nil, nil, nil); checkErr != nil {
		return nil, checkErr
	}

	start := 0
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		start = len(r.Flow.Sources)
		if checkErr := s.addSources(tx, scope, r, in.Sources); checkErr != nil {
			return checkErr
		}

		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddFlowSourcesResponse{FlowArn: in.FlowArn, Sources: r.Flow.Sources[start:]}, nil
}

func (s *Service) UpdateFlowSource(
	ctx context.Context,
	in *api.UpdateFlowSourceRequest,
) (*api.UpdateFlowSourceResponse, error) {
	// Validate merged encryption outside storage callbacks to avoid nested transactions.
	if in.Decryption != nil {
		current, err := s.DescribeFlow(ctx, &api.DescribeFlowRequest{FlowArn: in.FlowArn})
		if err != nil {
			return nil, err
		}

		for _, v := range current.Flow.Sources {
			if common.Value(v.SourceArn) == common.Value(in.SourceArn) {
				candidate := common.Clone(new(v))
				patchSource(candidate, in)
				if checkErr := s.encryption(ctx, candidate.Decryption); checkErr != nil {
					return nil, checkErr
				}
			}
		}
	}

	var result *api.Source
	_, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		for i := range r.Flow.Sources {
			v := &r.Flow.Sources[i]
			if common.Value(v.SourceArn) == common.Value(in.SourceArn) {
				patchSource(v, in)
				result = v
				return validateFlow(tx, scope, r)
			}
		}

		return missing()
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateFlowSourceResponse{FlowArn: in.FlowArn, Source: result}, nil
}

func (s *Service) RemoveFlowSource(
	ctx context.Context,
	in *api.RemoveFlowSourceRequest,
) (*api.RemoveFlowSourceResponse, error) {
	_, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		i := slices.IndexFunc(
			r.Flow.Sources,
			func(v api.Source) bool { return common.Value(v.SourceArn) == common.Value(in.SourceArn) },
		)
		if i < 0 {
			return missing()
		}

		if len(r.Flow.Sources) < 2 {
			return invalid("A flow must retain at least one source")
		}

		r.Flow.Sources = slices.Delete(r.Flow.Sources, i, i+1)
		delete(r.Tags, common.Value(in.SourceArn))
		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveFlowSourceResponse{FlowArn: in.FlowArn, SourceArn: in.SourceArn}, nil
}

func (s *Service) AddFlowOutputs(
	ctx context.Context,
	in *api.AddFlowOutputsRequest,
) (*api.AddFlowOutputsResponse, error) {
	if checkErr := s.references(ctx, nil, in.Outputs, nil, nil); checkErr != nil {
		return nil, checkErr
	}

	start := 0
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		start = len(r.Flow.Outputs)
		if checkErr := addOutputs(scope, r, in.Outputs); checkErr != nil {
			return checkErr
		}

		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddFlowOutputsResponse{FlowArn: in.FlowArn, Outputs: r.Flow.Outputs[start:]}, nil
}

func (s *Service) UpdateFlowOutput(
	ctx context.Context,
	in *api.UpdateFlowOutputRequest,
) (*api.UpdateFlowOutputResponse, error) {
	// Validate merged encryption outside storage callbacks to avoid nested transactions.
	if in.Encryption != nil {
		current, err := s.DescribeFlow(ctx, &api.DescribeFlowRequest{FlowArn: in.FlowArn})
		if err != nil {
			return nil, err
		}

		for i := range current.Flow.Outputs {
			v := current.Flow.Outputs[i]
			if common.Value(v.OutputArn) == common.Value(in.OutputArn) {
				candidate := common.Clone(new(v))
				patchOutput(candidate, in)
				if checkErr := s.encryption(ctx, candidate.Encryption); checkErr != nil {
					return nil, checkErr
				}
			}
		}
	}

	var result *api.Output
	_, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		for i := range r.Flow.Outputs {
			v := &r.Flow.Outputs[i]
			if common.Value(v.OutputArn) == common.Value(in.OutputArn) {
				patchOutput(v, in)
				result = v
				return validateFlow(tx, scope, r)
			}
		}

		return missing()
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateFlowOutputResponse{FlowArn: in.FlowArn, Output: result}, nil
}

func (s *Service) RemoveFlowOutput(
	ctx context.Context,
	in *api.RemoveFlowOutputRequest,
) (*api.RemoveFlowOutputResponse, error) {
	_, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		i := slices.IndexFunc(
			r.Flow.Outputs,
			func(v api.Output) bool { return common.Value(v.OutputArn) == common.Value(in.OutputArn) },
		)
		if i < 0 {
			return missing()
		}

		r.Flow.Outputs = slices.Delete(r.Flow.Outputs, i, i+1)
		delete(r.Tags, common.Value(in.OutputArn))
		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveFlowOutputResponse{FlowArn: in.FlowArn, OutputArn: in.OutputArn}, nil
}

func (s *Service) GrantFlowEntitlements(
	ctx context.Context,
	in *api.GrantFlowEntitlementsRequest,
) (*api.GrantFlowEntitlementsResponse, error) {
	if checkErr := s.references(ctx, nil, nil, in.Entitlements, nil); checkErr != nil {
		return nil, checkErr
	}

	start := 0
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		start = len(r.Flow.Entitlements)
		if checkErr := addEntitlements(scope, r, in.Entitlements); checkErr != nil {
			return checkErr
		}

		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.GrantFlowEntitlementsResponse{FlowArn: in.FlowArn, Entitlements: r.Flow.Entitlements[start:]}, nil
}

func (s *Service) UpdateFlowEntitlement(
	ctx context.Context,
	in *api.UpdateFlowEntitlementRequest,
) (*api.UpdateFlowEntitlementResponse, error) {
	// Validate merged encryption outside storage callbacks to avoid nested transactions.
	if in.Encryption != nil {
		current, err := s.DescribeFlow(ctx, &api.DescribeFlowRequest{FlowArn: in.FlowArn})
		if err != nil {
			return nil, err
		}

		for _, v := range current.Flow.Entitlements {
			if common.Value(v.EntitlementArn) == common.Value(in.EntitlementArn) {
				candidate := common.Clone(new(v))
				patchEntitlement(candidate, in)
				if checkErr := s.encryption(ctx, candidate.Encryption); checkErr != nil {
					return nil, checkErr
				}
			}
		}
	}

	var result *api.Entitlement
	_, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		for i := range r.Flow.Entitlements {
			v := &r.Flow.Entitlements[i]
			if common.Value(v.EntitlementArn) == common.Value(in.EntitlementArn) {
				patchEntitlement(v, in)
				result = v
				return validateFlow(tx, scope, r)
			}
		}

		return missing()
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateFlowEntitlementResponse{FlowArn: in.FlowArn, Entitlement: result}, nil
}

func (s *Service) RevokeFlowEntitlement(
	ctx context.Context,
	in *api.RevokeFlowEntitlementRequest,
) (*api.RevokeFlowEntitlementResponse, error) {
	_, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		i := slices.IndexFunc(
			r.Flow.Entitlements,
			func(v api.Entitlement) bool { return common.Value(v.EntitlementArn) == common.Value(in.EntitlementArn) },
		)
		if i < 0 {
			return missing()
		}

		r.Flow.Entitlements = slices.Delete(r.Flow.Entitlements, i, i+1)
		delete(r.Tags, common.Value(in.EntitlementArn))
		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.RevokeFlowEntitlementResponse{FlowArn: in.FlowArn, EntitlementArn: in.EntitlementArn}, nil
}

func (s *Service) AddFlowVpcInterfaces(
	ctx context.Context,
	in *api.AddFlowVpcInterfacesRequest,
) (*api.AddFlowVpcInterfacesResponse, error) {
	if checkErr := s.references(ctx, nil, nil, nil, in.VpcInterfaces); checkErr != nil {
		return nil, checkErr
	}

	start := 0
	r, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		start = len(r.Flow.VpcInterfaces)
		if checkErr := s.addVpcs(tx, scope, r, in.VpcInterfaces); checkErr != nil {
			return checkErr
		}

		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddFlowVpcInterfacesResponse{FlowArn: in.FlowArn, VpcInterfaces: r.Flow.VpcInterfaces[start:]}, nil
}

func (s *Service) RemoveFlowVpcInterface(
	ctx context.Context,
	in *api.RemoveFlowVpcInterfaceRequest,
) (*api.RemoveFlowVpcInterfaceResponse, error) {
	_, err := s.mutate(ctx, in.FlowArn, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		i := slices.IndexFunc(
			r.Flow.VpcInterfaces,
			func(v api.VpcInterface) bool { return common.Value(v.Name) == common.Value(in.VpcInterfaceName) },
		)
		if i < 0 {
			return missing()
		}

		r.Flow.VpcInterfaces = slices.Delete(r.Flow.VpcInterfaces, i, i+1)
		return validateFlow(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveFlowVpcInterfaceResponse{FlowArn: in.FlowArn, VpcInterfaceName: in.VpcInterfaceName}, nil
}
