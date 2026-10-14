package budgets

import (
	"context"
	"fmt"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
)

func (s *Service) CreateSubscriber(
	ctx context.Context,
	in *api.CreateSubscriberRequest,
) (*api.CreateSubscriberResponse, error) {
	if err := checkSubscriber(in.Subscriber); err != nil {
		return nil, err
	}

	err := s.change(ctx, in.AccountId, in.BudgetName, func(r *record) error {
		i, err := notificationIndex(r, in.Notification)
		if err != nil {
			return err
		}

		subs := r.Notifications[i].Subscribers
		subs = append(subs, *in.Subscriber)
		if checkErr := checkSubscribers(subs); checkErr != nil {
			return checkErr
		}

		r.Notifications[i].Subscribers = subs
		return nil
	})
	return &api.CreateSubscriberResponse{}, err
}

func (s *Service) UpdateSubscriber(
	ctx context.Context,
	in *api.UpdateSubscriberRequest,
) (*api.UpdateSubscriberResponse, error) {
	if err := checkSubscriber(in.NewSubscriber); err != nil {
		return nil, err
	}

	err := s.change(ctx, in.AccountId, in.BudgetName, func(r *record) error {
		i, err := notificationIndex(r, in.Notification)
		if err != nil {
			return err
		}

		subs := r.Notifications[i].Subscribers
		j, err := subscriberIndex(subs, in.OldSubscriber)
		if err != nil {
			return err
		}

		subs[j] = *in.NewSubscriber
		return checkSubscribers(subs)
	})
	return &api.UpdateSubscriberResponse{}, err
}

func (s *Service) DeleteSubscriber(
	ctx context.Context,
	in *api.DeleteSubscriberRequest,
) (*api.DeleteSubscriberResponse, error) {
	err := s.change(ctx, in.AccountId, in.BudgetName, func(r *record) error {
		i, err := notificationIndex(r, in.Notification)
		if err != nil {
			return err
		}

		subs := r.Notifications[i].Subscribers
		j, err := subscriberIndex(subs, in.Subscriber)
		if err != nil {
			return err
		}

		r.Notifications[i].Subscribers = slices.Delete(subs, j, j+1)
		return nil
	})
	return &api.DeleteSubscriberResponse{}, err
}

func (s *Service) DescribeSubscribersForNotification(
	ctx context.Context,
	in *api.DescribeSubscribersForNotificationRequest,
) (*api.DescribeSubscribersForNotificationResponse, error) {
	r, err := s.get(ctx, in.AccountId, in.BudgetName)
	if err != nil {
		return nil, err
	}

	i, err := notificationIndex(&r, in.Notification)
	if err != nil {
		return nil, err
	}

	subs := r.Notifications[i].Subscribers
	ids := make([]int, len(subs))
	for j := range ids {
		ids[j] = j
	}

	ids, next, err := page(ids, func(j int) string { return fmt.Sprintf("%02d", j) }, in.NextToken, in.MaxResults, 100)
	out := &api.DescribeSubscribersForNotificationResponse{NextToken: next}
	for _, j := range ids {
		out.Subscribers = append(out.Subscribers, subs[j])
	}

	return out, err
}
