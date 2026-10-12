// Package applicationautoscaling stores scalable targets, policies and scheduled
// actions. It never adjusts capacity, evaluates metrics or creates alarms.
package applicationautoscaling

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "applicationautoscaling"
	targetKind  = "scalable-target"
	policyKind  = "scaling-policy"
	actionKind  = "scheduled-action"
)

var _ api.API = (*Service)(nil)

type (
	Service struct{ store storage.Store }
	target  struct {
		Configuration api.ScalableTarget
		Tags          map[string]string
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }
func identity(namespace, resource, dimension *string) string {
	return *namespace + "/" + base64.RawURLEncoding.EncodeToString([]byte(*resource)) + "/" + *dimension
}

func childID(id string, name *string) string {
	return id + "/" + base64.RawURLEncoding.EncodeToString([]byte(*name))
}

func missing() error {
	return awserror.New("ObjectNotFoundException", "The specified target, policy or scheduled action does not exist")
}

func requireTarget(tx storage.ReadTx, scope request.Scope, id string) error {
	if _, ok := tx.Get(scope.Key(serviceName, targetKind, id)); !ok {
		return missing()
	}
	return nil
}

func (s *Service) RegisterScalableTarget(
	ctx context.Context,
	in *api.RegisterScalableTargetRequest,
) (*api.RegisterScalableTargetResponse, error) {
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

	if in.RoleARN != nil {
		if validationErr := checkRole(*in.RoleARN); validationErr != nil {
			return nil, validationErr
		}
	}

	if validationErr := checkTags(in.Tags); validationErr != nil {
		return nil, validationErr
	}

	id := identity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension)
	var out *api.RegisterScalableTargetResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, exists, e := storage.Read[target](tx, scope.Key(serviceName, targetKind, id))
		if e != nil {
			return e
		}

		if !exists {
			if in.MinCapacity == nil || in.MaxCapacity == nil {
				return awserror.Validation("MinCapacity and MaxCapacity are required for a new target")
			}

			if *in.ServiceNamespace == namespaceEMR && in.RoleARN == nil {
				return awserror.Validation("RoleARN is required for EMR")
			}

			r = &target{Configuration: api.ScalableTarget{
				ServiceNamespace:  in.ServiceNamespace,
				ResourceId:        in.ResourceId,
				ScalableDimension: in.ScalableDimension,
				CreationTime:      new(common.Unix(time.Now())),
				ScalableTargetARN: new(
					fmt.Sprintf(
						"arn:aws:application-autoscaling:%s:%s:scalable-target/%s",
						scope.Region,
						scope.AccountID,
						strings.ReplaceAll(uuid.NewV4().String(), "-", ""),
					),
				),
				RoleARN: new(defaultRole(scope, *in.ServiceNamespace)),
				SuspendedState: &api.SuspendedState{
					DynamicScalingInSuspended:  new(false),
					DynamicScalingOutSuspended: new(false),
					ScheduledScalingSuspended:  new(false),
				},
			}, Tags: maps.Clone(in.Tags)}
		} else if len(in.Tags) > 0 {
			return awserror.Validation("Use TagResource to tag an existing scalable target")
		}

		t := &r.Configuration
		if in.MinCapacity != nil {
			t.MinCapacity = in.MinCapacity
		}

		if in.MaxCapacity != nil {
			t.MaxCapacity = in.MaxCapacity
		}

		if e := checkCapacity(t.MinCapacity, t.MaxCapacity); e != nil {
			return e
		}

		if in.RoleARN != nil {
			t.RoleARN = in.RoleARN
		}

		if in.SuspendedState != nil {
			if in.SuspendedState.DynamicScalingInSuspended != nil {
				t.SuspendedState.DynamicScalingInSuspended = in.SuspendedState.DynamicScalingInSuspended
			}

			if in.SuspendedState.DynamicScalingOutSuspended != nil {
				t.SuspendedState.DynamicScalingOutSuspended = in.SuspendedState.DynamicScalingOutSuspended
			}

			if in.SuspendedState.ScheduledScalingSuspended != nil {
				t.SuspendedState.ScheduledScalingSuspended = in.SuspendedState.ScheduledScalingSuspended
			}
		}

		out = &api.RegisterScalableTargetResponse{ScalableTargetARN: t.ScalableTargetARN}
		return storage.Write(tx, scope.Key(serviceName, targetKind, id), r)
	})
	return out, err
}

func (s *Service) DeregisterScalableTarget(
	ctx context.Context,
	in *api.DeregisterScalableTargetRequest,
) (*api.DeregisterScalableTargetResponse, error) {
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

	id := identity(in.ServiceNamespace, in.ResourceId, in.ScalableDimension)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := requireTarget(tx, scope, id); e != nil {
			return e
		}

		for _, kind := range []string{policyKind, actionKind} {
			for _, key := range tx.Keys(scope.Key(serviceName, kind, id) + "/") {
				tx.Delete(key)
			}
		}

		tx.Delete(scope.Key(serviceName, targetKind, id))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeregisterScalableTargetResponse{}, nil
}
