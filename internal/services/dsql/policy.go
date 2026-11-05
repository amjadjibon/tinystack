package dsql

import (
	"context"
	"encoding/json"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dsql"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Lockout safety requires IAM policy evaluation; explicitly require the caller to bypass it.
func checkPolicy(policy *string, bypass *bool) error {
	if policy == nil {
		return nil
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*policy), &document); err != nil || document == nil {
		return invalid("Policy must be a JSON object")
	}

	if _, ok := document["Statement"]; !ok {
		return invalid("Policy must contain Statement")
	}

	if !common.Value(bypass) {
		return invalid("Policy lockout safety evaluation is unsupported; set bypassPolicyLockoutSafetyCheck")
	}

	return nil
}

func (s *Service) GetClusterPolicy(
	ctx context.Context,
	in *api.GetClusterPolicyInput,
) (*api.GetClusterPolicyOutput, error) {
	r, err := s.get(ctx, in.Identifier)
	if err != nil {
		return nil, err
	}

	if r.Policy == nil {
		return nil, missing()
	}

	return &api.GetClusterPolicyOutput{Policy: r.Policy, PolicyVersion: new(r.PolicyVersion)}, nil
}

func (s *Service) PutClusterPolicy(
	ctx context.Context,
	in *api.PutClusterPolicyInput,
) (*api.PutClusterPolicyOutput, error) {
	return mutate(ctx, s, "PutClusterPolicy", in.ClientToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.PutClusterPolicyOutput, error) {
			r, err := read(tx, scope, common.Value(in.Identifier))
			if err != nil {
				return nil, err
			}

			if err := checkPolicy(in.Policy, in.BypassPolicyLockoutSafetyCheck); err != nil {
				return nil, err
			}

			if in.ExpectedPolicyVersion != nil && *in.ExpectedPolicyVersion != r.PolicyVersion {
				return nil, conflict("Policy version does not match expectedPolicyVersion")
			}

			r.Policy = in.Policy
			r.PolicyVersion = common.RandomID(32)
			if err := write(tx, scope, r); err != nil {
				return nil, err
			}

			return &api.PutClusterPolicyOutput{PolicyVersion: new(r.PolicyVersion)}, nil
		})
}

func (s *Service) DeleteClusterPolicy(
	ctx context.Context,
	in *api.DeleteClusterPolicyInput,
) (*api.DeleteClusterPolicyOutput, error) {
	return mutate(ctx, s, "DeleteClusterPolicy", in.ClientToken, in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteClusterPolicyOutput, error) {
			r, err := read(tx, scope, common.Value(in.Identifier))
			if err != nil {
				return nil, err
			}

			if r.Policy == nil {
				return nil, missing()
			}

			if in.ExpectedPolicyVersion != nil && *in.ExpectedPolicyVersion != r.PolicyVersion {
				return nil, conflict("Policy version does not match expectedPolicyVersion")
			}

			version := r.PolicyVersion
			r.Policy = nil
			r.PolicyVersion = ""
			if err := write(tx, scope, r); err != nil {
				return nil, err
			}

			return &api.DeleteClusterPolicyOutput{PolicyVersion: new(version)}, nil
		})
}
