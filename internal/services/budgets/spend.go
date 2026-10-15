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

// costLines loads the account's cost fixture lines once per request, for
// actualSpend to read for every budget the request returns.
func (s *Service) costLines(ctx context.Context) ([]costdata.Line, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	fixtures, err := costdata.Load(ctx, s.store, scope)
	return costdata.Lines(fixtures), err
}

// actualSpend sets b's CalculatedSpend.ActualSpend from lines, for the budgets
// the fixture data can answer, and leaves it unset for the others.
func actualSpend(b *api.Budget, lines []costdata.Line) error {
	return actualSpendAt(b, lines, time.Now().UTC())
}

func actualSpendAt(b *api.Budget, lines []costdata.Line, now time.Time) error {
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

	start, end, active := budgetPeriod(b, now)
	if !active {
		return nil
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

	amount := costdata.Sum(costdata.Select(lines, start, end, match))
	b.CalculatedSpend = &api.CalculatedSpend{ActualSpend: &api.Spend{Amount: new(amount), Unit: new("USD")}}
	return nil
}

// calendarPeriod identifies a recurring period independently of mutable TimePeriod clipping.
func calendarPeriod(b *api.Budget, now time.Time) (start, end time.Time, active bool) {
	months := 0
	switch common.Value(b.TimeUnit) {
	case timeMonthly:
		months = 1
	case timeQuarterly:
		months = 3
	case timeAnnually:
		months = 12
	default:
		return time.Time{}, time.Time{}, false
	}

	month := time.Month((int(now.Month())-1)/months*months + 1)
	start = time.Date(now.Year(), month, 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, months, 0)
	return start, end, true
}

// budgetPeriod intersects the recurring period with the budget's active dates.
func budgetPeriod(b *api.Budget, now time.Time) (start, end time.Time, active bool) {
	start, end, active = calendarPeriod(b, now)
	if !active {
		return start, end, false
	}

	if b.TimePeriod != nil {
		if b.TimePeriod.Start != nil {
			t := time.Unix(int64(*b.TimePeriod.Start), 0).UTC()
			if t.After(now) {
				return time.Time{}, time.Time{}, false
			}

			if t.After(start) {
				start = t
			}
		}

		if b.TimePeriod.End != nil {
			t := time.Unix(int64(*b.TimePeriod.End), 0).UTC()
			if !t.After(now) {
				return time.Time{}, time.Time{}, false
			}

			if t.Before(end) {
				end = t
			}
		}
	}

	return start, end, true
}
