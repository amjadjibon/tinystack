package shield

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateSubscription(
	ctx context.Context,
	_ *api.CreateSubscriptionRequest,
) (*api.CreateSubscriptionResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, "subscription", "default")
		if _, ok, readErr := storage.Read[subscriptionRecord](tx, key); readErr != nil {
			return readErr
		} else if ok {
			return already()
		}

		now := time.Now().UTC()
		end := now.AddDate(1, 0, 0)
		limits := []api.Limit{}
		for _, kind := range []string{
			"CLOUDFRONT_DISTRIBUTION", resourceHostedZone, "ELASTIC_IP_ALLOCATION",
			"CLASSIC_LOAD_BALANCER", "APPLICATION_LOAD_BALANCER", "GLOBAL_ACCELERATOR",
		} {
			limits = append(limits, api.Limit{Type: new(kind), Max: new(int64(1000))})
		}

		r := subscriptionRecord{Subscription: api.Subscription{
			StartTime:               new(float64(now.Unix())),
			EndTime:                 new(float64(end.Unix())),
			TimeCommitmentInSeconds: new(int64(end.Sub(now).Seconds())),
			AutoRenew:               new("ENABLED"),
			ProactiveEngagementStatus: new(
				"DISABLED",
			),
			SubscriptionArn: new(arn(scope, "subscription", common.RandomID(32, "0123456789abcdef"))),
			Limits:          limits,
			SubscriptionLimits: &api.SubscriptionLimits{
				ProtectionLimits: &api.ProtectionLimits{ProtectedResourceTypeLimits: limits},
				ProtectionGroupLimits: &api.ProtectionGroupLimits{
					MaxProtectionGroups: new(int64(100)),
					PatternTypeLimits: &api.ProtectionGroupPatternTypeLimits{
						ArbitraryPatternLimits: &api.ProtectionGroupArbitraryPatternLimits{
							MaxMembers: new(int64(10000)),
						},
					},
				},
			},
		}}
		return storage.Write(tx, key, r)
	})
	return &api.CreateSubscriptionResponse{}, err
}

func (s *Service) DescribeSubscription(
	ctx context.Context,
	_ *api.DescribeSubscriptionRequest,
) (*api.DescribeSubscriptionResponse, error) {
	out := &api.DescribeSubscriptionResponse{}
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		out.Subscription = &r.Subscription
		return nil
	})
	return out, err
}

func (s *Service) UpdateSubscription(
	ctx context.Context,
	in *api.UpdateSubscriptionRequest,
) (*api.UpdateSubscriptionResponse, error) {
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		if in.AutoRenew != nil && common.Value(in.AutoRenew) != common.Value(r.Subscription.AutoRenew) {
			remaining := common.Value(r.Subscription.EndTime) - float64(time.Now().Unix())
			if remaining > 30*86400 || remaining < 86400 {
				return awserror.New(
					"LockedSubscriptionException",
					"AutoRenew can change only from 30 days to one day before subscription end",
				)
			}

			r.Subscription.AutoRenew = in.AutoRenew
		}

		return nil
	})
	return &api.UpdateSubscriptionResponse{}, err
}

func (s *Service) GetSubscriptionState(
	ctx context.Context,
	_ *api.GetSubscriptionStateRequest,
) (*api.GetSubscriptionStateResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.GetSubscriptionStateResponse{SubscriptionState: new("INACTIVE")}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, ok, readErr := storage.Read[subscriptionRecord](tx, scope.Key(serviceName, "subscription", "default"))
		if readErr != nil {
			return readErr
		}

		if ok {
			out.SubscriptionState = new("ACTIVE")
		}

		return nil
	})
	return out, err
}

func (s *Service) DeleteSubscription(
	ctx context.Context,
	_ *api.DeleteSubscriptionRequest,
) (*api.DeleteSubscriptionResponse, error) {
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, _ *subscriptionRecord) error {
		return awserror.New(
			"LockedSubscriptionException",
			"Shield Advanced subscriptions cannot be deleted during their time commitment; disable AutoRenew instead",
		)
	})
	return &api.DeleteSubscriptionResponse{}, err
}
