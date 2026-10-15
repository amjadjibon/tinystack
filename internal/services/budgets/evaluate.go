package budgets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const budgetIndex = "aws/budgets/index/"

func indexKey(scope request.Scope, name string) string {
	return budgetIndex + scope.AccountID + "/" + base64.RawURLEncoding.EncodeToString([]byte(name))
}

func alertPrefix(scope request.Scope, name string) string {
	return scope.Key(serviceName, "sent", base64.RawURLEncoding.EncodeToString([]byte(name))+"/")
}

// The modeled threshold is a double. Its shortest decimal representation, also
// used for identity, enters exact rational arithmetic; money never becomes a float.
func notificationID(n *api.Notification) string {
	threshold := common.Value(n.Threshold)
	if threshold == 0 {
		threshold = 0 // Canonicalize negative zero, which phase A's notification identity treats as zero.
	}

	value := fmt.Sprintf("%s/%s/%s/%s", common.Value(n.NotificationType), common.Value(n.ComparisonOperator),
		strconv.FormatFloat(threshold, 'f', -1, 64), common.Value(n.ThresholdType))
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func sentKey(scope request.Scope, b *api.Budget, n *api.Notification, now time.Time) string {
	start, _, _ := calendarPeriod(b, now)
	return alertPrefix(scope, *b.BudgetName) + notificationID(n) + "/" + common.Value(b.TimeUnit) + "/" +
		strconv.FormatInt(start.Unix(), 10)
}

func budgetLimit(b *api.Budget, now time.Time) *api.Spend {
	if b.BudgetLimit != nil {
		return b.BudgetLimit
	}

	var selected *api.Spend
	var latest int64 = -1
	for epoch, limit := range b.PlannedBudgetLimits {
		at, err := strconv.ParseInt(epoch, 10, 64)
		if err == nil && at <= now.Unix() && at > latest {
			selected, latest = new(limit), at
		}
	}

	return selected
}

func thresholdAmount(n *api.Notification, limit *api.Spend) *big.Rat {
	threshold, _ := new(big.Rat).SetString(strconv.FormatFloat(*n.Threshold, 'f', -1, 64))
	if common.Value(n.ThresholdType) == thresholdPercentage {
		amount, _ := new(big.Rat).SetString(*limit.Amount)
		threshold.Mul(threshold, amount)
		threshold.Mul(threshold, big.NewRat(1, 100))
	}

	return threshold
}

func breached(n *api.Notification, spend string, threshold *big.Rat) bool {
	actual, _ := new(big.Rat).SetString(spend)
	cmp := actual.Cmp(threshold)
	switch common.Value(n.ComparisonOperator) {
	case greaterThan:
		return cmp > 0
	case lessThan:
		return cmp < 0
	case equalTo:
		return cmp == 0
	default:
		return false
	}
}

// visit re-evaluates each indexed budget every second, so fixture replacement,
// deletion, and definition/subscriber changes need no separate dirty-state protocol.
func (s *Service) visit(ctx context.Context) {
	var keys []string
	err := s.store.View(ctx, func(tx storage.ReadTx) error { keys = tx.Keys(budgetIndex); return nil })
	if err == nil {
		for _, k := range keys {
			if err = s.evaluate(ctx, k, s.now().UTC()); err != nil {
				break
			}
		}
	}

	if err == nil {
		err = s.deliverDue(ctx, s.now().UTC())
	}

	if err != nil && ctx.Err() == nil {
		s.logger.Warn("evaluate budget alerts", "error", err)
	}
}

func (s *Service) evaluate(ctx context.Context, index string, now time.Time) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, exists := tx.Get(index)
		if !exists {
			return nil
		}

		r, exists, err := storage.Read[record](tx, string(k))
		if err != nil {
			return err
		}

		if !exists {
			tx.Delete(index)
			return nil
		}

		// Read definitions, fixture revision, marker and new jobs in one snapshot.
		account, _, _ := strings.Cut(string(k), "/")
		scope := request.Scope{AccountID: account}
		fixtures, err := storage.Records[costdata.Fixture](tx, scope.Prefix("costdata", "fixture"))
		if err != nil {
			return err
		}

		if err := actualSpendAt(&r.Budget, costdata.Lines(fixtures), now); err != nil {
			return err
		}

		if r.Budget.CalculatedSpend == nil {
			return nil
		}

		limit := budgetLimit(&r.Budget, now)

		for _, notification := range r.Notifications {
			n := notification.Notification
			if common.Value(n.NotificationType) != "ACTUAL" ||
				common.Value(n.ThresholdType) == thresholdPercentage && limit == nil {
				continue
			}

			marker := sentKey(scope, &r.Budget, n, now)
			if _, sent := tx.Get(marker); sent {
				continue
			}

			threshold := thresholdAmount(n, limit)
			if !breached(n, *r.Budget.CalculatedSpend.ActualSpend.Amount, threshold) {
				continue
			}

			eventID := common.RandomID(32)
			if err := storage.Write(tx, marker, eventID); err != nil {
				return err
			}

			subject, body := alertMessage(scope, &r.Budget, n, limit, threshold, now, eventID)
			for _, sub := range notification.Subscribers {
				job := deliveryJob{
					ID: common.RandomID(32), EventID: eventID, Source: arn(account, *r.Budget.BudgetName),
					Account: account, Budget: *r.Budget.BudgetName, Subscriber: sub, Subject: subject, Body: body,
					Status: jobPending, Next: now.UnixMilli(),
				}

				if err := storage.Write(tx, jobKey(&job), job); err != nil {
					return err
				}

				tx.Put(jobIndex+account+"/"+job.ID, []byte(jobKey(&job)))
			}
		}

		return nil
	})
}

// notificationStates derives state from durable markers, including immediately
// after rollover or an API update carrying a caller-supplied NotificationState.
func (s *Service) notificationStates(ctx context.Context, account string, r *record) error {
	now := s.now().UTC()
	return s.store.View(ctx, func(tx storage.ReadTx) error {
		setNotificationStates(tx, request.Scope{AccountID: account}, r, now)
		return nil
	})
}

func setNotificationStates(tx storage.ReadTx, scope request.Scope, r *record, now time.Time) {
	for _, v := range r.Notifications {
		state := "OK"
		if _, _, active := budgetPeriod(&r.Budget, now); active {
			if _, sent := tx.Get(sentKey(scope, &r.Budget, v.Notification, now)); sent {
				state = "ALARM"
			}
		}

		v.Notification.NotificationState = new(state)
	}
}
