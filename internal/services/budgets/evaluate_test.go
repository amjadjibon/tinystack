package budgets

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestDecimalThresholds(t *testing.T) {
	for _, tc := range []struct {
		name, limit, spend, kind, operator string
		threshold                          float64
		want                               bool
	}{
		{"percentage-below", "100", "79", "PERCENTAGE", "GREATER_THAN", 80, false},
		{"percentage-boundary", "100", "80", "PERCENTAGE", "GREATER_THAN", 80, false},
		{"percentage-above", "100", "80.000000000001", "PERCENTAGE", "GREATER_THAN", 80, true},
		{"less-boundary", "100", "80", "PERCENTAGE", "LESS_THAN", 80, false},
		{"less-credit", "100", "-1", "PERCENTAGE", "LESS_THAN", 80, true},
		{"equality", "0.3", "0.1", "PERCENTAGE", "EQUAL_TO", 100.0 / 3, false},
		{"exact-equality", "0.3", "0.099", "PERCENTAGE", "EQUAL_TO", 33, true},
		{"absolute", "999", "0.3", "ABSOLUTE_VALUE", "EQUAL_TO", 0.3, true},
		{"zero-limit", "0", "0", "PERCENTAGE", "EQUAL_TO", 80, true},
		{
			"large-limit", "999999999999999999999999999999", "999999999999999999999999999998",
			"PERCENTAGE", "LESS_THAN", 100, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &api.Notification{
				Threshold:          new(tc.threshold),
				ThresholdType:      new(tc.kind),
				ComparisonOperator: new(tc.operator),
			}

			threshold := thresholdAmount(n, &api.Spend{Amount: new(tc.limit), Unit: new("USD")})
			if got := breached(n, tc.spend, threshold); got != tc.want {
				t.Fatalf("%s vs %s: %v", tc.spend, threshold, got)
			}
		})
	}
}

func TestEvaluationClaimsRolloverAndRecreation(t *testing.T) {
	store := &memory.Store{}
	s := New(store, delivery.New(), nil)
	defer s.Stop()
	now := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	scope := request.Scope{AccountID: "000000000000"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	name := "once/per-period"
	input := &api.CreateBudgetRequest{
		AccountId: new(scope.AccountID), Budget: &api.Budget{
			BudgetName: new(name), BudgetType: new(budgetCost), TimeUnit: new(timeMonthly),
			BudgetLimit: &api.Spend{Amount: new("0"), Unit: new("USD")}, TimePeriod: &api.TimePeriod{
				Start: new(float64(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).Unix())),
				End:   new(float64(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC).Unix())),
			},
		},
		NotificationsWithSubscribers: []api.NotificationWithSubscribers{{
			Notification: &api.Notification{
				NotificationType: new(
					"ACTUAL",
				),
				ComparisonOperator: new("EQUAL_TO"),
				Threshold:          new(80.0),
				ThresholdType:      new("PERCENTAGE"),
			},
			Subscribers: []api.Subscriber{
				{SubscriptionType: new("EMAIL"), Address: new("a@example.com")},
				{SubscriptionType: new("EMAIL"), Address: new("b@example.com")},
			},
		}},
	}

	if _, err := s.CreateBudget(ctx, input); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.evaluate(ctx, indexKey(scope, name), now); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	count := func(prefix string) int {
		t.Helper()
		var n int
		if err := store.View(ctx, func(tx storage.ReadTx) error { n = len(tx.Keys(prefix)); return nil }); err != nil {
			t.Fatal(err)
		}

		return n
	}

	if count(alertPrefix(scope, name)) != 1 || count(jobIndex) != 2 {
		t.Fatal("concurrent evaluation duplicated a logical alert")
	}

	now = now.AddDate(0, 1, 0)
	got, err := s.DescribeNotificationsForBudget(ctx, &api.DescribeNotificationsForBudgetRequest{
		AccountId: new(scope.AccountID), BudgetName: new(name),
	})
	if err != nil || *got.Notifications[0].NotificationState != "OK" {
		t.Fatal(got, err)
	}

	if err = s.evaluate(ctx, indexKey(scope, name), now); err != nil {
		t.Fatal(err)
	}

	if count(alertPrefix(scope, name)) != 2 || count(jobIndex) != 4 {
		t.Fatal("new month did not re-arm")
	}

	if _, err = s.DeleteBudget(
		ctx,
		&api.DeleteBudgetRequest{AccountId: new(scope.AccountID), BudgetName: new(name)},
	); err != nil {
		t.Fatal(err)
	}

	if count(alertPrefix(scope, name)) != 0 || count(jobIndex) != 0 || count(jobPrefix(scope, name)) != 0 {
		t.Fatal("budget deletion retained alerts or pending jobs")
	}

	if _, err = s.CreateBudget(ctx, input); err != nil {
		t.Fatal(err)
	}

	if err = s.evaluate(ctx, indexKey(scope, name), now); err != nil {
		t.Fatal(err)
	}

	if count(alertPrefix(scope, name)) != 1 || count(jobIndex) != 2 {
		t.Fatal("recreated budget retained old sent marker")
	}
}

func TestPlannedLimitSelection(t *testing.T) {
	now := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC)
	b := &api.Budget{PlannedBudgetLimits: map[string]api.Spend{
		"1759276800": {Amount: new("50"), Unit: new("USD")},
		"1790812800": {Amount: new("100"), Unit: new("USD")},
		"1793491200": {Amount: new("200"), Unit: new("USD")},
	}}
	if got := budgetLimit(b, now); got == nil || *got.Amount != "100" {
		t.Fatal(got)
	}

	if got := budgetLimit(b, now.AddDate(0, 1, 0)); got == nil || *got.Amount != "200" {
		t.Fatal(got)
	}

	if got := budgetLimit(b, now.AddDate(0, 2, 0)); got == nil || *got.Amount != "200" {
		t.Fatal(got)
	}

	if got := budgetLimit(b, time.Unix(0, 0)); got != nil {
		t.Fatal(got)
	}
}

func TestDeliveryCompletionAfterReset(t *testing.T) {
	ctx := context.Background()
	store := &memory.Store{}
	s := New(store, delivery.New(), nil)
	defer s.Stop()
	job := deliveryJob{
		ID:      "job",
		EventID: "event",
		Account: "000000000000",
		Budget:  "reset",
		Status:  jobPending,
		Subscriber: api.Subscriber{
			SubscriptionType: new("SNS"),
			Address:          new("arn:aws:sns:us-east-1:000000000000:topic"),
		},
		Subject: "alert",
		Body:    "alert",
	}
	index := jobIndex + job.Account + "/" + job.ID
	if err := store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(index, []byte(jobKey(&job)))
		return storage.Write(tx, jobKey(&job), job)
	}); err != nil {
		t.Fatal(err)
	}
	s.caller = catalog.NewCaller(catalog.Service{Name: "sns", Operations: map[string]catalog.Operation{
		"Publish": {
			NewInput: func() any { return &snsapi.PublishInput{} },
			Invoke: func(callCtx context.Context, _ any) (any, error) {
				// Publishing must run outside the claim transaction, and a reset during
				// delivery must not be overwritten by the acknowledgment.
				return &snsapi.PublishResponse{}, storage.Clear(callCtx, store)
			},
		},
	}})
	if err := s.deliverDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.View(ctx, func(tx storage.ReadTx) error {
		if keys := tx.Keys(""); len(keys) != 0 {
			t.Errorf("completion resurrected state after reset: %v", keys)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationIdentityCanonicalizesZeroAndState(t *testing.T) {
	n := api.Notification{
		NotificationType: new("ACTUAL"), ComparisonOperator: new(equalTo), Threshold: new(0.0),
		ThresholdType: new("PERCENTAGE"), NotificationState: new("OK"),
	}
	other := n
	other.Threshold = new(math.Copysign(0, -1))
	other.NotificationState = new("ALARM")
	if !sameNotification(&n, &other) || notificationID(&n) != notificationID(&other) {
		t.Fatal("sent-marker identity differs from the notification CRUD identity")
	}
}

func TestSentMarkerUsesCalendarPeriod(t *testing.T) {
	now := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC)
	scope := request.Scope{AccountID: "000000000000"}
	b := &api.Budget{BudgetName: new("calendar"), TimeUnit: new(timeMonthly)}
	n := &api.Notification{
		NotificationType: new("ACTUAL"), ComparisonOperator: new(greaterThan),
		ThresholdType: new("PERCENTAGE"), Threshold: new(80.0),
	}
	before := sentKey(scope, b, n, now)
	b.TimePeriod = &api.TimePeriod{Start: new(float64(now.AddDate(0, 0, -1).Unix()))}
	if after := sentKey(scope, b, n, now); before != after {
		t.Fatal("active-date edit re-armed the same calendar period")
	}
	b.TimeUnit = new(timeQuarterly)
	if after := sentKey(scope, b, n, now); before == after {
		t.Fatal("different recurring periods share a sent marker")
	}
}
