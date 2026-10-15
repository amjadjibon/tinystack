package budgets

import (
	"context"
	"maps"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(k, serviceName)
			if !ok || !strings.HasPrefix(rest, "budget/") {
				continue
			}

			r, err := read(tx, k)
			if err != nil {
				return err
			}

			scope := request.Scope{AccountID: account}
			fixtures, err := storage.Records[costdata.Fixture](tx, scope.Prefix("costdata", "fixture"))
			if err != nil {
				return err
			}

			now := s.now().UTC()
			if err := actualSpendAt(&r.Budget, costdata.Lines(fixtures), now); err != nil {
				return err
			}

			ready := r.Budget.CalculatedSpend != nil
			setNotificationStates(tx, scope, &r, now)

			out = append(out, catalog.Resource{
				Account: account, Region: region, Type: "budget", Name: *r.Budget.BudgetName,
				ARN: arn(account, *r.Budget.BudgetName), Tags: maps.Clone(r.Tags), Details: map[string]any{
					"RecordsOnly":    !ready,
					"ExecutionReady": ready,
					"Configuration":  r.Budget,
					"Notifications":  r.Notifications,
				},
			})
		}

		return nil
	})
	return out, err
}
