package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/budgets"
	bt "github.com/aws/aws-sdk-go-v2/service/budgets/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/delivery"
)

func budgetAlertFixture(t *testing.T, s *httptest.Server, amounts ...string) {
	t.Helper()
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lines := make([]string, 0, len(amounts))
	for i, amount := range amounts {
		lines = append(lines, fmt.Sprintf(`{"id":"line%d","start":%q,"end":%q,"service":"S3",`+
			`"unblendedCost":%q,"currency":"USD"}`, i, start.Format(time.RFC3339Nano),
			start.Add(time.Nanosecond).Format(time.RFC3339Nano), amount))
	}

	fixtureRequest(t, s, http.MethodPut, "costs/fixtures/alerts", `{"lines":[`+strings.Join(lines, ",")+`]}`, 200)
}

func createBudgetAlert(t *testing.T, c *sdk.Client, name string, n *bt.Notification, subscribers []bt.Subscriber,
	change func(*bt.Budget),
) {
	t.Helper()
	b := &bt.Budget{
		BudgetName: new(name), BudgetType: bt.BudgetTypeCost, TimeUnit: bt.TimeUnitMonthly,
		BudgetLimit: &bt.Spend{Amount: new("100"), Unit: new("USD")},
	}

	if change != nil {
		change(b)
	}

	_, err := c.CreateBudget(context.Background(), &sdk.CreateBudgetInput{
		AccountId: new("000000000000"), Budget: b,
		NotificationsWithSubscribers: []bt.NotificationWithSubscribers{{Notification: n, Subscribers: subscribers}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func budgetWait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("budget evaluation/delivery did not finish within 12 seconds")
}

func budgetAlertState(t *testing.T, c *sdk.Client, name string) bt.NotificationState {
	t.Helper()
	out, err := c.DescribeNotificationsForBudget(context.Background(), &sdk.DescribeNotificationsForBudgetInput{
		AccountId: new("000000000000"), BudgetName: new(name),
	})
	if err != nil || len(out.Notifications) != 1 {
		t.Fatal(out, err)
	}

	return out.Notifications[0].NotificationState
}

func budgetDeliveries(t *testing.T, s *httptest.Server) []delivery.Record {
	t.Helper()
	var out struct {
		Deliveries []delivery.Record `json:"deliveries"`
	}

	if status := internal(t, s, http.MethodGet, "/_tinystack/deliveries", &out); status != 200 {
		t.Fatal(status)
	}

	var records []delivery.Record
	for i := range out.Deliveries {
		r := &out.Deliveries[i]
		if r.Via == delivery.ViaBudgets {
			records = append(records, *r)
		}
	}

	return records
}

func TestBudgetAlertsSNSThresholdAndCredits(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	topics, queues := snsClient(s, "test"), sqsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("budget-alerts")})
	if err != nil {
		t.Fatal(err)
	}

	queueURL, queueARN := queueARN(t, queues, "budget-alerts")
	_, err = topics.Subscribe(
		ctx,
		&sns.SubscribeInput{TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &queueARN},
	)
	if err != nil {
		t.Fatal(err)
	}

	c := budgetsClient(s, "test", "ap-south-1") // The topic is in us-east-1; budgets are global.
	n := &bt.Notification{
		NotificationType: bt.NotificationTypeActual, ComparisonOperator: bt.ComparisonOperatorGreaterThan,
		Threshold: 80, ThresholdType: bt.ThresholdTypePercentage,
	}

	budgetAlertFixture(t, s, "79")
	createBudgetAlert(t, c, "sns-monthly", n, []bt.Subscriber{{
		SubscriptionType: bt.SubscriptionTypeSns,
		Address:          topic.TopicArn,
	}}, nil)
	time.Sleep(2200 * time.Millisecond)
	if state := budgetAlertState(t, c, "sns-monthly"); state != bt.NotificationStateOk {
		t.Fatal(state)
	}

	if len(budgetDeliveries(t, s)) != 0 {
		t.Fatal("delivered below threshold")
	}

	budgetAlertFixture(t, s, "81")
	var receipt *string
	budgetWait(t, func() bool {
		out, receiveErr := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		if len(out.Messages) == 0 {
			return false
		}

		if len(out.Messages) != 1 {
			t.Fatal(out.Messages)
		}

		var envelope struct{ Subject, Message, TopicArn string }
		if decodeErr := json.Unmarshal([]byte(*out.Messages[0].Body), &envelope); decodeErr != nil {
			t.Fatal(decodeErr)
		}

		if envelope.TopicArn != *topic.TopicArn || !strings.HasPrefix(envelope.Subject, "AWS Budgets:") ||
			!strings.Contains(envelope.Message, "ACTUAL Amount: $81") ||
			!strings.Contains(
				envelope.Message,
				"Alert Threshold: > $80.",
			) || !strings.Contains(envelope.Message, "Event ID:") {
			t.Fatal(envelope)
		}

		receipt = out.Messages[0].ReceiptHandle
		return true
	})
	_, err = queues.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: receipt})
	if err != nil {
		t.Fatal(err)
	}

	// A credit drops spend below the rule; a later re-cross must not resend.
	budgetAlertFixture(t, s, "81", "-5")
	time.Sleep(2200 * time.Millisecond)
	if budgetAlertState(t, c, "sns-monthly") != bt.NotificationStateAlarm {
		t.Fatal("marker lost after credit")
	}

	budgetAlertFixture(t, s, "82")
	time.Sleep(2200 * time.Millisecond)
	out, err := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(out.Messages) != 0 || len(budgetDeliveries(t, s)) != 1 {
		t.Fatal(out, err)
	}
}

func TestBudgetAlertsOperatorsEmailAndStoredOnly(t *testing.T) {
	s := server(t)
	c := budgetsClient(s, "test", "eu-west-1")
	budgetAlertFixture(t, s, "80.000000000001")
	email := []bt.Subscriber{{SubscriptionType: bt.SubscriptionTypeEmail, Address: new("budget@example.com")}}
	cases := []struct {
		change        func(*bt.Budget)
		name          string
		operator      bt.ComparisonOperator
		thresholdType bt.ThresholdType
		threshold     float64
		forecast      bool
		fires         bool
	}{
		{
			name: "planned-absolute", operator: bt.ComparisonOperatorGreaterThan,
			thresholdType: bt.ThresholdTypeAbsoluteValue, threshold: 80, fires: true,
			change: func(b *bt.Budget) {
				b.BudgetLimit = nil
				b.PlannedBudgetLimits = map[string]bt.Spend{fmt.Sprint(time.Now().AddDate(0, 1, 0).Unix()): {
					Amount: new("100"), Unit: new("USD"),
				}}
			},
		},
		{
			name: "absolute", operator: bt.ComparisonOperatorGreaterThan, thresholdType: bt.ThresholdTypeAbsoluteValue,
			threshold: 80, fires: true,
		},
		{
			name: "equal", operator: bt.ComparisonOperatorEqualTo, thresholdType: bt.ThresholdTypeAbsoluteValue,
			threshold: 80.000000000001, fires: true,
		},
		{
			name: "less", operator: bt.ComparisonOperatorLessThan, thresholdType: bt.ThresholdTypePercentage,
			threshold: 81, fires: true,
		},
		{name: "greater-boundary", operator: bt.ComparisonOperatorGreaterThan, threshold: 80.000000000001},
		{name: "less-boundary", operator: bt.ComparisonOperatorLessThan, threshold: 80.000000000001},
		{name: "forecast", operator: bt.ComparisonOperatorGreaterThan, threshold: 1, forecast: true},
		{
			name: "daily", operator: bt.ComparisonOperatorGreaterThan, threshold: 1,
			change: func(b *bt.Budget) { b.TimeUnit = bt.TimeUnitDaily },
		},
		{
			name: "usage", operator: bt.ComparisonOperatorGreaterThan, threshold: 1,
			change: func(b *bt.Budget) { b.BudgetType = bt.BudgetTypeUsage },
		},
		{
			name: "currency", operator: bt.ComparisonOperatorGreaterThan, threshold: 1,
			change: func(b *bt.Budget) { b.BudgetLimit.Unit = new("EUR") },
		},
		{
			name: "costtypes", operator: bt.ComparisonOperatorGreaterThan, threshold: 1,
			change: func(b *bt.Budget) {
				//nolint:staticcheck // Verify the retained legacy CostTypes configuration is not evaluated.
				b.CostTypes = &bt.CostTypes{UseBlended: new(true)}
			},
		},
		{
			name: "filter", operator: bt.ComparisonOperatorGreaterThan, threshold: 1,
			change: func(b *bt.Budget) {
				//nolint:staticcheck // Verify unsupported legacy filter dimensions remain stored only.
				b.CostFilters = map[string][]string{"TAG": {"team"}}
			},
		},
	}

	for _, tc := range cases {
		kind := bt.ThresholdTypeAbsoluteValue
		if tc.thresholdType != "" {
			kind = tc.thresholdType
		}

		n := &bt.Notification{
			NotificationType: bt.NotificationTypeActual, ComparisonOperator: tc.operator,
			Threshold: tc.threshold, ThresholdType: kind, NotificationState: bt.NotificationStateAlarm,
		}

		if tc.forecast {
			n.NotificationType = bt.NotificationTypeForecasted
		}

		createBudgetAlert(t, c, tc.name, n, email, tc.change)
	}

	budgetWait(t, func() bool { return len(budgetDeliveries(t, s)) == 4 })
	time.Sleep(1200 * time.Millisecond)
	for _, tc := range cases {
		want := bt.NotificationStateOk
		if tc.fires {
			want = bt.NotificationStateAlarm
		}

		if got := budgetAlertState(t, c, tc.name); got != want {
			t.Fatalf("%s: state %s, want %s", tc.name, got, want)
		}
	}

	records := budgetDeliveries(t, s)
	if len(records) != 4 {
		t.Fatal(records)
	}

	for _, r := range records {
		if r.Target != "budget@example.com" || r.Status != "delivered" {
			t.Fatal(r)
		}
	}

	var messages struct {
		Messages []delivery.Message `json:"messages"`
	}

	if status := internal(
		t,
		s,
		http.MethodGet,
		"/_tinystack/messages?service=budgets&channel=email",
		&messages,
	); status != 200 ||
		len(messages.Messages) != 4 {
		t.Fatal(status, messages)
	}

	for _, m := range messages.Messages {
		if m.Service != "budgets" || m.Account != "000000000000" || m.Region != "" || m.ID == "" || m.Subject == "" ||
			m.Body == "" {
			t.Fatal(m)
		}
	}

	// DELETE is observed without dirty flags and makes a new LESS_THAN rule fire at zero.
	fixtureRequest(t, s, http.MethodDelete, "costs/fixtures/alerts", "", 204)
	createBudgetAlert(t, c, "deleted-fixture", &bt.Notification{
		NotificationType:   bt.NotificationTypeActual,
		ComparisonOperator: bt.ComparisonOperatorLessThan, Threshold: 1, ThresholdType: bt.ThresholdTypeAbsoluteValue,
	}, email, nil)
	budgetWait(t, func() bool { return budgetAlertState(t, c, "deleted-fixture") == bt.NotificationStateAlarm })
}

func TestBudgetAlertsFailuresRetryAndReset(t *testing.T) {
	s := server(t)
	c := budgetsClient(s, "test", "us-west-2")
	budgetAlertFixture(t, s, "81")
	n := &bt.Notification{
		NotificationType: bt.NotificationTypeActual, ComparisonOperator: bt.ComparisonOperatorGreaterThan,
		Threshold: 80, ThresholdType: bt.ThresholdTypePercentage,
	}

	for _, name := range []string{"missing", "foreign"} {
		account := "000000000000"
		if name == "foreign" {
			account = "111111111111"
		}

		createBudgetAlert(t, c, name, n, []bt.Subscriber{{
			SubscriptionType: bt.SubscriptionTypeSns,
			Address:          new("arn:aws:sns:us-east-1:" + account + ":" + name),
		}}, nil)
	}

	budgetWait(t, func() bool { return len(budgetDeliveries(t, s)) >= 2 })
	for _, r := range budgetDeliveries(t, s) {
		if r.Status != "failed" || r.Error == "" {
			t.Fatal(r)
		}
	}

	// A missing topic created before a retry lets that persisted job succeed.
	topics := snsClient(s, "test")
	if _, err := topics.CreateTopic(context.Background(), &sns.CreateTopicInput{Name: new("missing")}); err != nil {
		t.Fatal(err)
	}

	budgetWait(t, func() bool {
		for _, r := range budgetDeliveries(t, s) {
			if strings.HasSuffix(r.Target, ":missing") && r.Status == "delivered" {
				return true
			}
		}

		return false
	})
	budgetWait(t, func() bool {
		count := 0
		for _, r := range budgetDeliveries(t, s) {
			if strings.HasSuffix(r.Target, ":foreign") {
				count++
			}
		}

		return count == 4
	})
	time.Sleep(2200 * time.Millisecond)
	foreign := 0
	for _, r := range budgetDeliveries(t, s) {
		if strings.HasSuffix(r.Target, ":foreign") {
			foreign++
		}
	}

	if foreign != 4 {
		t.Fatalf("unbounded retries: %d", foreign)
	}

	// Leave a pending retry, reset, and recreate its target. No old delivery may return.
	createBudgetAlert(t, c, "pending-reset", n, []bt.Subscriber{{
		SubscriptionType: bt.SubscriptionTypeSns,
		Address:          new("arn:aws:sns:us-east-1:000000000000:pending-reset"),
	}}, nil)
	budgetWait(t, func() bool {
		for _, r := range budgetDeliveries(t, s) {
			if strings.HasSuffix(r.Target, ":pending-reset") {
				return true
			}
		}

		return false
	})
	fixtureRequest(t, s, http.MethodPost, "reset", "", 200)
	if _, err := topics.CreateTopic(
		context.Background(),
		&sns.CreateTopicInput{Name: new("pending-reset")},
	); err != nil {
		t.Fatal(err)
	}

	time.Sleep(2200 * time.Millisecond)
	if records := budgetDeliveries(t, s); len(records) != 0 {
		t.Fatal("reset retained jobs", records)
	}

	budgetAlertFixture(t, s, "81")
	createBudgetAlert(t, c, "missing", n, []bt.Subscriber{{
		SubscriptionType: bt.SubscriptionTypeEmail,
		Address:          new("reset@example.com"),
	}}, nil)
	budgetWait(t, func() bool { return len(budgetDeliveries(t, s)) == 1 })
}

func TestBudgetAlertsPersistentRestart(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() {
		if stop != nil {
			stop()
		}
	})
	c := budgetsClient(s, "test", "us-east-1")
	budgetAlertFixture(t, s, "81")
	createBudgetAlert(t, c, "persisted", &bt.Notification{
		NotificationType:   bt.NotificationTypeActual,
		ComparisonOperator: bt.ComparisonOperatorGreaterThan, Threshold: 80, ThresholdType: bt.ThresholdTypePercentage,
	},
		[]bt.Subscriber{{SubscriptionType: bt.SubscriptionTypeEmail, Address: new("persist@example.com")}}, nil)
	budgetWait(t, func() bool { return len(budgetDeliveries(t, s)) == 1 })
	stop()
	stop = nil
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = budgetsClient(s, "test", "ap-south-1")
	time.Sleep(2200 * time.Millisecond)
	if budgetAlertState(t, c, "persisted") != bt.NotificationStateAlarm || len(budgetDeliveries(t, s)) != 0 {
		t.Fatal("restart resent a logical alert")
	}
}

func TestBudgetAlertsDefinitionChanges(t *testing.T) {
	s := server(t)
	c := budgetsClient(s, "test", "eu-central-1")
	ctx := context.Background()
	budgetAlertFixture(t, s, "81")
	n := &bt.Notification{
		NotificationType: bt.NotificationTypeActual, ComparisonOperator: bt.ComparisonOperatorGreaterThan,
		Threshold: 80, ThresholdType: bt.ThresholdTypePercentage,
	}

	old := bt.Subscriber{SubscriptionType: bt.SubscriptionTypeEmail, Address: new("old@example.com")}
	createBudgetAlert(
		t,
		c,
		"edited",
		n,
		[]bt.Subscriber{old},
		func(b *bt.Budget) { b.BudgetLimit.Amount = new("1000") },
	)
	time.Sleep(2200 * time.Millisecond)
	if len(budgetDeliveries(t, s)) != 0 {
		t.Fatal("fired before budget update")
	}

	_, err := c.UpdateSubscriber(ctx, &sdk.UpdateSubscriberInput{
		AccountId: new("000000000000"), BudgetName: new("edited"),
		Notification: n, OldSubscriber: &old, NewSubscriber: &bt.Subscriber{
			SubscriptionType: bt.SubscriptionTypeEmail,
			Address:          new("new@example.com"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateBudget(ctx, &sdk.UpdateBudgetInput{AccountId: new("000000000000"), NewBudget: &bt.Budget{
		BudgetName: new("edited"), BudgetType: bt.BudgetTypeCost, TimeUnit: bt.TimeUnitMonthly,
		BudgetLimit: &bt.Spend{Amount: new("100"), Unit: new("USD")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	budgetWait(t, func() bool { return len(budgetDeliveries(t, s)) == 1 })
	if records := budgetDeliveries(t, s); records[0].Target != "new@example.com" {
		t.Fatal(records)
	}

	newer := *n
	newer.Threshold = 90
	_, err = c.UpdateNotification(ctx, &sdk.UpdateNotificationInput{
		AccountId: new("000000000000"), BudgetName: new("edited"),
		OldNotification: n, NewNotification: &newer,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := budgetAlertState(t, c, "edited"); got != bt.NotificationStateOk {
		t.Fatal("new rule inherited old marker", got)
	}

	budgetAlertFixture(t, s, "91")
	budgetWait(t, func() bool { return len(budgetDeliveries(t, s)) == 2 })
}
