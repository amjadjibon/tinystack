package networkfirewall

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) checkResourceExists(tx storage.ReadTx, scope request.Scope, arn string) error {
	// Resource can be a firewall, policy, or rule group
	if fwName, err := resolveFirewallName(scope, nil, new(arn)); err == nil {
		if keyExists(tx, scope.Key(serviceName, kindFirewall, fwName)) {
			return nil
		}
	}

	if polName, err := resolvePolicyName(scope, nil, new(arn)); err == nil {
		if keyExists(tx, scope.Key(serviceName, kindPolicy, polName)) {
			return nil
		}
	}

	if rgName, err := resolveRuleGroupName(scope, nil, new(arn)); err == nil {
		if keyExists(tx, scope.Key(serviceName, kindRuleGroup, rgName)) {
			return nil
		}
	}

	return awserror.New("ResourceNotFoundException", fmt.Sprintf("Resource not found: %s", arn))
}

func (s *Service) PutResourcePolicy(
	ctx context.Context,
	in *api.PutResourcePolicyRequest,
) (*api.PutResourcePolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := *in.ResourceArn
	key := scope.Key(serviceName, kindResourcePolicy, arn)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if cErr := s.checkResourceExists(tx, scope, arn); cErr != nil {
			return cErr
		}

		rec := resourcePolicyRecord{
			ResourceArn: arn,
			Policy:      *in.Policy,
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutResourcePolicyResponse{}, nil
}

func (s *Service) DescribeResourcePolicy(
	ctx context.Context,
	in *api.DescribeResourcePolicyRequest,
) (*api.DescribeResourcePolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := *in.ResourceArn
	key := scope.Key(serviceName, kindResourcePolicy, arn)

	var resp *api.DescribeResourcePolicyResponse
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, ok, rErr := storage.Read[resourcePolicyRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !ok {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("No resource policy found for %s", arn))
		}

		resp = &api.DescribeResourcePolicyResponse{
			Policy: new(rec.Policy),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *Service) DeleteResourcePolicy(
	ctx context.Context,
	in *api.DeleteResourcePolicyRequest,
) (*api.DeleteResourcePolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := *in.ResourceArn
	key := scope.Key(serviceName, kindResourcePolicy, arn)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if !keyExists(tx, key) {
			return awserror.New("ResourceNotFoundException", fmt.Sprintf("No resource policy found for %s", arn))
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteResourcePolicyResponse{}, nil
}
