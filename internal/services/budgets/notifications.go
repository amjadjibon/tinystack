package budgets

import (
	"context"
	"fmt"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
)

func notificationLimit() error {
	return awserror.New("CreationLimitExceededException", "A budget supports five notifications.")
}

func (s *Service) CreateNotification(
	ctx context.Context,
	in *api.CreateNotificationRequest,
) (*api.CreateNotificationResponse, error) {
	if err := checkNotification(in.Notification); err != nil {
		return nil, err
	}

	if err := checkSubscribers(in.Subscribers); err != nil {
		return nil, err
	}

	err := s.change(ctx, in.AccountId, in.BudgetName, func(r *record) error {
		for _, v := range r.Notifications {
			if sameNotification(v.Notification, in.Notification) {
				return duplicate()
			}
		}

		if len(r.Notifications) >= 5 {
			return notificationLimit()
		}

		r.Notifications = append(
			r.Notifications,
			api.NotificationWithSubscribers{Notification: in.Notification, Subscribers: in.Subscribers},
		)
		return nil
	})
	return &api.CreateNotificationResponse{}, err
}

func (s *Service) UpdateNotification(
	ctx context.Context,
	in *api.UpdateNotificationRequest,
) (*api.UpdateNotificationResponse, error) {
	if err := checkNotification(in.NewNotification); err != nil {
		return nil, err
	}

	err := s.change(ctx, in.AccountId, in.BudgetName, func(r *record) error {
		i, err := notificationIndex(r, in.OldNotification)
		if err != nil {
			return err
		}

		for j, v := range r.Notifications {
			if i != j && sameNotification(v.Notification, in.NewNotification) {
				return duplicate()
			}
		}

		r.Notifications[i].Notification = in.NewNotification
		return nil
	})
	return &api.UpdateNotificationResponse{}, err
}

func (s *Service) DeleteNotification(
	ctx context.Context,
	in *api.DeleteNotificationRequest,
) (*api.DeleteNotificationResponse, error) {
	err := s.change(ctx, in.AccountId, in.BudgetName, func(r *record) error {
		i, err := notificationIndex(r, in.Notification)
		if err != nil {
			return err
		}

		r.Notifications = slices.Delete(r.Notifications, i, i+1)
		return nil
	})
	return &api.DeleteNotificationResponse{}, err
}

func (s *Service) DescribeNotificationsForBudget(
	ctx context.Context,
	in *api.DescribeNotificationsForBudgetRequest,
) (*api.DescribeNotificationsForBudgetResponse, error) {
	r, err := s.get(ctx, in.AccountId, in.BudgetName)
	if err != nil {
		return nil, err
	}

	if stateErr := s.notificationStates(ctx, *in.AccountId, &r); stateErr != nil {
		return nil, stateErr
	}

	// Aggregate children retain insertion order; ordinal IDs preserve that order for pagination.
	ids := make([]int, len(r.Notifications))
	for i := range ids {
		ids[i] = i
	}

	ids, next, err := page(ids, func(i int) string { return fmt.Sprintf("%02d", i) }, in.NextToken, in.MaxResults, 100)
	out := &api.DescribeNotificationsForBudgetResponse{NextToken: next}
	for _, i := range ids {
		out.Notifications = append(out.Notifications, *r.Notifications[i].Notification)
	}

	return out, err
}
