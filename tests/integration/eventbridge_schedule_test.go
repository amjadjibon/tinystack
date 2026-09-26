package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// TestEventBridgeScheduledRules covers the scheduled rule API. The scheduler
// itself, whose shortest rate is a minute, is tested with a controlled clock
// in the eventbridge package.
func TestEventBridgeScheduledRules(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	queues := sqsClient(s, "test")
	ctx := context.Background()
	queueURL, queueARN := queueARN(t, queues, "ticks")

	_, err := c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: aws.String("nightly"), ScheduleExpression: aws.String("cron(0 3 * * ? *)"),
	})
	if err != nil {
		t.Fatal(err)
	}

	rule, err := c.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String("nightly")})
	if err != nil || aws.ToString(rule.ScheduleExpression) != "cron(0 3 * * ? *)" || rule.EventPattern != nil ||
		rule.State != ebtypes.RuleStateEnabled {
		t.Fatalf("got %+v, %v", rule, err)
	}

	listed, err := c.ListRules(ctx, &eventbridge.ListRulesInput{})
	if err != nil || len(listed.Rules) != 1 || aws.ToString(listed.Rules[0].ScheduleExpression) == "" {
		t.Fatalf("got %+v, %v", listed, err)
	}

	// A schedule-only rule matches no events.
	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: aws.String("nightly"), Targets: []ebtypes.Target{{Id: aws.String("q"), Arn: aws.String(queueARN)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	sent, err := c.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
		Source: aws.String("shop"), DetailType: aws.String("order"), Detail: aws.String(`{}`),
	}}})
	if err != nil || sent.FailedEntryCount != 0 || receiveOne(t, queues, queueURL) != "" {
		t.Fatalf("got %+v, %v", sent, err)
	}

	// A rule may have both a pattern and a schedule; changing it keeps its targets.
	_, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: aws.String("nightly"), ScheduleExpression: aws.String("rate(1 hour)"),
		EventPattern: aws.String(orderPattern), State: ebtypes.RuleStateDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	rule, err = c.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String("nightly")})
	if err != nil || aws.ToString(rule.ScheduleExpression) != "rate(1 hour)" ||
		aws.ToString(rule.EventPattern) != orderPattern || rule.State != ebtypes.RuleStateDisabled {
		t.Fatalf("got %+v, %v", rule, err)
	}

	for _, expression := range []string{
		"rate(1 minutes)", "rate(0 hours)", "rate(5 weeks)", "cron(0 3 * * * *)", "cron(0 3 * *)",
		"cron(0 3 ? * 6L *)", "every day",
	} {
		_, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
			Name: aws.String("bad"), ScheduleExpression: aws.String(expression),
		})
		code(t, err, "ValidationException")
	}

	// Only the default bus has scheduled rules.
	if _, err = c.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: aws.String("tick"), EventBusName: aws.String("orders"), ScheduleExpression: aws.String("rate(5 minutes)"),
	})
	code(t, err, "ValidationException")
}
