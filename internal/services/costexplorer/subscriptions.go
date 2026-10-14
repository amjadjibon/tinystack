package costexplorer

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func validateSubscription(sub *api.AnomalySubscription, scope request.Scope, tx storage.ReadTx) error {
	if err := genericName(sub.SubscriptionName); err != nil {
		return err
	}

	if !slices.Contains([]string{"DAILY", "WEEKLY", "IMMEDIATE"}, common.Value(sub.Frequency)) {
		return awserror.Validation("Frequency must be DAILY, WEEKLY or IMMEDIATE.")
	}

	if (sub.Threshold == nil) == (sub.ThresholdExpression == nil) {
		return awserror.Validation("Exactly one of Threshold and ThresholdExpression is required.")
	}

	if sub.Threshold != nil && *sub.Threshold < 0 {
		return awserror.Validation("Threshold must be nonnegative.")
	}

	if sub.ThresholdExpression != nil {
		if err := validateThreshold(sub.ThresholdExpression); err != nil {
			return err
		}
	}

	if len(sub.MonitorArnList) == 0 || len(sub.Subscribers) == 0 {
		return awserror.Validation("MonitorArnList and Subscribers are required.")
	}

	for _, subscriber := range sub.Subscribers {
		if err := validateSubscriber(subscriber, common.Value(sub.Frequency)); err != nil {
			return err
		}
	}

	for _, value := range sub.MonitorArnList {
		k, e := resourceKey(scope, new(value), monitorKind)
		if e != nil {
			return e
		}

		if _, ok := tx.Get(k); !ok {
			return notFound(monitorKind)
		}
	}

	return nil
}

func (s *Service) CreateAnomalySubscription(
	ctx context.Context,
	in *api.CreateAnomalySubscriptionRequest,
) (*api.CreateAnomalySubscriptionResponse, error) {
	sub := in.AnomalySubscription
	if sub == nil {
		return nil, awserror.Validation("AnomalySubscription is required.")
	}

	if sub.SubscriptionArn != nil || sub.AccountId != nil {
		return nil, awserror.Validation("SubscriptionArn and AccountId are output fields.")
	}

	for _, subscriber := range sub.Subscribers {
		if subscriber.Status != nil {
			return nil, awserror.Validation("Subscriber Status is an output field.")
		}
	}

	r := &record{Subscription: common.Clone(sub)}
	if err := s.create(ctx, subscriptionKind, r, in.ResourceTags); err != nil {
		return nil, err
	}

	return &api.CreateAnomalySubscriptionResponse{SubscriptionArn: new(r.ARN)}, nil
}

func (s *Service) GetAnomalySubscriptions(
	ctx context.Context,
	in *api.GetAnomalySubscriptionsRequest,
) (*api.GetAnomalySubscriptionsResponse, error) {
	rs, next, err := s.list(ctx, subscriptionKind, in.NextPageToken, in.MaxResults, func(r *record) bool {
		return (len(in.SubscriptionArnList) == 0 || slices.Contains(in.SubscriptionArnList, r.ARN)) &&
			(in.MonitorArn == nil || slices.Contains(r.Subscription.MonitorArnList, *in.MonitorArn))
	}, in.SubscriptionArnList)
	if err != nil {
		return nil, err
	}

	out := &api.GetAnomalySubscriptionsResponse{AnomalySubscriptions: []api.AnomalySubscription{}, NextPageToken: next}
	for _, r := range rs {
		out.AnomalySubscriptions = append(out.AnomalySubscriptions, *r.Subscription)
	}

	return out, nil
}

func (s *Service) UpdateAnomalySubscription(
	ctx context.Context,
	in *api.UpdateAnomalySubscriptionRequest,
) (*api.UpdateAnomalySubscriptionResponse, error) {
	if in.Threshold != nil && in.ThresholdExpression != nil {
		return nil, awserror.Validation("Threshold and ThresholdExpression are mutually exclusive.")
	}

	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.edit(ctx, in.SubscriptionArn, subscriptionKind, func(r *record, tx storage.WriteTx) error {
		merged := r.Subscription
		if in.Threshold != nil {
			merged.Threshold, merged.ThresholdExpression = in.Threshold, nil
		}

		if in.ThresholdExpression != nil {
			merged.ThresholdExpression, merged.Threshold = in.ThresholdExpression, nil
		}

		if in.Frequency != nil {
			merged.Frequency = in.Frequency
		}

		if in.MonitorArnList != nil {
			merged.MonitorArnList = in.MonitorArnList
		}

		if in.Subscribers != nil {
			merged.Subscribers = in.Subscribers
		}

		if in.SubscriptionName != nil {
			merged.SubscriptionName = in.SubscriptionName
		}

		return validateSubscription(merged, scope, tx)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAnomalySubscriptionResponse{SubscriptionArn: in.SubscriptionArn}, nil
}

func (s *Service) DeleteAnomalySubscription(
	ctx context.Context,
	in *api.DeleteAnomalySubscriptionRequest,
) (*api.DeleteAnomalySubscriptionResponse, error) {
	if err := s.remove(ctx, in.SubscriptionArn, subscriptionKind); err != nil {
		return nil, err
	}

	return &api.DeleteAnomalySubscriptionResponse{}, nil
}
