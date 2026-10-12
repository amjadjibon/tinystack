package applicationautoscaling

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type filter struct {
	Namespace *string
	Dimension *string
	Resources []string
	Names     []string
}

func (f filter) check() error {
	if validationErr := checkFilter(f.Namespace, f.Dimension); validationErr != nil {
		return validationErr
	}

	if len(f.Resources) > 50 || len(f.Names) > 50 {
		return awserror.Validation("At most 50 filter values are allowed")
	}

	for _, v := range append(slices.Clone(f.Resources), f.Names...) {
		if validationErr := checkText(&v, 1600); validationErr != nil {
			return validationErr
		}
	}

	return nil
}

func (f filter) matches(namespace, resource, dimension, name *string) bool {
	return *namespace == *f.Namespace && (f.Dimension == nil || *f.Dimension == *dimension) &&
		(len(f.Resources) == 0 || slices.Contains(f.Resources, *resource)) &&
		(len(f.Names) == 0 || slices.Contains(f.Names, common.Value(name)))
}

func page[T any](
	scope request.Scope,
	kind string,
	f filter,
	items []T,
	id func(T) string,
	maxResults *int64,
	token *string,
) (selected []T, next *string, err error) {
	limit := int64(50)
	if maxResults != nil {
		limit = *maxResults
	}

	if limit < 1 || limit > 50 {
		return nil, nil, awserror.Validation("MaxResults must be 1-50")
	}

	raw, err := json.Marshal(f)
	if err != nil {
		return nil, nil, err
	}

	binding := fmt.Sprintf("%x/", sha256.Sum256(append([]byte(scope.AccountID+"/"+scope.Region+"/"+kind+"/"), raw...)))
	invalid := awserror.New("InvalidNextTokenException", "Invalid NextToken for this scope, API and filters")
	if token != nil {
		decoded, e := base64.RawURLEncoding.DecodeString(*token)
		if e != nil || !strings.HasPrefix(string(decoded), binding) || len(decoded) == len(binding) {
			return nil, nil, invalid
		}
	}

	selected, next, err = common.Page(items, func(v T) string { return binding + id(v) }, token, int(limit), invalid)
	if selected == nil {
		selected = []T{}
	}

	return selected, next, err
}

func (s *Service) DescribeScalableTargets(
	ctx context.Context,
	in *api.DescribeScalableTargetsRequest,
) (*api.DescribeScalableTargetsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	f := filter{Namespace: in.ServiceNamespace, Dimension: in.ScalableDimension, Resources: in.ResourceIds}
	if validationErr := f.check(); validationErr != nil {
		return nil, validationErr
	}

	var items []api.ScalableTarget
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[target](tx, scope.Prefix(serviceName, targetKind))
		if e != nil {
			return e
		}

		for _, r := range records {
			t := r.Configuration
			if f.matches(t.ServiceNamespace, t.ResourceId, t.ScalableDimension, nil) {
				items = append(items, t)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	values, next, err := page(scope, targetKind, f, items, func(t api.ScalableTarget) string {
		return identity(t.ServiceNamespace, t.ResourceId, t.ScalableDimension)
	}, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.DescribeScalableTargetsResponse{ScalableTargets: values, NextToken: next}, nil
}

func (s *Service) DescribeScalingPolicies(
	ctx context.Context,
	in *api.DescribeScalingPoliciesRequest,
) (*api.DescribeScalingPoliciesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	f := filter{Namespace: in.ServiceNamespace, Dimension: in.ScalableDimension, Names: in.PolicyNames}
	if in.ResourceId != nil {
		f.Resources = []string{*in.ResourceId}
	}

	if validationErr := f.check(); validationErr != nil {
		return nil, validationErr
	}

	var items []api.ScalingPolicy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.ScalingPolicy](tx, scope.Prefix(serviceName, policyKind))
		if e != nil {
			return e
		}

		for _, r := range records {
			if f.matches(r.ServiceNamespace, r.ResourceId, r.ScalableDimension, r.PolicyName) {
				items = append(items, *r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	values, next, err := page(scope, policyKind, f, items, func(p api.ScalingPolicy) string {
		return childID(identity(p.ServiceNamespace, p.ResourceId, p.ScalableDimension), p.PolicyName)
	}, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.DescribeScalingPoliciesResponse{ScalingPolicies: values, NextToken: next}, nil
}

func (s *Service) DescribeScheduledActions(
	ctx context.Context,
	in *api.DescribeScheduledActionsRequest,
) (*api.DescribeScheduledActionsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	f := filter{Namespace: in.ServiceNamespace, Dimension: in.ScalableDimension, Names: in.ScheduledActionNames}
	if in.ResourceId != nil {
		f.Resources = []string{*in.ResourceId}
	}

	if validationErr := f.check(); validationErr != nil {
		return nil, validationErr
	}

	var items []api.ScheduledAction
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.ScheduledAction](tx, scope.Prefix(serviceName, actionKind))
		if e != nil {
			return e
		}

		for _, r := range records {
			if f.matches(r.ServiceNamespace, r.ResourceId, r.ScalableDimension, r.ScheduledActionName) {
				items = append(items, *r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	values, next, err := page(scope, actionKind, f, items, func(a api.ScheduledAction) string {
		return childID(identity(a.ServiceNamespace, a.ResourceId, a.ScalableDimension), a.ScheduledActionName)
	}, in.MaxResults, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.DescribeScheduledActionsResponse{ScheduledActions: values, NextToken: next}, nil
}
