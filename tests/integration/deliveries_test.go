package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	eventtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/delivery"
)

// deliveries waits until the delivery log holds want records and returns them,
// newest first.
func deliveries(t *testing.T, s *httptest.Server, want int) []delivery.Record {
	t.Helper()
	var out struct {
		Deliveries []delivery.Record `json:"deliveries"`
	}

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if status := internal(t, s, http.MethodGet, "/_tinystack/deliveries", &out); status != http.StatusOK {
			t.Fatalf("status %d", status)
		}

		if len(out.Deliveries) >= want {
			return out.Deliveries
		}
	}

	t.Fatalf("wanted %d deliveries, got %+v", want, out.Deliveries)
	return nil
}

func TestDeliveryLog(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	queues := sqsClient(s, "test")
	queueURL, queueArn := queueARN(t, queues, "delivered")
	_, goneArn := queueARN(t, queues, "gone")
	topics := snsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("news")})
	if err != nil {
		t.Fatal(err)
	}

	for _, endpoint := range []string{queueArn, goneArn} {
		_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
			TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(endpoint),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	goneURL := strings.TrimSuffix(queueURL, "delivered") + "gone"
	if _, err = queues.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &goneURL}); err != nil {
		t.Fatal(err)
	}

	if _, err = topics.Publish(
		ctx,
		&sns.PublishInput{TopicArn: topic.TopicArn, Message: aws.String("hi")},
	); err != nil {
		t.Fatal(err)
	}

	records := deliveries(t, s, 2)
	byTarget := map[string]delivery.Record{}
	for _, r := range records {
		byTarget[r.Target] = r
	}

	ok, failed := byTarget[queueArn], byTarget[goneArn]
	if ok.Via != delivery.ViaSNS || ok.Source != *topic.TopicArn || ok.Status != "delivered" ||
		!strings.Contains(ok.Payload, `"Message":"hi"`) || ok.Size != len(ok.Payload) {
		t.Fatalf("delivered: %+v", ok)
	}

	if failed.Status != "failed" || failed.Error == "" {
		t.Fatalf("failed: %+v", failed)
	}

	if status := internal(t, s, http.MethodDelete, "/_tinystack/deliveries", nil); status != http.StatusNoContent {
		t.Fatalf("DELETE status %d", status)
	}

	functions := lambdaClient(s, "test")
	fn := createFunction(t, functions, "broken", "app.fail")
	events := eventsClient(s, "test")
	ruleArn := putRule(t, events, "fanout", `{"source":["app"]}`)
	_, err = events.PutTargets(
		ctx,
		&eventbridge.PutTargetsInput{Rule: aws.String("fanout"), Targets: []eventtypes.Target{
			{Id: aws.String("fn"), Arn: fn.FunctionArn},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = events.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []eventtypes.PutEventsRequestEntry{{
		Source: aws.String("app"), DetailType: aws.String("t"), Detail: aws.String(`{}`),
	}}})
	if err != nil {
		t.Fatal(err)
	}

	// The rule hands the event to Lambda, whose first attempt then fails.
	records = deliveries(t, s, 2)
	attempt, handed := records[0], records[1]
	if handed.Via != delivery.ViaEventBridge || handed.Source != ruleArn || handed.Target != *fn.FunctionArn ||
		handed.Status != "delivered" {
		t.Fatalf("rule delivery: %+v", handed)
	}

	if attempt.Via != delivery.ViaLambdaAsync || attempt.Target != *fn.FunctionArn || attempt.Status != "failed" ||
		!strings.Contains(attempt.Error, "attempt 1 of 3") || !strings.Contains(attempt.Error, "boom") {
		t.Fatalf("async attempt: %+v", attempt)
	}

	internal(t, s, http.MethodDelete, "/_tinystack/deliveries", nil)
	echo := createFunction(t, functions, "echo", "app.echo")
	_, err = functions.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: aws.String("echo"), EventSourceArn: &queueArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = queues.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("job")},
	); err != nil {
		t.Fatal(err)
	}

	// The queue still holds the SNS notification, so the batch may carry both.
	var batch delivery.Record
	for deadline := time.Now().Add(5 * time.Second); batch.Via == "" && time.Now().Before(deadline); {
		for _, r := range deliveries(t, s, 1) {
			if r.Via == delivery.ViaEventSourceMapping && strings.Contains(r.Payload, "job") {
				batch = r
			}
		}
	}

	if batch.Source != queueArn || batch.Target != *echo.FunctionArn || batch.Status != "delivered" {
		t.Fatalf("mapping batch: %+v", batch)
	}
}
