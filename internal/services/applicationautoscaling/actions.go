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

func (s *Service) PutScheduledAction(
	ctx context.Context,
	in *api.PutScheduledActionRequest,
) (*api.PutScheduledActionResponse, error) {
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

	if validationErr := checkName(in.ScheduledActionName, true); validationErr != nil {
		return nil, validationErr
	}

	if validationErr := checkTimezone(in.Timezone); validationErr != nil {
		return nil, validationErr
	}

	id := identity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension)
	key := scope.Key(serviceName, actionKind, childID(id, in.ScheduledActionName))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := requireTarget(tx, scope, id); e != nil {
			return e
		}

		r, exists, e := storage.Read[api.ScheduledAction](tx, key)
		if e != nil {
			return e
		}

		if !exists {
			r = &api.ScheduledAction{
				ScheduledActionName: in.ScheduledActionName,
				ServiceNamespace:    in.ServiceNamespace,
				ResourceId:          in.ResourceId,
				ScalableDimension:   in.ScalableDimension,
				ScheduledActionARN: new(
					fmt.Sprintf(
						"arn:aws:autoscaling:%s:%s:scheduledAction:%s:resource/%s/%s:scheduledActionName/%s",
						scope.Region,
						scope.AccountID,
						uuid.NewV4().String(),
						*in.ServiceNamespace,
						*in.ResourceId,
						*in.ScheduledActionName,
					),
				),
				CreationTime: new(common.Unix(time.Now())),
				Timezone:     new("UTC"),
			}
		}

		if in.Schedule != nil {
			r.Schedule = in.Schedule
		}

		if in.Timezone != nil {
			r.Timezone = in.Timezone
		}

		if in.StartTime != nil {
			r.StartTime = in.StartTime
		}

		if in.EndTime != nil {
			r.EndTime = in.EndTime
		}

		if in.ScalableTargetAction != nil {
			r.ScalableTargetAction = in.ScalableTargetAction
		}

		if e := checkAction(r); e != nil {
			return e
		}

		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutScheduledActionResponse{}, nil
}

func (s *Service) DeleteScheduledAction(
	ctx context.Context,
	in *api.DeleteScheduledActionRequest,
) (*api.DeleteScheduledActionResponse, error) {
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

	if validationErr := checkName(in.ScheduledActionName, true); validationErr != nil {
		return nil, validationErr
	}

	key := scope.Key(
		serviceName,
		actionKind,
		childID(identity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension), in.ScheduledActionName),
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

	return &api.DeleteScheduledActionResponse{}, nil
}
