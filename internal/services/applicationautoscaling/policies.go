package applicationautoscaling

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) PutScalingPolicy(
	ctx context.Context,
	in *api.PutScalingPolicyRequest,
) (*api.PutScalingPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	in = common.Clone(in)
	if in == nil {
		return nil, awserror.Validation("Input contains invalid JSON values")
	}

	if validationErr := checkIdentity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkName(in.PolicyName, false); validationErr != nil {
		return nil, validationErr
	}

	id := identity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension)
	key := scope.Key(serviceName, policyKind, childID(id, in.PolicyName))
	var out *api.PutScalingPolicyResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := requireTarget(tx, scope, id); e != nil {
			return e
		}

		r, exists, e := storage.Read[api.ScalingPolicy](tx, key)
		if e != nil {
			return e
		}

		if !exists {
			r = &api.ScalingPolicy{
				PolicyARN: new(
					fmt.Sprintf(
						"arn:aws:autoscaling:%s:%s:scalingPolicy:%s:resource/%s/%s:policyName/%s",
						scope.Region,
						scope.AccountID,
						uuid.NewV4().String(),
						*in.ServiceNamespace,
						*in.ResourceId,
						*in.PolicyName,
					),
				),
				PolicyName:        in.PolicyName,
				ServiceNamespace:  in.ServiceNamespace,
				ResourceId:        in.ResourceId,
				ScalableDimension: in.ScalableDimension,
				CreationTime:      new(common.Unix(time.Now())),
				Alarms:            []api.Alarm{},
			}
		}

		if in.PolicyType != nil {
			if common.Value(r.PolicyType) != *in.PolicyType {
				r.StepScalingPolicyConfiguration = nil
				r.TargetTrackingScalingPolicyConfiguration = nil
			}

			r.PolicyType = in.PolicyType
		}

		if in.StepScalingPolicyConfiguration != nil {
			r.StepScalingPolicyConfiguration = in.StepScalingPolicyConfiguration
		}

		if in.TargetTrackingScalingPolicyConfiguration != nil {
			r.TargetTrackingScalingPolicyConfiguration = in.TargetTrackingScalingPolicyConfiguration
		}

		if e := checkPolicy(r); e != nil {
			return e
		}

		out = &api.PutScalingPolicyResponse{PolicyARN: r.PolicyARN, Alarms: []api.Alarm{}}
		return storage.Write(tx, key, r)
	})
	return out, err
}

func (s *Service) DeleteScalingPolicy(
	ctx context.Context,
	in *api.DeleteScalingPolicyRequest,
) (*api.DeleteScalingPolicyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, awserror.Validation("Input is required")
	}

	if validationErr := checkIdentity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkName(in.PolicyName, false); validationErr != nil {
		return nil, validationErr
	}

	key := scope.Key(
		serviceName,
		policyKind,
		childID(identity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension), in.PolicyName),
	)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(key); !ok {
			return missing()
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteScalingPolicyResponse{}, nil
}
