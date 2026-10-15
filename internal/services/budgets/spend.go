package budgets

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	ce "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
)

func (s *Service) actualSpend(ctx context.Context, b *api.Budget) error {
	b.CalculatedSpend = nil

	if common.Value(b.BudgetType) != budgetCost || b.CostTypes != nil {
		return nil
	}
	if b.BudgetLimit != nil {
		if common.Value(b.BudgetLimit.Unit) != "USD" {
			return nil
		}
	} else {
		if len(b.PlannedBudgetLimits) == 0 {
			return nil
		}
		for _, limit := range b.PlannedBudgetLimits {
			if common.Value(limit.Unit) != "USD" {
				return nil
			}
		}
	}

	months := 0
	switch common.Value(b.TimeUnit) {
	case timeMonthly:
		months = 1
	case timeQuarterly:
		months = 3
	case timeAnnually:
		months = 12
	default:
		return nil
	}

	now := time.Now().UTC()
	month := time.Month((int(now.Month())-1)/months*months + 1)
	start := time.Date(now.Year(), month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, months, 0)
	if b.TimePeriod != nil {
		if b.TimePeriod.Start != nil {
			t := time.Unix(int64(*b.TimePeriod.Start), 0).UTC()
			if t.After(now) {
				return nil
			}

			if t.After(start) {
				start = t
			}
		}

		if b.TimePeriod.End != nil {
			t := time.Unix(int64(*b.TimePeriod.End), 0).UTC()
			if !t.After(now) {
				return nil
			}

			if t.Before(end) {
				end = t
			}
		}
	}

	if end.After(now) {
		end = now
	}

	var filter *ce.Expression
	if len(b.CostFilters) > 0 {
		filter = &ce.Expression{}
		for key, values := range b.CostFilters {
			if len(values) == 0 {
				return nil
			}
			if _, ok := costdata.Dimension(&costdata.Line{}, key); !ok {
				return nil
			}

			filter.And = append(
				filter.And,
				ce.Expression{Dimensions: &ce.DimensionValues{Key: new(key), Values: values}},
			)
		}
	}

	match, err := costdata.Predicate(filter)
	if err != nil {
		return err
	}

	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	fixtures, err := costdata.Load(ctx, s.store, scope)
	if err != nil {
		return err
	}

	amount := costdata.Sum(costdata.Select(costdata.Lines(fixtures), start, end, match))
	b.CalculatedSpend = &api.CalculatedSpend{ActualSpend: &api.Spend{Amount: new(amount), Unit: new("USD")}}
	return nil
}
