package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func snsClient(s *httptest.Server, key string) *sns.Client {
	return sns.New(
		sns.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(s.URL),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

// queueARN creates a queue and returns its URL and ARN.
func queueARN(t *testing.T, c *sqs.Client, name string) (queueURL, arn string) {
	t.Helper()
	queueURL = createQueue(t, c, name, nil)
	attrs, err := c.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl:       &queueURL,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}

	return queueURL, attrs.Attributes["QueueArn"]
}

func TestSNSTopicsAndSubscriptions(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()

	created, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String("events"), Attributes: map[string]string{"DisplayName": "Events"},
	})
	if err != nil || *created.TopicArn != "arn:aws:sns:us-east-1:000000000000:events" {
		t.Fatal(created, err)
	}

	topicARN := created.TopicArn
	again, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String("events"), Attributes: map[string]string{"DisplayName": "Events"},
	})
	if err != nil || *again.TopicArn != *topicARN {
		t.Fatal("identical CreateTopic was not idempotent", again, err)
	}

	var badParam *snstypes.InvalidParameterException
	for _, in := range []*sns.CreateTopicInput{
		{Name: aws.String("events")},
		{Name: aws.String("orders.fifo")},
		{Name: aws.String("orders"), Attributes: map[string]string{"FifoTopic": "true"}},
		{Name: aws.String("bad name")},
	} {
		if _, err = c.CreateTopic(ctx, in); !errors.As(err, &badParam) {
			t.Fatalf("CreateTopic %s accepted: %v", *in.Name, err)
		}
	}

	_, arn := queueARN(t, q, "subscriber")
	sub, err := c.Subscribe(ctx, &sns.SubscribeInput{TopicArn: topicARN, Protocol: aws.String("sqs"), Endpoint: &arn})
	if err != nil {
		t.Fatal(err)
	}

	dup, err := c.Subscribe(ctx, &sns.SubscribeInput{TopicArn: topicARN, Protocol: aws.String("sqs"), Endpoint: &arn})
	if err != nil || *dup.SubscriptionArn != *sub.SubscriptionArn {
		t.Fatal("duplicate Subscribe did not return the existing subscription", dup, err)
	}

	for _, in := range []*sns.SubscribeInput{
		{TopicArn: topicARN, Protocol: aws.String("http"), Endpoint: aws.String("https://example.com")},
		{TopicArn: topicARN, Protocol: aws.String("sms"), Endpoint: aws.String("5550100")},
		{TopicArn: topicARN, Protocol: aws.String("sqs"), Endpoint: aws.String("not-an-arn")},
		{TopicArn: topicARN, Protocol: aws.String("sqs"), Endpoint: aws.String("arn:aws:sqs:us-east-1:111111111111:x")},
	} {
		if _, err = c.Subscribe(ctx, in); !errors.As(err, &badParam) {
			t.Fatalf("Subscribe %s accepted: %v", *in.Endpoint, err)
		}
	}

	var missing *snstypes.NotFoundException
	_, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: aws.String("arn:aws:sns:us-east-1:000000000000:missing"), Protocol: aws.String("sqs"), Endpoint: &arn,
	})
	if !errors.As(err, &missing) {
		t.Fatalf("Subscribe to a missing topic: %v", err)
	}

	byTopic, err := c.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{TopicArn: topicARN})
	if err != nil || len(byTopic.Subscriptions) != 1 || *byTopic.Subscriptions[0].Endpoint != arn ||
		*byTopic.Subscriptions[0].Owner != "000000000000" {
		t.Fatal(byTopic, err)
	}

	// Another account cannot see or use the topic.
	other := snsClient(s, "team-b")
	if _, err = other.Publish(ctx, &sns.PublishInput{TopicArn: topicARN, Message: aws.String("x")}); !errors.As(
		err,
		&missing,
	) {
		t.Fatalf("cross-account Publish: %v", err)
	}

	if listed, listErr := other.ListTopics(ctx, &sns.ListTopicsInput{}); listErr != nil || len(listed.Topics) != 0 {
		t.Fatal("topic visible to another account", listed, listErr)
	}

	// ListTopics pages 100 topics at a time.
	for i := range 100 {
		if _, err = c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(fmt.Sprintf("t%03d", i))}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.ListTopics(ctx, &sns.ListTopicsInput{})
	if err != nil || len(first.Topics) != 100 || first.NextToken == nil {
		t.Fatal(first, err)
	}

	second, err := c.ListTopics(ctx, &sns.ListTopicsInput{NextToken: first.NextToken})
	if err != nil || len(second.Topics) != 1 || second.NextToken != nil {
		t.Fatal(second, err)
	}

	if _, err = c.Unsubscribe(ctx, &sns.UnsubscribeInput{SubscriptionArn: sub.SubscriptionArn}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Unsubscribe(ctx, &sns.UnsubscribeInput{SubscriptionArn: sub.SubscriptionArn}); !errors.As(
		err,
		&missing,
	) {
		t.Fatalf("second Unsubscribe: %v", err)
	}

	// Deleting a topic is idempotent and removes it.
	for range 2 {
		if _, err = c.DeleteTopic(ctx, &sns.DeleteTopicInput{TopicArn: topicARN}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topicARN, Message: aws.String("x")}); !errors.As(
		err,
		&missing,
	) {
		t.Fatalf("Publish to a deleted topic: %v", err)
	}
}

func TestSNSDeliversToSQS(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()

	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("orders")})
	if err != nil {
		t.Fatal(err)
	}

	envelopeURL, envelopeARN := queueARN(t, q, "envelope")
	rawURL, rawARN := queueARN(t, q, "raw")
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &envelopeARN,
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn:   topic.TopicArn,
		Protocol:   aws.String("sqs"),
		Endpoint:   &rawARN,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A subscription to a deleted queue does not fail Publish.
	goneURL, goneARN := queueARN(t, q, "gone")
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &goneARN,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &goneURL}); err != nil {
		t.Fatal(err)
	}

	published, err := c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn,
		Subject:  aws.String("New order"),
		Message:  aws.String("order 42"),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"color": {DataType: aws.String("String"), StringValue: aws.String("red")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	receive := func(queueURL string) sqstypes.Message {
		t.Helper()
		out, receiveErr := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: &queueURL, MessageAttributeNames: []string{"All"}, WaitTimeSeconds: 1,
		})
		if receiveErr != nil || len(out.Messages) != 1 {
			t.Fatal("no delivery", out, receiveErr)
		}

		if _, receiveErr = q.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl: &queueURL, ReceiptHandle: out.Messages[0].ReceiptHandle,
		}); receiveErr != nil {
			t.Fatal(receiveErr)
		}

		return out.Messages[0]
	}

	var envelope struct {
		MessageAttributes map[string]struct{ Type, Value string }
		Type              string
		MessageId         string
		TopicArn          string
		Subject           string
		Message           string
		UnsubscribeURL    string
	}

	if err = json.Unmarshal([]byte(*receive(envelopeURL).Body), &envelope); err != nil {
		t.Fatal(err)
	}

	if envelope.Type != "Notification" || envelope.MessageId != *published.MessageId ||
		envelope.TopicArn != *topic.TopicArn || envelope.Subject != "New order" || envelope.Message != "order 42" ||
		envelope.MessageAttributes["color"].Value != "red" || envelope.UnsubscribeURL == "" {
		t.Fatalf("envelope %+v", envelope)
	}

	rawMessage := receive(rawURL)
	if *rawMessage.Body != "order 42" || *rawMessage.MessageAttributes["color"].StringValue != "red" {
		t.Fatalf("raw delivery %+v", rawMessage)
	}

	// With MessageStructure json, SQS subscribers get the "sqs" text.
	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn:         topic.TopicArn,
		MessageStructure: aws.String("json"),
		Message:          aws.String(`{"default":"for everyone","sqs":"for queues"}`),
	}); err != nil {
		t.Fatal(err)
	}

	if body := *receive(rawURL).Body; body != "for queues" {
		t.Fatalf("structured message delivered %q", body)
	}

	var badParam *snstypes.InvalidParameterException
	for _, in := range []*sns.PublishInput{
		{TopicArn: topic.TopicArn, Message: aws.String("")},
		{TopicArn: topic.TopicArn, Message: aws.String(`{"sqs":"x"}`), MessageStructure: aws.String("json")},
		{TopicArn: topic.TopicArn, Message: aws.String("x"), MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"AWS.reserved": {DataType: aws.String("String"), StringValue: aws.String("x")},
		}},
	} {
		if _, err = c.Publish(ctx, in); !errors.As(err, &badParam) {
			t.Fatalf("Publish %q accepted: %v", *in.Message, err)
		}
	}

	// After Unsubscribe the raw queue receives nothing more.
	if _, err = c.Unsubscribe(ctx, &sns.UnsubscribeInput{SubscriptionArn: raw.SubscriptionArn}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: aws.String("later")}); err != nil {
		t.Fatal(err)
	}

	left, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &rawURL})
	if err != nil || len(left.Messages) != 0 {
		t.Fatal("unsubscribed queue received a message", left, err)
	}

	subs, err := c.ListSubscriptions(ctx, &sns.ListSubscriptionsInput{})
	if err != nil || len(subs.Subscriptions) != 2 {
		t.Fatal(subs, err)
	}
}
