package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// posted is a request an HTTP subscriber received.
type posted struct {
	header http.Header
	body   string
}

// subscriber starts an HTTP endpoint that hands each request it receives to
// the returned channel.
func subscriber(t *testing.T) (endpoint string, received chan posted) {
	t.Helper()
	received = make(chan posted, 8)
	endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- posted{header: r.Header.Clone(), body: string(body)}
	})).URL

	return endpoint, received
}

func receive(t *testing.T, received chan posted) posted {
	t.Helper()
	select {
	case p := <-received:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("the endpoint received nothing")
		return posted{}
	}
}

func TestSNSHTTPSubscriptions(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	ctx := context.Background()
	created, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("hooks")})
	if err != nil {
		t.Fatal(err)
	}

	topicARN := created.TopicArn
	_, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topicARN, Protocol: aws.String("http"), Endpoint: aws.String("ftp://example.com"),
	})
	wantCode(t, err, "InvalidParameter")

	endpoint, received := subscriber(t)
	subscribed, err := c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topicARN, Protocol: aws.String("http"), Endpoint: &endpoint,
	})
	if err != nil || aws.ToString(subscribed.SubscriptionArn) != "pending confirmation" {
		t.Fatalf("subscribe = %+v, %v", subscribed, err)
	}

	request := receive(t, received)
	var confirmation struct {
		Type, Token, TopicArn, SubscribeURL string
	}
	if json.Unmarshal([]byte(request.body), &confirmation) != nil ||
		confirmation.Type != "SubscriptionConfirmation" || confirmation.TopicArn != aws.ToString(topicARN) ||
		request.header.Get("X-Amz-Sns-Message-Type") != "SubscriptionConfirmation" {
		t.Fatalf("confirmation = %+v %s", request.header, request.body)
	}

	listed, err := c.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{TopicArn: topicARN})
	if err != nil || aws.ToString(listed.Subscriptions[0].SubscriptionArn) != "PendingConfirmation" {
		t.Fatalf("list = %+v, %v", listed, err)
	}

	// Nothing is delivered before the subscription is confirmed.
	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topicARN, Message: aws.String("early")}); err != nil {
		t.Fatal(err)
	}

	// The endpoint's owner confirms by visiting the SubscribeURL, unsigned.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, confirmation.SubscribeURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	confirmed, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(confirmed), "<SubscriptionArn>"+
		aws.ToString(topicARN)+":") {
		t.Fatalf("confirm = %d %s", resp.StatusCode, confirmed)
	}

	listed, err = c.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{TopicArn: topicARN})
	if err != nil {
		t.Fatal(err)
	}

	subARN := listed.Subscriptions[0].SubscriptionArn
	if !strings.HasPrefix(aws.ToString(subARN), aws.ToString(topicARN)+":") {
		t.Fatalf("confirmed subscription = %s", aws.ToString(subARN))
	}

	_, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topicARN, Message: aws.String("hello"), Subject: aws.String("greeting"),
	})
	if err != nil {
		t.Fatal(err)
	}

	request = receive(t, received)
	var note struct{ Type, Message, Subject, TopicArn string }
	if json.Unmarshal([]byte(request.body), &note) != nil || note.Type != "Notification" ||
		note.Message != "hello" || note.Subject != "greeting" ||
		request.header.Get("X-Amz-Sns-Subscription-Arn") != aws.ToString(subARN) ||
		request.header.Get("X-Amz-Sns-Message-Type") != "Notification" {
		t.Fatalf("notification = %+v %s", request.header, request.body)
	}

	// Raw delivery posts the message alone.
	_, err = c.SetSubscriptionAttributes(ctx, &sns.SetSubscriptionAttributesInput{
		SubscriptionArn: subARN, AttributeName: aws.String("RawMessageDelivery"), AttributeValue: aws.String("true"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topicARN, Message: aws.String("raw")}); err != nil {
		t.Fatal(err)
	}

	request = receive(t, received)
	if request.body != "raw" || request.header.Get("X-Amz-Sns-Rawdelivery") != "true" {
		t.Fatalf("raw = %+v %s", request.header, request.body)
	}

	_, err = c.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{TopicArn: topicARN, Token: aws.String("bad")})
	wantCode(t, err, "InvalidParameter")
}

func TestSNSEmailSubscriptions(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	ctx := context.Background()
	created, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("alerts")})
	if err != nil {
		t.Fatal(err)
	}

	topicARN := created.TopicArn
	_, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topicARN, Protocol: aws.String("email"), Endpoint: aws.String("not an address"),
	})
	wantCode(t, err, "InvalidParameter")

	_, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topicARN, Protocol: aws.String("email"), Endpoint: aws.String("ops@example.com"),
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	wantCode(t, err, "InvalidParameter")

	subscribed, err := c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topicARN, Protocol: aws.String("email"), Endpoint: aws.String("ops@example.com"),
		ReturnSubscriptionArn: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	attributes, err := c.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{
		SubscriptionArn: subscribed.SubscriptionArn,
	})
	if err != nil || attributes.Attributes["PendingConfirmation"] != "true" {
		t.Fatalf("attributes = %+v, %v", attributes, err)
	}

	// The confirmation message is recorded, never sent.
	records := deliveries(t, s, 1)
	var confirmation struct{ Token string }
	if records[0].Target != "ops@example.com" || json.Unmarshal([]byte(records[0].Payload), &confirmation) != nil {
		t.Fatalf("confirmation = %+v", records[0])
	}

	confirmed, err := c.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{
		TopicArn: topicARN, Token: &confirmation.Token,
	})
	if err != nil || aws.ToString(confirmed.SubscriptionArn) != aws.ToString(subscribed.SubscriptionArn) {
		t.Fatalf("confirm = %+v, %v", confirmed, err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topicARN, Message: aws.String("disk full")}); err != nil {
		t.Fatal(err)
	}

	records = deliveries(t, s, 2)
	if records[0].Target != "ops@example.com" || records[0].Payload != "disk full" || records[0].Status != "delivered" {
		t.Fatalf("email = %+v", records[0])
	}
}
