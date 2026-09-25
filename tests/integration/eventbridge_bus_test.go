package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// receiveBody returns the body of the one message a queue holds.
func receiveBody(t *testing.T, c *sqs.Client, queueURL string) string {
	t.Helper()
	out, err := c.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl:        &queueURL,
		WaitTimeSeconds: 2,
	})
	if err != nil || len(out.Messages) != 1 {
		t.Fatalf("%+v %v", out, err)
	}

	_, err = c.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
		QueueUrl:      &queueURL,
		ReceiptHandle: out.Messages[0].ReceiptHandle,
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Messages[0].Body)
}

func TestEventBridgeCustomBuses(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	busARN := "arn:aws:events:us-east-1:000000000000:event-bus/orders"
	created, err := c.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{
		Name:        aws.String("orders"),
		Description: aws.String("order events"),
		Tags:        []ebtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil || aws.ToString(created.EventBusArn) != busARN {
		t.Fatalf("%+v %v", created, err)
	}

	for _, name := range []string{"orders", "default"} {
		_, err = c.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: aws.String(name)})
		code(t, err, "ResourceAlreadyExistsException")
	}

	for _, name := range []string{"a/b", "aws.partner"} {
		_, err = c.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: aws.String(name)})
		code(t, err, "ValidationException")
	}

	for _, ref := range []string{"orders", busARN} {
		described, describeErr := c.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{Name: aws.String(ref)})
		if describeErr != nil || aws.ToString(described.Arn) != busARN ||
			aws.ToString(described.Description) != "order events" || described.CreationTime == nil {
			t.Fatalf("%+v %v", described, describeErr)
		}
	}

	if _, err = c.UpdateEventBus(ctx, &eventbridge.UpdateEventBusInput{
		Name:        aws.String("orders"),
		Description: aws.String("all order events"),
	}); err != nil {
		t.Fatal(err)
	}

	buses, err := c.ListEventBuses(ctx, &eventbridge.ListEventBusesInput{})
	if err != nil || len(buses.EventBuses) != 2 || aws.ToString(buses.EventBuses[0].Name) != "default" ||
		aws.ToString(buses.EventBuses[1].Description) != "all order events" {
		t.Fatalf("%+v %v", buses, err)
	}

	buses, err = c.ListEventBuses(ctx, &eventbridge.ListEventBusesInput{NamePrefix: aws.String("ord")})
	if err != nil || len(buses.EventBuses) != 1 {
		t.Fatalf("%+v %v", buses, err)
	}

	// A rule on the custom bus sees only that bus's events.
	rule, err := c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:         aws.String("paid"),
		EventPattern: aws.String(orderPattern),
		EventBusName: aws.String("orders"),
	})
	ruleARN := "arn:aws:events:us-east-1:000000000000:rule/orders/paid"
	if err != nil || aws.ToString(rule.RuleArn) != ruleARN {
		t.Fatalf("%+v %v", rule, err)
	}

	described, err := c.DescribeRule(ctx, &eventbridge.DescribeRuleInput{
		Name:         aws.String("paid"),
		EventBusName: aws.String(busARN),
	})
	if err != nil || aws.ToString(described.EventBusName) != "orders" || aws.ToString(described.Arn) != ruleARN {
		t.Fatalf("%+v %v", described, err)
	}

	_, err = c.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String("paid")})
	code(t, err, "ResourceNotFoundException")
	rules, err := c.ListRules(ctx, &eventbridge.ListRulesInput{EventBusName: aws.String("orders")})
	if err != nil || len(rules.Rules) != 1 || aws.ToString(rules.Rules[0].EventBusName) != "orders" {
		t.Fatalf("%+v %v", rules, err)
	}

	if rules, err = c.ListRules(ctx, &eventbridge.ListRulesInput{}); err != nil || len(rules.Rules) != 0 {
		t.Fatalf("%+v %v", rules, err)
	}

	queueURL, queue := queueARN(t, q, "paid")
	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:         aws.String("paid"),
		EventBusName: aws.String("orders"),
		Targets:      []ebtypes.Target{{Id: aws.String("q"), Arn: &queue}},
	})
	if err != nil {
		t.Fatal(err)
	}

	entry := func(bus string) ebtypes.PutEventsRequestEntry {
		return ebtypes.PutEventsRequestEntry{
			EventBusName: aws.String(bus),
			Source:       aws.String("shop"),
			DetailType:   aws.String("order"),
			Detail:       aws.String(`{"id":"o-1"}`),
		}
	}

	put, err := c.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{
		entry("default"), entry(busARN), entry("missing"),
	}})
	if err != nil || put.FailedEntryCount != 1 || aws.ToString(put.Entries[2].ErrorCode) != "NotFoundException" ||
		put.Entries[1].EventId == nil {
		t.Fatalf("%+v %v", put, err)
	}

	var delivered struct {
		ID     string          `json:"id"`
		Detail json.RawMessage `json:"detail"`
	}
	if err = json.Unmarshal([]byte(receiveBody(t, q, queueURL)), &delivered); err != nil ||
		delivered.ID != aws.ToString(put.Entries[1].EventId) {
		t.Fatalf("%+v %v", delivered, err)
	}

	// Tags work on custom buses and their rules.
	for _, arn := range []string{busARN, ruleARN} {
		if _, err = c.TagResource(ctx, &eventbridge.TagResourceInput{
			ResourceARN: aws.String(arn),
			Tags:        []ebtypes.Tag{{Key: aws.String("stage"), Value: aws.String("dev")}},
		}); err != nil {
			t.Fatal(err)
		}

		tags, tagErr := c.ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{ResourceARN: aws.String(arn)})
		if tagErr != nil || len(tags.Tags) == 0 {
			t.Fatalf("%s: %+v %v", arn, tags, tagErr)
		}
	}

	// Other accounts cannot see the bus.
	_, err = eventsClient(s, "team-b").DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{
		Name: aws.String("orders"),
	})
	code(t, err, "ResourceNotFoundException")

	// A bus with rules cannot be deleted, and the default bus never can.
	_, err = c.DeleteEventBus(ctx, &eventbridge.DeleteEventBusInput{Name: aws.String("orders")})
	code(t, err, "ValidationException")
	_, err = c.DeleteEventBus(ctx, &eventbridge.DeleteEventBusInput{Name: aws.String("default")})
	code(t, err, "ValidationException")
	if _, err = c.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{
		Rule:         aws.String("paid"),
		EventBusName: aws.String("orders"),
		Ids:          []string{"q"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteRule(ctx, &eventbridge.DeleteRuleInput{
		Name:         aws.String("paid"),
		EventBusName: aws.String("orders"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteEventBus(ctx, &eventbridge.DeleteEventBusInput{Name: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{Name: aws.String("orders")})
	code(t, err, "ResourceNotFoundException")
	_, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:         aws.String("paid"),
		EventPattern: aws.String(orderPattern),
		EventBusName: aws.String("orders"),
	})
	code(t, err, "ResourceNotFoundException")
}

func TestEventBridgeInputTransformer(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	putRule(t, c, "orders", orderPattern)
	jsonURL, jsonQueue := queueARN(t, q, "json")
	textURL, textQueue := queueARN(t, q, "text")
	paths := map[string]string{
		"id":       "$.detail.id",
		"total":    "$.detail.total",
		"first":    "$.detail.items[0].sku",
		"customer": "$.detail.customer",
		"missing":  "$.detail.nope",
	}
	_, err := c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: aws.String("orders"),
		Targets: []ebtypes.Target{
			{Id: aws.String("json"), Arn: &jsonQueue, InputTransformer: &ebtypes.InputTransformer{
				InputPathsMap: paths,
				InputTemplate: aws.String(`{"order": <id>, "total": <total>, "first": "<first>", ` +
					`"customer": <customer>, "missing": <missing>, "rule": "<aws.events.rule-name>", ` +
					`"note": "<customer> said \"hi\"", "kept": "<unknown>"}`),
			}},
			{Id: aws.String("text"), Arn: &textQueue, InputTransformer: &ebtypes.InputTransformer{
				InputPathsMap: map[string]string{"id": "$.detail.id"},
				InputTemplate: aws.String(`"Order <id> was paid"`),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: aws.String("orders")})
	if err != nil || len(listed.Targets) != 2 || listed.Targets[0].InputTransformer == nil ||
		listed.Targets[0].InputTransformer.InputPathsMap["first"] != "$.detail.items[0].sku" {
		t.Fatalf("%+v %v", listed, err)
	}

	_, err = c.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
		Source:     aws.String("shop"),
		DetailType: aws.String("order"),
		Detail: aws.String(`{"id":"o-1","total":42.50,"items":[{"sku":"X-1"}],` +
			`"customer":{"name":"Ada \"A\""}}`),
	}}})
	if err != nil {
		t.Fatal(err)
	}

	body := receiveBody(t, q, jsonURL)
	var got map[string]any
	if err = json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("%s: %v", body, err)
	}

	customer, _ := got["customer"].(map[string]any)
	if got["order"] != "o-1" || got["total"] != 42.5 || got["first"] != "X-1" || customer["name"] != `Ada "A"` ||
		got["missing"] != nil || got["rule"] != "orders" || got["kept"] != "<unknown>" ||
		!strings.HasPrefix(got["note"].(string), `{"name":"Ada \"A\""} said "hi"`) {
		t.Fatalf("%s", body)
	}

	if text := receiveBody(t, q, textURL); text != `"Order o-1 was paid"` {
		t.Fatal(text)
	}

	for name, transformer := range map[string]*ebtypes.InputTransformer{
		"long template":  {InputTemplate: aws.String(strings.Repeat("x", 8193))},
		"reserved key":   {InputPathsMap: map[string]string{"aws.id": "$.id"}, InputTemplate: aws.String("<aws.id>")},
		"bad path":       {InputPathsMap: map[string]string{"id": "$..id"}, InputTemplate: aws.String("<id>")},
		"bad key":        {InputPathsMap: map[string]string{"a b": "$.id"}, InputTemplate: aws.String("<a b>")},
		"with the input": nil,
	} {
		target := ebtypes.Target{Id: aws.String("bad"), Arn: &jsonQueue, InputTransformer: transformer}
		if transformer == nil {
			target.InputTransformer = &ebtypes.InputTransformer{InputTemplate: aws.String("{}")}
			target.Input = aws.String("{}")
		}

		_, err = c.PutTargets(
			ctx,
			&eventbridge.PutTargetsInput{Rule: aws.String("orders"), Targets: []ebtypes.Target{target}},
		)
		if err == nil {
			t.Fatal(name, "accepted")
		}

		code(t, err, "ValidationException")
	}
}
