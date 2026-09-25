package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func eventsClient(s *httptest.Server, key string) *eventbridge.Client {
	return eventbridge.New(
		eventbridge.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(s.URL),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

const orderPattern = `{"source":["shop"],"detail-type":["order"]}`

func putRule(t *testing.T, c *eventbridge.Client, name, pattern string) string {
	t.Helper()
	out, err := c.PutRule(context.Background(), &eventbridge.PutRuleInput{
		Name:         aws.String(name),
		EventPattern: aws.String(pattern),
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.RuleArn)
}

func TestEventBridgeRules(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	arn := putRule(t, c, "orders", orderPattern)
	if arn != "arn:aws:events:us-east-1:000000000000:rule/orders" {
		t.Fatal(arn)
	}

	rule, err := c.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String("orders")})
	if err != nil {
		t.Fatal(err)
	}

	if aws.ToString(rule.EventPattern) != orderPattern || rule.State != ebtypes.RuleStateEnabled ||
		aws.ToString(rule.EventBusName) != "default" || aws.ToString(rule.Arn) != arn {
		t.Fatalf("%+v", rule)
	}

	// PutRule updates an existing rule in place.
	_, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:         aws.String("orders"),
		EventPattern: aws.String(orderPattern),
		State:        ebtypes.RuleStateDisabled,
		Description:  aws.String("all orders"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.EnableRule(ctx, &eventbridge.EnableRuleInput{Name: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	rule, err = c.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String("orders")})
	if err != nil || rule.State != ebtypes.RuleStateEnabled || aws.ToString(rule.Description) != "all orders" {
		t.Fatalf("%+v %v", rule, err)
	}

	for _, name := range []string{"payments", "refunds"} {
		putRule(t, c, name, `{"source":["bank"]}`)
	}

	var names []string
	var token *string
	for {
		page, listErr := c.ListRules(ctx, &eventbridge.ListRulesInput{Limit: aws.Int32(2), NextToken: token})
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, r := range page.Rules {
			names = append(names, aws.ToString(r.Name))
		}

		if token = page.NextToken; token == nil {
			break
		}
	}

	if strings.Join(names, ",") != "orders,payments,refunds" {
		t.Fatal(names)
	}

	prefixed, err := c.ListRules(ctx, &eventbridge.ListRulesInput{NamePrefix: aws.String("pay")})
	if err != nil || len(prefixed.Rules) != 1 {
		t.Fatalf("%+v %v", prefixed, err)
	}

	queues := sqsClient(s, "test")
	_, queue := queueARN(t, queues, "rule-target")
	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:    aws.String("orders"),
		Targets: []ebtypes.Target{{Id: aws.String("queue"), Arn: aws.String(queue)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	targets, err := c.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: aws.String("orders")})
	if err != nil || len(targets.Targets) != 1 || aws.ToString(targets.Targets[0].Arn) != queue {
		t.Fatalf("%+v %v", targets, err)
	}

	_, err = c.DeleteRule(ctx, &eventbridge.DeleteRuleInput{Name: aws.String("orders")})
	code(t, err, "ValidationException")
	_, err = c.RemoveTargets(ctx, &eventbridge.RemoveTargetsInput{Rule: aws.String("orders"), Ids: []string{"queue"}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteRule(ctx, &eventbridge.DeleteRuleInput{Name: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeRule(ctx, &eventbridge.DescribeRuleInput{Name: aws.String("orders")})
	code(t, err, "ResourceNotFoundException")
	// Deleting a missing rule succeeds, as in EventBridge.
	if _, err = c.DeleteRule(ctx, &eventbridge.DeleteRuleInput{Name: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	// Rules are scoped to the caller's account.
	other, err := eventsClient(s, "team-b").ListRules(ctx, &eventbridge.ListRulesInput{})
	if err != nil || len(other.Rules) != 0 {
		t.Fatalf("%+v %v", other, err)
	}
}

func TestEventBridgeValidation(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	putRule(t, c, "orders", orderPattern)
	for pattern, want := range map[string]string{
		`not json`:                          "InvalidEventPatternException",
		`{}`:                                "InvalidEventPatternException",
		`{"source":"shop"}`:                 "InvalidEventPatternException",
		`{"source":[]}`:                     "InvalidEventPatternException",
		`{"source":[{"bogus":"x"}]}`:        "InvalidEventPatternException",
		`{"source":[{"cidr":"10.0/8"}]}`:    "InvalidEventPatternException",
		`{"n":[{"numeric":[">",1,">",2]}]}`: "InvalidEventPatternException",
		`{"$or":[{"a":["x"]}]}`:             "InvalidEventPatternException",
		``:                                  "ValidationException",
	} {
		_, err := c.PutRule(ctx, &eventbridge.PutRuleInput{Name: aws.String("bad"), EventPattern: aws.String(pattern)})
		code(t, err, want)
	}

	// Scheduled rules are not implemented, so the unmodeled field fails.
	_, err := c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:               aws.String("cron"),
		ScheduleExpression: aws.String("rate(5 minutes)"),
	})
	code(t, err, "ValidationException")
	_, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:         aws.String("custom"),
		EventPattern: aws.String(orderPattern),
		EventBusName: aws.String("custom-bus"),
	})
	code(t, err, "ResourceNotFoundException")
	_, err = c.EnableRule(ctx, &eventbridge.EnableRuleInput{Name: aws.String("missing")})
	code(t, err, "ResourceNotFoundException")

	queue := "arn:aws:sqs:us-east-1:000000000000:q"
	for name, target := range map[string]ebtypes.Target{
		"unsupported service": {Id: aws.String("t"), Arn: aws.String("arn:aws:states:us-east-1:000000000000:x")},
		"other account":       {Id: aws.String("t"), Arn: aws.String("arn:aws:sqs:us-east-1:111111111111:q")},
		"bad arn":             {Id: aws.String("t"), Arn: aws.String("queue")},
		"bad id":              {Id: aws.String("t/1"), Arn: aws.String(queue)},
		"bad input":           {Id: aws.String("t"), Arn: aws.String(queue), Input: aws.String("{")},
		"input and path": {
			Id: aws.String("t"), Arn: aws.String(queue), Input: aws.String("{}"), InputPath: aws.String("$.detail"),
		},
		"unsupported path": {Id: aws.String("t"), Arn: aws.String(queue), InputPath: aws.String("$.a[0]")},
		"sqs parameters": {
			Id:            aws.String("t"),
			Arn:           aws.String("arn:aws:sns:us-east-1:000000000000:topic"),
			SqsParameters: &ebtypes.SqsParameters{MessageGroupId: aws.String("g")},
		},
	} {
		_, err = c.PutTargets(
			ctx,
			&eventbridge.PutTargetsInput{Rule: aws.String("orders"), Targets: []ebtypes.Target{target}},
		)
		if err == nil {
			t.Fatal(name, "accepted")
		}

		code(t, err, "ValidationException")
	}

	var targets []ebtypes.Target
	for i := range 6 {
		targets = append(targets, ebtypes.Target{Id: aws.String(fmt.Sprint(i)), Arn: aws.String(queue)})
	}

	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{Rule: aws.String("orders"), Targets: targets})
	code(t, err, "LimitExceededException")
	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{Rule: aws.String("missing"), Targets: targets[:1]})
	code(t, err, "ResourceNotFoundException")
	_, err = c.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: make([]ebtypes.PutEventsRequestEntry, 11)})
	code(t, err, "ValidationException")
}

func TestEventBridgeTestEventPattern(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	event := `{"id":"1","account":"000000000000","source":"shop","time":"2026-01-01T00:00:00Z","region":"us-east-1",
		"detail-type":"order","resources":[],"detail":{"state":"PAID","amount":42.5,"tags":["a","vip"],
		"items":[{"sku":"X-1"},{"sku":"Y-2"}],"note":null,"customer":{"country":"DE","ip":"10.1.2.3"}}}`
	for pattern, want := range map[string]bool{
		`{"source":["shop"]}`:                                                                  true,
		`{"source":["bank"]}`:                                                                  false,
		`{"source":["shop"],"detail-type":["refund"]}`:                                         false,
		`{"detail":{"state":["PAID","SHIPPED"]}}`:                                              true,
		`{"detail":{"amount":[42.5]}}`:                                                         true,
		`{"detail":{"amount":["42.5"]}}`:                                                       false,
		`{"detail":{"tags":["vip"]}}`:                                                          true,
		`{"detail":{"items":{"sku":["Y-2"]}}}`:                                                 true,
		`{"detail":{"note":[null]}}`:                                                           true,
		`{"detail":{"state":[{"prefix":"PA"}]}}`:                                               true,
		`{"detail":{"state":[{"suffix":"ID"}]}}`:                                               true,
		`{"detail":{"state":[{"equals-ignore-case":"paid"}]}}`:                                 true,
		`{"detail":{"state":[{"wildcard":"P*D"}]}}`:                                            true,
		`{"detail":{"state":[{"wildcard":"P*X"}]}}`:                                            false,
		`{"detail":{"state":[{"anything-but":"PAID"}]}}`:                                       false,
		`{"detail":{"state":[{"anything-but":["NEW","OLD"]}]}}`:                                true,
		`{"detail":{"state":[{"anything-but":{"prefix":"P"}}]}}`:                               false,
		`{"detail":{"amount":[{"numeric":[">",40,"<=",42.5]}]}}`:                               true,
		`{"detail":{"amount":[{"numeric":["<",40]}]}}`:                                         false,
		`{"detail":{"customer":{"ip":[{"cidr":"10.1.0.0/16"}]}}}`:                              true,
		`{"detail":{"customer":{"ip":[{"cidr":"10.2.0.0/16"}]}}}`:                              false,
		`{"detail":{"customer":{"country":[{"exists":true}]}}}`:                                true,
		`{"detail":{"missing":[{"exists":false}]}}`:                                            true,
		`{"detail":{"customer":[{"exists":true}]}}`:                                            false,
		`{"detail":{"missing":[{"anything-but":"x"}]}}`:                                        false,
		`{"source":["shop"],"$or":[{"detail":{"state":["NEW"]}},{"detail":{"tags":["vip"]}}]}`: true,
		`{"$or":[{"source":["bank"]},{"detail-type":["refund"]}]}`:                             false,
	} {
		out, err := c.TestEventPattern(context.Background(), &eventbridge.TestEventPatternInput{
			EventPattern: aws.String(pattern),
			Event:        aws.String(event),
		})
		if err != nil {
			t.Fatal(pattern, err)
		}

		if out.Result != want {
			t.Errorf("%s: got %t", pattern, out.Result)
		}
	}
}

func TestEventBridgeDelivers(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverWith(t, runtime)
	c := eventsClient(s, "test")
	queues := sqsClient(s, "test")
	ctx := context.Background()
	wholeURL, whole := queueARN(t, queues, "whole")
	detailURL, detail := queueARN(t, queues, "detail")
	fifoURL := createQueue(t, queues, "ordered.fifo", map[string]string{"FifoQueue": "true"})
	fifo := "arn:aws:sqs:us-east-1:000000000000:ordered.fifo"
	topicInboxURL, topicInbox := queueARN(t, queues, "topic-inbox")
	topics := snsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("fanout")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn:   topic.TopicArn,
		Protocol:   aws.String("sqs"),
		Endpoint:   &topicInbox,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	fn := createFunction(t, lambdaClient(s, "test"), "on-order", "app.echo")
	putRule(t, c, "orders", `{"source":["shop"],"detail":{"state":["PAID"]}}`)
	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: aws.String("orders"),
		Targets: []ebtypes.Target{
			{Id: aws.String("whole"), Arn: &whole},
			{Id: aws.String("detail"), Arn: &detail, InputPath: aws.String("$.detail")},
			{
				Id:            aws.String("fifo"),
				Arn:           &fifo,
				Input:         aws.String(`{"fixed":true}`),
				SqsParameters: &ebtypes.SqsParameters{MessageGroupId: aws.String("orders")},
			},
			{Id: aws.String("topic"), Arn: topic.TopicArn},
			{Id: aws.String("function"), Arn: fn.FunctionArn},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A disabled rule does not deliver.
	putRule(t, c, "muted", `{"source":["shop"]}`)
	_, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:    aws.String("muted"),
		Targets: []ebtypes.Target{{Id: aws.String("whole"), Arn: &whole}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.DisableRule(ctx, &eventbridge.DisableRuleInput{Name: aws.String("muted")}); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	out, err := c.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{
		{
			Source:     aws.String("shop"),
			DetailType: aws.String("order"),
			Detail:     aws.String(`{"state": "PAID", "id": 7}`),
			Resources:  []string{"arn:aws:s3:::bucket"},
			Time:       &at,
		},
		{Source: aws.String("shop"), DetailType: aws.String("order"), Detail: aws.String(`{"state":"NEW"}`)},
		{DetailType: aws.String("order"), Detail: aws.String(`{}`)},
		{Source: aws.String("shop"), DetailType: aws.String("order"), Detail: aws.String(`[1]`)},
		{
			Source:       aws.String("shop"),
			DetailType:   aws.String("order"),
			Detail:       aws.String(`{}`),
			EventBusName: aws.String("custom-bus"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if out.FailedEntryCount != 3 || len(out.Entries) != 5 || aws.ToString(out.Entries[0].EventId) == "" ||
		aws.ToString(out.Entries[2].ErrorCode) != "InvalidArgument" ||
		aws.ToString(out.Entries[3].ErrorCode) != "MalformedDetail" ||
		aws.ToString(out.Entries[4].ErrorCode) != "NotFoundException" {
		t.Fatalf("%+v", out)
	}

	receive := func(queueURL string) []string {
		t.Helper()
		msgs, receiveErr := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
		})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}

		bodies := make([]string, 0, len(msgs.Messages))
		for _, m := range msgs.Messages {
			bodies = append(bodies, aws.ToString(m.Body))
		}

		return bodies
	}

	bodies := receive(wholeURL)
	if len(bodies) != 1 {
		t.Fatal(bodies)
	}

	var event map[string]any
	if err = json.Unmarshal([]byte(bodies[0]), &event); err != nil {
		t.Fatal(err)
	}

	eventID := aws.ToString(out.Entries[0].EventId)
	if event["id"] != eventID || event["source"] != "shop" || event["detail-type"] != "order" ||
		event["account"] != "000000000000" || event["region"] != "us-east-1" || event["version"] != "0" ||
		event["time"] != "2026-01-02T03:04:05Z" || fmt.Sprint(event["resources"]) != "[arn:aws:s3:::bucket]" ||
		fmt.Sprint(event["detail"]) != "map[id:7 state:PAID]" {
		t.Fatal(bodies[0])
	}

	if bodies = receive(detailURL); len(bodies) != 1 || bodies[0] != `{"id":7,"state":"PAID"}` {
		t.Fatal(bodies)
	}

	if bodies = receive(fifoURL); len(bodies) != 1 || bodies[0] != `{"fixed":true}` {
		t.Fatal(bodies)
	}

	if bodies = receive(topicInboxURL); len(bodies) != 1 || !strings.Contains(bodies[0], eventID) {
		t.Fatal(bodies)
	}

	select {
	case call := <-runtime.calls:
		if !strings.Contains(string(call.payload), eventID) || call.fn.Name != "on-order" {
			t.Fatalf("%s", call.payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EventBridge did not invoke the function")
	}
}

func TestEventBridgeJSON11Wire(t *testing.T) {
	s := server(t)
	r, err := http.NewRequest("POST", s.URL, strings.NewReader(`{"Name":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("X-Amz-Target", "AWSEvents.DescribeRule")
	r.Header.Set("Content-Type", "application/x-amz-json-1.1")
	resp, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Content-Type") != "application/x-amz-json-1.1" ||
		!strings.Contains(string(body), `"__type":"ResourceNotFoundException"`) {
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}

	// JSON 1.0 bodies are not accepted for a JSON 1.1 service.
	r, err = http.NewRequest("POST", s.URL, strings.NewReader(`{"Name":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("X-Amz-Target", "AWSEvents.DescribeRule")
	r.Header.Set("Content-Type", "application/x-amz-json-1.0")
	resp2, err := s.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}

	if closeErr := resp2.Body.Close(); closeErr != nil {
		t.Error(closeErr)
	}

	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatal(resp2.StatusCode)
	}
}
