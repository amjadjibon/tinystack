package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	eventtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type browsed struct {
	Details map[string]any    `json:"details"`
	Tags    map[string]string `json:"tags"`
	Service string            `json:"service"`
	Type    string            `json:"type"`
	Account string            `json:"account"`
	Region  string            `json:"region"`
	Name    string            `json:"name"`
	ARN     string            `json:"arn"`
}

func TestResourceBrowser(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	queues := sqsClient(s, "team-b")
	queueURL, queueArn := queueARN(t, queues, "browsed")
	_, sourceArn := queueARN(t, sqsClient(s, "test"), "source")
	_, err := queues.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("hi")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = queues.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: &queueURL, Tags: map[string]string{"team": "b"}})
	if err != nil {
		t.Fatal(err)
	}

	topics := snsClient(s, "test")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("browsed")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &sourceArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = create(ctx, client(s, "test", "eu-west-1"), "browsed"); err != nil {
		t.Fatal(err)
	}

	buckets := s3Client(s, "test", "us-east-1")
	if _, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("browsed")}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"a", "b/c"} {
		_, err = buckets.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("browsed"), Key: aws.String(key), Body: strings.NewReader("12345"),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	functions := lambdaClient(s, "test")
	createFunction(t, functions, "browsed", "app.echo")
	_, err = functions.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: aws.String("browsed"), EventSourceArn: &sourceArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	events := eventsClient(s, "test")
	ruleArn := putRule(t, events, "browsed", `{"source":["app"]}`)
	_, err = events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: aws.String("browsed"), Targets: []eventtypes.Target{{Id: aws.String("q"), Arn: &sourceArn}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var out struct {
		Resources []browsed `json:"resources"`
	}

	if status := internal(t, s, http.MethodGet, "/_tinystack/resources", &out); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}

	got := map[string]browsed{}
	for _, r := range out.Resources {
		got[r.Service+" "+r.Type+" "+r.Name] = r
	}

	if len(got) != 8 {
		t.Fatalf("resources: %+v", out.Resources)
	}

	queue := got["sqs queue browsed"]
	if queue.Account != "111111111111" || queue.ARN != queueArn || queue.Tags["team"] != "b" ||
		queue.Details["ApproximateNumberOfMessages"] != "1" {
		t.Fatalf("queue: %+v", queue)
	}

	subscriptions, _ := got["sns topic browsed"].Details["Subscriptions"].([]any)
	if got["sns topic browsed"].ARN != *topic.TopicArn || len(subscriptions) != 1 {
		t.Fatalf("topic: %+v", got["sns topic browsed"])
	}

	if table := got["dynamodb table browsed"]; table.Region != "eu-west-1" || table.Details["TableStatus"] != "ACTIVE" {
		t.Fatalf("table: %+v", table)
	}

	if bucket := got["s3 bucket browsed"]; bucket.Details["Objects"] != 2.0 || bucket.Details["Size"] != 10.0 {
		t.Fatalf("bucket: %+v", bucket)
	}

	if fn := got["lambda function browsed"]; fn.Details["Runtime"] != "python3.12" {
		t.Fatalf("function: %+v", fn)
	}

	if rule := got["eventbridge rule browsed"]; rule.ARN != ruleArn || len(rule.Details["Targets"].([]any)) != 1 {
		t.Fatalf("rule: %+v", rule)
	}

	for _, r := range out.Resources {
		if r.Service == "lambda" && r.Type == "event-source-mapping" && r.Details["FunctionArn"] == nil {
			t.Fatalf("mapping: %+v", r)
		}
	}

	if status := internal(t, s, http.MethodPost, "/_tinystack/resources", nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", status)
	}
}
