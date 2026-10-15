package budgets

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateBudget(ctx context.Context, in *api.CreateBudgetRequest) (*api.CreateBudgetResponse, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	if checkErr := checkBudget(in.Budget); checkErr != nil {
		return nil, checkErr
	}

	tags, err := tagMap(in.ResourceTags)
	if err != nil {
		return nil, err
	}

	if len(in.NotificationsWithSubscribers) > 5 {
		return nil, notificationLimit()
	}

	for i, v := range in.NotificationsWithSubscribers {
		if checkErr := checkNotification(v.Notification); checkErr != nil {
			return nil, checkErr
		}

		if checkErr := checkSubscribers(v.Subscribers); checkErr != nil {
			return nil, checkErr
		}

		for _, old := range in.NotificationsWithSubscribers[:i] {
			if sameNotification(old.Notification, v.Notification) {
				return nil, duplicate()
			}
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, *in.Budget.BudgetName)
		if _, ok := tx.Get(k); ok {
			return duplicate()
		}

		return storage.Write(
			tx,
			k,
			record{Budget: *in.Budget, Notifications: in.NotificationsWithSubscribers, Tags: tags},
		)
	})
	return &api.CreateBudgetResponse{}, err
}

func (s *Service) DescribeBudget(
	ctx context.Context,
	in *api.DescribeBudgetRequest,
) (*api.DescribeBudgetResponse, error) {
	r, err := s.get(ctx, in.AccountId, in.BudgetName)
	if err == nil {
		err = s.actualSpend(ctx, &r.Budget)
	}

	return &api.DescribeBudgetResponse{Budget: &r.Budget}, err
}

func (s *Service) DescribeBudgets(
	ctx context.Context,
	in *api.DescribeBudgetsRequest,
) (*api.DescribeBudgetsResponse, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	var rows []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		rows, e = storage.Records[record](tx, scope.Prefix(serviceName, "budget"))
		return e
	})
	if err != nil {
		return nil, err
	}

	rows, next, err := page(
		rows,
		func(r *record) string { return key(scope, *r.Budget.BudgetName) },
		in.NextToken,
		in.MaxResults,
		1000,
	)
	out := &api.DescribeBudgetsResponse{NextToken: next}
	for _, r := range rows {
		if e := s.actualSpend(ctx, &r.Budget); e != nil {
			return nil, e
		}

		out.Budgets = append(out.Budgets, r.Budget)
	}

	return out, err
}

func (s *Service) UpdateBudget(ctx context.Context, in *api.UpdateBudgetRequest) (*api.UpdateBudgetResponse, error) {
	if err := checkBudget(in.NewBudget); err != nil {
		return nil, err
	}

	err := s.change(
		ctx,
		in.AccountId,
		in.NewBudget.BudgetName,
		func(r *record) error { r.Budget = *in.NewBudget; return nil },
	)
	return &api.UpdateBudgetResponse{}, err
}

func (s *Service) DeleteBudget(ctx context.Context, in *api.DeleteBudgetRequest) (*api.DeleteBudgetResponse, error) {
	scope, err := scopeFor(ctx, in.AccountId)
	if err != nil {
		return nil, err
	}

	if checkErr := checkName(in.BudgetName); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, *in.BudgetName)
		if _, readErr := read(tx, k); readErr != nil {
			return readErr
		}

		tx.Delete(k)
		return nil
	})
	return &api.DeleteBudgetResponse{}, err
}
