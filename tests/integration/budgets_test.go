package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	sdk "github.com/aws/aws-sdk-go-v2/service/budgets"
	"github.com/aws/aws-sdk-go-v2/service/budgets/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func budgetsClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func wantBudgetsError(t *testing.T, err error, code string) {
	t.Helper()
	var e smithy.APIError
	if !errors.As(err, &e) || e.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestBudgetsSDKLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := budgetsClient(s, "test", "us-east-1")
	account := new("000000000000")
	name := new("monthly/team")
	b := &types.Budget{
		BudgetName: name, BudgetType: types.BudgetTypeCost, TimeUnit: types.TimeUnitMonthly,
		BudgetLimit: &types.Spend{Amount: new("100.25"), Unit: new("USD")},
	}
	n := &types.Notification{
		NotificationType: types.NotificationTypeActual, ComparisonOperator: types.ComparisonOperatorGreaterThan,
		Threshold: 80.0, ThresholdType: types.ThresholdTypePercentage,
	}
	sub := types.Subscriber{SubscriptionType: types.SubscriptionTypeEmail, Address: new("first@example.com")}
	_, err := c.CreateBudget(ctx, &sdk.CreateBudgetInput{
		AccountId: account,
		Budget:    b,
		NotificationsWithSubscribers: []types.NotificationWithSubscribers{
			{Notification: n, Subscribers: []types.Subscriber{sub}},
		},
		ResourceTags: []types.ResourceTag{{Key: new("team"), Value: new("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateBudget(ctx, &sdk.CreateBudgetInput{AccountId: account, Budget: b})
	wantBudgetsError(t, err, "DuplicateRecordException")
	otherRegion := budgetsClient(s, "test", "eu-west-1")
	got, err := otherRegion.DescribeBudget(ctx, &sdk.DescribeBudgetInput{AccountId: account, BudgetName: name})
	if err != nil || got.Budget.CalculatedSpend != nil || aws.ToString(got.Budget.BudgetLimit.Amount) != "100.25" {
		t.Fatal(got, err)
	}
	_, err = budgetsClient(
		s,
		"111111111111",
		"us-east-1",
	).DescribeBudget(ctx, &sdk.DescribeBudgetInput{AccountId: account, BudgetName: name})
	wantBudgetsError(t, err, "AccessDeniedException")
	_, err = c.DescribeBudget(ctx, &sdk.DescribeBudgetInput{AccountId: new("111111111111"), BudgetName: name})
	wantBudgetsError(t, err, "AccessDeniedException")
	b.BudgetLimit.Amount = new("200")
	_, err = c.UpdateBudget(ctx, &sdk.UpdateBudgetInput{AccountId: account, NewBudget: b})
	if err != nil {
		t.Fatal(err)
	}
	n.NotificationState = types.NotificationStateAlarm // State is excluded from notification identity.
	newer := *n
	newer.Threshold = 90.0
	_, err = c.UpdateNotification(
		ctx,
		&sdk.UpdateNotificationInput{AccountId: account, BudgetName: name, OldNotification: n, NewNotification: &newer},
	)
	if err != nil {
		t.Fatal(err)
	}
	second := types.Subscriber{SubscriptionType: types.SubscriptionTypeEmail, Address: new("second@example.com")}
	_, err = c.CreateSubscriber(
		ctx,
		&sdk.CreateSubscriberInput{AccountId: account, BudgetName: name, Notification: &newer, Subscriber: &second},
	)
	if err != nil {
		t.Fatal(err)
	}
	third := second
	third.Address = new("third@example.com")
	_, err = c.UpdateSubscriber(
		ctx,
		&sdk.UpdateSubscriberInput{
			AccountId:     account,
			BudgetName:    name,
			Notification:  &newer,
			OldSubscriber: &second,
			NewSubscriber: &third,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	subs, err := c.DescribeSubscribersForNotification(
		ctx,
		&sdk.DescribeSubscribersForNotificationInput{
			AccountId:    account,
			BudgetName:   name,
			Notification: &newer,
			MaxResults:   new(int32(1)),
		},
	)
	if err != nil || len(subs.Subscribers) != 1 || subs.NextToken == nil {
		t.Fatal(subs, err)
	}
	next, err := c.DescribeSubscribersForNotification(
		ctx,
		&sdk.DescribeSubscribersForNotificationInput{
			AccountId:    account,
			BudgetName:   name,
			Notification: &newer,
			NextToken:    subs.NextToken,
		},
	)
	if err != nil || len(next.Subscribers) != 1 || aws.ToString(next.Subscribers[0].Address) != "third@example.com" {
		t.Fatal(next, err)
	}
	_, err = c.DeleteSubscriber(
		ctx,
		&sdk.DeleteSubscriberInput{AccountId: account, BudgetName: name, Notification: &newer, Subscriber: &third},
	)
	if err != nil {
		t.Fatal(err)
	}
	extra := *n
	extra.Threshold = 50.0
	_, err = c.CreateNotification(
		ctx,
		&sdk.CreateNotificationInput{
			AccountId:    account,
			BudgetName:   name,
			Notification: &extra,
			Subscribers:  []types.Subscriber{sub},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := c.DescribeNotificationsForBudget(
		ctx,
		&sdk.DescribeNotificationsForBudgetInput{AccountId: account, BudgetName: name, MaxResults: new(int32(1))},
	)
	if err != nil || len(notifications.Notifications) != 1 || notifications.NextToken == nil {
		t.Fatal(notifications, err)
	}
	_, err = c.DeleteNotification(
		ctx,
		&sdk.DeleteNotificationInput{AccountId: account, BudgetName: name, Notification: &extra},
	)
	if err != nil {
		t.Fatal(err)
	}
	resource := new("arn:aws:budgets::000000000000:budget/monthly/team")
	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceARN:  resource,
			ResourceTags: []types.ResourceTag{{Key: new("env"), Value: new("test")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: resource})
	if err != nil || len(tags.ResourceTags) != 2 {
		t.Fatal(tags, err)
	}
	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceARN: resource, ResourceTagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.DescribeBudgets(ctx, &sdk.DescribeBudgetsInput{AccountId: account})
	if err != nil || len(all.Budgets) != 1 {
		t.Fatal(all, err)
	}
	_, err = c.DeleteBudget(ctx, &sdk.DeleteBudgetInput{AccountId: account, BudgetName: name})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DescribeBudget(ctx, &sdk.DescribeBudgetInput{AccountId: account, BudgetName: name})
	wantBudgetsError(t, err, "NotFoundException")
	_, err = c.DescribeBudgetPerformanceHistory(
		ctx,
		&sdk.DescribeBudgetPerformanceHistoryInput{AccountId: account, BudgetName: name},
	)
	wantBudgetsError(t, err, "UnknownOperationException")
}

func TestBudgetsValidationAndPagination(t *testing.T) {
	c := budgetsClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	account := new("000000000000")
	b := &types.Budget{
		BudgetName: new("a"), BudgetType: types.BudgetTypeCost, TimeUnit: types.TimeUnitMonthly,
		BudgetLimit: &types.Spend{Amount: new("100"), Unit: new("USD")},
	}
	for _, name := range []string{"a", "b"} {
		b.BudgetName = new(name)
		if _, err := c.CreateBudget(ctx, &sdk.CreateBudgetInput{AccountId: account, Budget: b}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.DescribeBudgets(ctx, &sdk.DescribeBudgetsInput{AccountId: account, MaxResults: new(int32(1))})
	if err != nil || len(page.Budgets) != 1 || page.NextToken == nil {
		t.Fatal(page, err)
	}
	next, err := c.DescribeBudgets(ctx, &sdk.DescribeBudgetsInput{AccountId: account, NextToken: page.NextToken})
	if err != nil || len(next.Budgets) != 1 ||
		aws.ToString(next.Budgets[0].BudgetName) == aws.ToString(page.Budgets[0].BudgetName) {
		t.Fatal(next, err)
	}
	_, err = c.DescribeBudgets(ctx, &sdk.DescribeBudgetsInput{AccountId: account, NextToken: new("!")})
	wantBudgetsError(t, err, "InvalidNextTokenException")
	b.BudgetName = new("bad:name")
	_, err = c.CreateBudget(ctx, &sdk.CreateBudgetInput{AccountId: account, Budget: b})
	wantBudgetsError(t, err, "InvalidParameterException")
	b.BudgetName = new("unsupported")
	b.AutoAdjustData = &types.AutoAdjustData{AutoAdjustType: types.AutoAdjustTypeForecast}
	_, err = c.CreateBudget(ctx, &sdk.CreateBudgetInput{AccountId: account, Budget: b})
	wantBudgetsError(t, err, "ValidationException")
	n := &types.Notification{
		NotificationType: types.NotificationTypeActual, ComparisonOperator: types.ComparisonOperatorGreaterThan,
		Threshold: 10.0,
	}
	sub := types.Subscriber{SubscriptionType: types.SubscriptionTypeEmail, Address: new("test@example.com")}
	for i := range 5 {
		n.Threshold = float64(i)
		_, err = c.CreateNotification(
			ctx,
			&sdk.CreateNotificationInput{
				AccountId:    account,
				BudgetName:   new("a"),
				Notification: n,
				Subscribers:  []types.Subscriber{sub},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	n.Threshold = 99.0
	_, err = c.CreateNotification(
		ctx,
		&sdk.CreateNotificationInput{
			AccountId:    account,
			BudgetName:   new("a"),
			Notification: n,
			Subscribers:  []types.Subscriber{sub},
		},
	)
	wantBudgetsError(t, err, "CreationLimitExceededException")
}

func TestBudgetsPersistenceAndSubscriberLimits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := budgetsClient(s, "test", "us-east-1")
	account := new("000000000000")
	name := new("persistent")
	n := &types.Notification{
		NotificationType:   types.NotificationTypeActual,
		ComparisonOperator: types.ComparisonOperatorGreaterThan,
		Threshold:          80,
	}
	sns := types.Subscriber{
		SubscriptionType: types.SubscriptionTypeSns,
		Address:          new("arn:aws:sns:us-east-1:000000000000:topic"),
	}
	_, err := c.CreateBudget(ctx, &sdk.CreateBudgetInput{
		AccountId: account,
		Budget: &types.Budget{
			BudgetName: name, BudgetType: types.BudgetTypeCost, TimeUnit: types.TimeUnitMonthly,
			BudgetLimit: &types.Spend{Amount: new("100"), Unit: new("USD")},
		},
		NotificationsWithSubscribers: []types.NotificationWithSubscribers{
			{Notification: n, Subscribers: []types.Subscriber{sns}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	extra := sns
	extra.Address = new("arn:aws:sns:us-east-1:000000000000:other")
	_, err = c.CreateSubscriber(
		ctx,
		&sdk.CreateSubscriberInput{AccountId: account, BudgetName: name, Notification: n, Subscriber: &extra},
	)
	wantBudgetsError(t, err, "CreationLimitExceededException")
	for i := range 10 {
		sub := types.Subscriber{
			SubscriptionType: types.SubscriptionTypeEmail,
			Address:          new(fmt.Sprintf("person%d@example.com", i)),
		}
		_, err = c.CreateSubscriber(
			ctx,
			&sdk.CreateSubscriberInput{AccountId: account, BudgetName: name, Notification: n, Subscriber: &sub},
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	sub := types.Subscriber{SubscriptionType: types.SubscriptionTypeEmail, Address: new("extra@example.com")}
	_, err = c.CreateSubscriber(
		ctx,
		&sdk.CreateSubscriberInput{AccountId: account, BudgetName: name, Notification: n, Subscriber: &sub},
	)
	wantBudgetsError(t, err, "CreationLimitExceededException")
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = budgetsClient(s, "test", "ap-southeast-1")
	got, err := c.DescribeSubscribersForNotification(
		ctx,
		&sdk.DescribeSubscribersForNotificationInput{AccountId: account, BudgetName: name, Notification: n},
	)
	if err != nil || len(got.Subscribers) != 11 {
		t.Fatal(got, err)
	}
	isolated, err := budgetsClient(
		s,
		"111111111111",
		"us-east-1",
	).DescribeBudgets(ctx, &sdk.DescribeBudgetsInput{AccountId: new("111111111111")})
	if err != nil || len(isolated.Budgets) != 0 {
		t.Fatal(isolated, err)
	}
}

func TestBudgetsVerifiedSigningRegionsAndInventory(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = "verify"
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	c := budgetsClient(s, "test", "us-east-1")
	_, err := c.CreateBudget(ctx, &sdk.CreateBudgetInput{
		AccountId: new("000000000000"),
		Budget: &types.Budget{
			BudgetName: new("verified"), BudgetType: types.BudgetTypeCost, TimeUnit: types.TimeUnitMonthly,
			BudgetLimit: &types.Spend{Amount: new("10"), Unit: new("USD")},
		},
		ResourceTags: []types.ResourceTag{{Key: new("team"), Value: new("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"AccountId":"000000000000","BudgetName":"verified"}`
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	r.Header.Set("X-Amz-Target", "AWSBudgetServiceGateway.DescribeBudget")
	sum := sha256.Sum256([]byte(body))
	err = v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, r,
		hex.EncodeToString(sum[:]), "budgets", "eu-west-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(data), "verified") {
		t.Fatal(response.StatusCode, string(data), err)
	}
	tagsClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	arn := new("arn:aws:budgets::000000000000:budget/verified")
	tagged, err := tagsClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*arn}, Tags: map[string]string{"env": "test"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatal(tagged, err)
	}
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: arn})
	if err != nil || len(tags.ResourceTags) != 2 {
		t.Fatal(tags, err)
	}
}
