package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

func waitForSQSMessage(t *testing.T, q *sqs.Client, queueURL string) sqstypes.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := q.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl:              &queueURL,
			MaxNumberOfMessages:   1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(out.Messages) > 0 {
			_, _ = q.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
				QueueUrl: &queueURL, ReceiptHandle: out.Messages[0].ReceiptHandle,
			})
			return out.Messages[0]
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for message on %s", queueURL)
	return sqstypes.Message{}
}

func TestSNSSubscriptionRedrivePolicy(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")

	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("dlq-topic")})
	if err != nil {
		t.Fatal(err)
	}

	dlqURL, dlqARN := queueARN(t, q, "sns-dlq")
	missingURL, missingARN := queueARN(t, q, "will-delete")
	if _, err = q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &missingURL}); err != nil {
		t.Fatal(err)
	}

	// Invalid RedrivePolicy values are rejected.
	for _, bad := range []string{
		"not-json",
		`{}`,
		`{"deadLetterTargetArn":"arn:aws:sns:us-east-1:000000000000:not-sqs"}`,
		`{"deadLetterTargetArn":"` + dlqARN + `","extra":1}`,
	} {
		if bad == "{}" {
			continue
		}

		_, err = c.Subscribe(ctx, &sns.SubscribeInput{
			TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &missingARN,
			Attributes: map[string]string{"RedrivePolicy": bad},
		})
		wantCode(t, err, "InvalidParameter")
	}

	redriveJSON := fmt.Sprintf(`{"deadLetterTargetArn":%q}`, dlqARN)
	sub, err := c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &missingARN,
		Attributes: map[string]string{"RawMessageDelivery": "true", "RedrivePolicy": redriveJSON},
	})
	if err != nil {
		t.Fatal(err)
	}

	attrs, err := c.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{
		SubscriptionArn: sub.SubscriptionArn,
	})
	if err != nil || attrs.Attributes["RedrivePolicy"] != redriveJSON {
		t.Fatalf("RedrivePolicy attr = %+v, %v", attrs, err)
	}

	// Missing SQS target redrives raw message with original attributes plus ErrorCode and ErrorMessage.
	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn,
		Message:  new("lost-order"),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"env": {DataType: new("String"), StringValue: new("prod")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	dlqMsg := waitForSQSMessage(t, q, dlqURL)
	if aws.ToString(dlqMsg.Body) != "lost-order" ||
		aws.ToString(dlqMsg.MessageAttributes["env"].StringValue) != "prod" ||
		aws.ToString(dlqMsg.MessageAttributes["ErrorCode"].StringValue) == "" ||
		aws.ToString(dlqMsg.MessageAttributes["ErrorMessage"].StringValue) == "" {
		t.Fatalf("unexpected SQS DLQ message: %+v", dlqMsg)
	}

	// Missing Lambda target redrives SNS JSON envelope with ErrorCode and ErrorMessage.
	lambdaDLQURL, lambdaDLQARN := queueARN(t, q, "lambda-dlq")
	lambdaSub, err := c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("lambda"),
		Endpoint: new("arn:aws:lambda:us-east-1:000000000000:function:does-not-exist"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.SetSubscriptionAttributes(ctx, &sns.SetSubscriptionAttributesInput{
		SubscriptionArn: lambdaSub.SubscriptionArn,
		AttributeName:   new("RedrivePolicy"),
		AttributeValue:  new(fmt.Sprintf(`{"deadLetterTargetArn":%q}`, lambdaDLQARN)),
	}); err != nil {
		t.Fatal(err)
	}

	// Clearing RedrivePolicy on the SQS subscription stops redriving it.
	if _, err = c.SetSubscriptionAttributes(ctx, &sns.SetSubscriptionAttributesInput{
		SubscriptionArn: sub.SubscriptionArn,
		AttributeName:   new("RedrivePolicy"),
		AttributeValue:  new(""),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: new("lambda-fail"),
	}); err != nil {
		t.Fatal(err)
	}

	lambdaMsg := waitForSQSMessage(t, q, lambdaDLQURL)
	var envelope struct {
		Type    string
		Message string
	}
	if err = json.Unmarshal([]byte(aws.ToString(lambdaMsg.Body)), &envelope); err != nil ||
		envelope.Type != "Notification" || envelope.Message != "lambda-fail" ||
		aws.ToString(lambdaMsg.MessageAttributes["ErrorCode"].StringValue) == "" {
		t.Fatalf("unexpected Lambda DLQ message: %+v", lambdaMsg)
	}
}

func TestSNSSubscriptionRedrivePolicyAuthorized(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Authorization = config.AuthorizationEnforce
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := snsClient(s, "test")
	q := sqsClient(s, "test")

	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("auth-dlq")})
	if err != nil {
		t.Fatal(err)
	}

	_, targetARN := queueARN(t, q, "denied-target")
	dlqURL, dlqARN := queueARN(t, q, "allowed-dlq")
	allowDelivery(t, s, "test", dlqARN, "sns.amazonaws.com", *topic.TopicArn)

	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &targetARN,
		Attributes: map[string]string{
			"RawMessageDelivery": "true",
			"RedrivePolicy":      fmt.Sprintf(`{"deadLetterTargetArn":%q}`, dlqARN),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Primary queue denies delivery; DLQ policy allows sns.amazonaws.com, so message lands in DLQ.
	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: new("denied-primary"),
	}); err != nil {
		t.Fatal(err)
	}

	msg := waitForSQSMessage(t, q, dlqURL)
	if aws.ToString(msg.Body) != "denied-primary" ||
		aws.ToString(msg.MessageAttributes["ErrorCode"].StringValue) != "AccessDenied" {
		t.Fatalf("authorized DLQ msg = %+v", msg)
	}
}

func TestSNSDeliveryPolicyAndRetries(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")

	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("retry-topic")})
	if err != nil {
		t.Fatal(err)
	}

	attrs, err := c.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: topic.TopicArn})
	if err != nil || !strings.Contains(attrs.Attributes["EffectiveDeliveryPolicy"], `"defaultHealthyRetryPolicy"`) {
		t.Fatalf("default topic EffectiveDeliveryPolicy = %+v, %v", attrs, err)
	}

	// Invalid delivery policies fail explicitly.
	for _, bad := range []string{
		`not-json`,
		`{}`,
		`{"http":{"defaultHealthyRetryPolicy":{"numRetries":101}}}`,
		`{"http":{"defaultHealthyRetryPolicy":{"minDelayTarget":30,"maxDelayTarget":10}}}`,
		`{"http":{"defaultHealthyRetryPolicy":{"backoffFunction":"bogus"}}}`,
		`{"http":{"defaultRequestPolicy":{"headerContentType":"text/html"}}}`,
	} {
		_, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
			TopicArn: topic.TopicArn, AttributeName: new("DeliveryPolicy"), AttributeValue: new(bad),
		})
		wantCode(t, err, "InvalidParameter")
	}

	topicPolicy := `{"http":{"defaultHealthyRetryPolicy":{"minDelayTarget":1,"maxDelayTarget":2,"numRetries":2,` +
		`"numNoDelayRetries":1,"backoffFunction":"linear"},"disableSubscriptionOverrides":false,` +
		`"defaultRequestPolicy":{"headerContentType":"application/json"}}}`
	if _, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: topic.TopicArn, AttributeName: new("DeliveryPolicy"), AttributeValue: new(topicPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	// Flaky HTTP server fails first 2 notifications with 500, succeeds on 3rd.
	var flakyAttempts atomic.Int32
	var flakyContentType atomic.Value
	confirmCh := make(chan string, 2)
	flakyDone := make(chan struct{}, 2)
	flakySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/hook" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Amz-Sns-Message-Type") == "SubscriptionConfirmation" {
			var conf struct{ Token string }
			_ = json.Unmarshal(body, &conf)
			confirmCh <- conf.Token
			w.WriteHeader(http.StatusOK)
			return
		}

		flakyContentType.Store(r.Header.Get("Content-Type"))
		n := flakyAttempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		flakyDone <- struct{}{}
	}))
	t.Cleanup(flakySrv.Close)

	flakySub, err := c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("http"), Endpoint: new(flakySrv.URL + "/hook"),
		ReturnSubscriptionArn: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case tok := <-confirmCh:
		if _, err = c.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{
			TopicArn: topic.TopicArn, Token: &tok,
		}); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for flaky confirmation")
	}

	subAttrs, err := c.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{
		SubscriptionArn: flakySub.SubscriptionArn,
	})
	if err != nil || !strings.Contains(subAttrs.Attributes["EffectiveDeliveryPolicy"], `"numRetries":2`) ||
		!strings.Contains(subAttrs.Attributes["EffectiveDeliveryPolicy"], `"application/json"`) {
		t.Fatalf("subscription EffectiveDeliveryPolicy = %+v, %v", subAttrs, err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("retry-ok")}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-flakyDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("expected 3rd attempt to succeed, attempts=%d", flakyAttempts.Load())
	}

	if flakyAttempts.Load() != 3 || flakyContentType.Load() != "application/json" {
		t.Fatalf("attempts=%d contentType=%v", flakyAttempts.Load(), flakyContentType.Load())
	}

	// Failing HTTP endpoint exhausts configured retries and redrives to DLQ.
	var failAttempts atomic.Int32
	failConfirmCh := make(chan string, 2)
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/fail" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Amz-Sns-Message-Type") == "SubscriptionConfirmation" {
			var conf struct{ Token string }
			_ = json.Unmarshal(body, &conf)
			failConfirmCh <- conf.Token
			w.WriteHeader(http.StatusOK)
			return
		}

		failAttempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(failSrv.Close)

	dlqURL, dlqARN := queueARN(t, q, "http-retry-dlq")
	subPolicy := `{"healthyRetryPolicy":{"minDelayTarget":1,"maxDelayTarget":1,"numRetries":2,"numNoDelayRetries":2}}`
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("http"), Endpoint: new(failSrv.URL + "/fail"),
		Attributes: map[string]string{
			"DeliveryPolicy": subPolicy,
			"RedrivePolicy":  fmt.Sprintf(`{"deadLetterTargetArn":%q}`, dlqARN),
		},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case tok := <-failConfirmCh:
		if _, err = c.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{
			TopicArn: topic.TopicArn, Token: &tok,
		}); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fail confirmation")
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("always-503")}); err != nil {
		t.Fatal(err)
	}

	dlqMsg := waitForSQSMessage(t, q, dlqURL)
	if failAttempts.Load() != 3 {
		t.Fatalf("expected 3 attempts (initial + 2 retries), got %d", failAttempts.Load())
	}

	if aws.ToString(dlqMsg.MessageAttributes["ErrorCode"].StringValue) != "503" {
		t.Fatalf("expected ErrorCode=503, got %+v", dlqMsg.MessageAttributes)
	}
}

func TestSNSKMSEncryption(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")
	k := kmsClient(s, "test", "us-east-1")

	// AWS-managed key alias/aws/sns skips KMS validation.
	managed, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name:       new("kms-managed"),
		Attributes: map[string]string{"KmsMasterKeyId": "alias/aws/sns"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: managed.TopicArn, Message: new("ok")}); err != nil {
		t.Fatal(err)
	}

	// Customer-managed KMS key lifecycle: enabled -> disabled -> pending deletion -> cleared.
	key := createKey(t, k, "sns-sse")
	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name:       new("kms-cmk"),
		Attributes: map[string]string{"KmsMasterKeyId": *key.Arn},
	})
	if err != nil {
		t.Fatal(err)
	}

	attrs, err := c.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: topic.TopicArn})
	if err != nil || attrs.Attributes["KmsMasterKeyId"] != *key.Arn {
		t.Fatalf("KmsMasterKeyId attr = %+v, %v", attrs, err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("encrypted")}); err != nil {
		t.Fatal(err)
	}

	if _, err = k.DisableKey(ctx, &kms.DisableKeyInput{KeyId: key.KeyId}); err != nil {
		t.Fatal(err)
	}

	var kmsDisabled *snstypes.KMSDisabledException
	_, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("blocked")})
	if !errors.As(err, &kmsDisabled) {
		t.Fatalf("disabled key Publish err = %v", err)
	}

	_, err = c.PublishBatch(ctx, &sns.PublishBatchInput{
		TopicArn: topic.TopicArn,
		PublishBatchRequestEntries: []snstypes.PublishBatchRequestEntry{
			{Id: new("1"), Message: new("blocked")},
		},
	})
	if !errors.As(err, &kmsDisabled) {
		t.Fatalf("disabled key PublishBatch err = %v", err)
	}

	if _, err = k.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{
		KeyId: key.KeyId, PendingWindowInDays: new(int32(7)),
	}); err != nil {
		t.Fatal(err)
	}

	var kmsInvalidState *snstypes.KMSInvalidStateException
	_, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("deleting")})
	if !errors.As(err, &kmsInvalidState) {
		t.Fatalf("pending deletion Publish err = %v", err)
	}

	// Missing KMS key fails with KMSNotFoundException.
	if _, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: topic.TopicArn, AttributeName: new("KmsMasterKeyId"), AttributeValue: new("alias/no-such-key"),
	}); err != nil {
		t.Fatal(err)
	}

	var kmsNotFound *snstypes.KMSNotFoundException
	_, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("missing")})
	if !errors.As(err, &kmsNotFound) {
		t.Fatalf("missing key Publish err = %v", err)
	}

	// Clearing KmsMasterKeyId restores publishing and removes the attribute.
	if _, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: topic.TopicArn, AttributeName: new("KmsMasterKeyId"), AttributeValue: new(""),
	}); err != nil {
		t.Fatal(err)
	}

	attrs, err = c.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: topic.TopicArn})
	if _, exists := attrs.Attributes["KmsMasterKeyId"]; err != nil || exists {
		t.Fatalf("cleared KmsMasterKeyId = %+v, %v", attrs, err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("unencrypted")}); err != nil {
		t.Fatal(err)
	}
}

func TestSNSAuthenticateOnUnsubscribe(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")

	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("auth-unsub")})
	if err != nil {
		t.Fatal(err)
	}

	endpoint, received := subscriber(t)
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("http"), Endpoint: &endpoint,
	}); err != nil {
		t.Fatal(err)
	}

	confReq := receive(t, received)
	var conf struct{ Token string }
	if err = json.Unmarshal([]byte(confReq.body), &conf); err != nil {
		t.Fatal(err)
	}

	// Invalid AuthenticateOnUnsubscribe value fails.
	_, err = c.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{
		TopicArn: topic.TopicArn, Token: &conf.Token, AuthenticateOnUnsubscribe: new("maybe"),
	})
	wantCode(t, err, "InvalidParameter")

	// Signed confirmation with AuthenticateOnUnsubscribe=true requires authentication to unsubscribe.
	confirmed, err := c.ConfirmSubscription(ctx, &sns.ConfirmSubscriptionInput{
		TopicArn: topic.TopicArn, Token: &conf.Token, AuthenticateOnUnsubscribe: new("true"),
	})
	if err != nil {
		t.Fatal(err)
	}

	attrs, err := c.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{
		SubscriptionArn: confirmed.SubscriptionArn,
	})
	if err != nil || attrs.Attributes["ConfirmationWasAuthenticated"] != "true" {
		t.Fatalf("attrs = %+v, %v", attrs, err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("hi")}); err != nil {
		t.Fatal(err)
	}

	noteReq := receive(t, received)
	var note struct{ UnsubscribeURL string }
	if err = json.Unmarshal([]byte(noteReq.body), &note); err != nil {
		t.Fatal(err)
	}

	// Unsigned GET to UnsubscribeURL is rejected.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, note.UnsubscribeURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "AuthorizationError") {
		t.Fatalf("unsigned unsubscribe = %d %s", resp.StatusCode, body)
	}

	// Authenticated owner Unsubscribe succeeds.
	if _, err = c.Unsubscribe(ctx, &sns.UnsubscribeInput{SubscriptionArn: confirmed.SubscriptionArn}); err != nil {
		t.Fatal(err)
	}

	// Unsigned SubscribeURL confirmation sets ConfirmationWasAuthenticated=false and allows unsigned UnsubscribeURL.
	endpoint2, received2 := subscriber(t)
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("http"), Endpoint: &endpoint2,
	}); err != nil {
		t.Fatal(err)
	}

	confReq2 := receive(t, received2)
	var conf2 struct{ SubscribeURL string }
	if err = json.Unmarshal([]byte(confReq2.body), &conf2); err != nil {
		t.Fatal(err)
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, conf2.SubscribeURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("unsigned confirm status = %d", resp2.StatusCode)
	}

	listed, err := c.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{TopicArn: topic.TopicArn})
	if err != nil || len(listed.Subscriptions) != 1 {
		t.Fatalf("listed = %+v, %v", listed, err)
	}

	attrs2, err := c.GetSubscriptionAttributes(ctx, &sns.GetSubscriptionAttributesInput{
		SubscriptionArn: listed.Subscriptions[0].SubscriptionArn,
	})
	if err != nil || attrs2.Attributes["ConfirmationWasAuthenticated"] != "false" {
		t.Fatalf("unsigned ConfirmationWasAuthenticated = %+v, %v", attrs2, err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("hi2")}); err != nil {
		t.Fatal(err)
	}

	noteReq2 := receive(t, received2)
	var note2 struct{ UnsubscribeURL string }
	if err = json.Unmarshal([]byte(noteReq2.body), &note2); err != nil {
		t.Fatal(err)
	}

	unsubReq, err := http.NewRequestWithContext(ctx, http.MethodGet, note2.UnsubscribeURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	unsubResp, err := http.DefaultClient.Do(unsubReq)
	if err != nil {
		t.Fatal(err)
	}

	_ = unsubResp.Body.Close()
	if unsubResp.StatusCode != http.StatusOK {
		t.Fatalf("unsigned unsubscribe without AuthenticateOnUnsubscribe = %d", unsubResp.StatusCode)
	}
}

func TestSNSCrossAccountAndFirehoseSubscriptions(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Identity.Accounts["test"] = cfg.Defaults.Account
	cfg.Identity.Accounts["other"] = "222222222222"
	cfg.Identity.Authorization = config.AuthorizationEnforce
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	s := serverConfig(t, cfg, runtime)

	acctA := snsClient(s, "test")
	acctBQueues := sqsClient(s, "other")
	acctBLambda := lambdaClient(s, "other")

	topic, err := acctA.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("cross-acct")})
	if err != nil {
		t.Fatal(err)
	}

	queueBURL, queueBARN := queueARN(t, acctBQueues, "cross-inbox")
	fnB, err := acctBLambda.CreateFunction(ctx, &lambda.CreateFunctionInput{
		FunctionName: new("cross-fn"),
		Runtime:      lambdatypes.RuntimePython312,
		Role:         new("arn:aws:iam::222222222222:role/lambda"),
		Handler:      new("app.echo"),
		Code:         &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e, c): return e\n")},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cross-account SQS and Lambda subscriptions succeed when resource authorization is on.
	if _, err = acctA.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &queueBARN,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = acctA.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("lambda"), Endpoint: fnB.FunctionArn,
	}); err != nil {
		t.Fatal(err)
	}

	// Without target policies, deliveries are denied (Publish still succeeds).
	if _, err = acctA.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: new("blocked-cross"),
	}); err != nil {
		t.Fatal(err)
	}

	if got := receiveOne(t, acctBQueues, queueBURL); got != "" {
		t.Fatalf("denied cross-account SQS received %q", got)
	}

	// Allow sns.amazonaws.com from Account A's topic on Account B's queue and function.
	allowDelivery(t, s, "other", queueBARN, "sns.amazonaws.com", *topic.TopicArn)
	allowServiceOnClient(t, acctBLambda, "cross-fn", "sns.amazonaws.com", *topic.TopicArn)

	if _, err = acctA.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: new("allowed-cross"),
	}); err != nil {
		t.Fatal(err)
	}

	msg := waitForSQSMessage(t, acctBQueues, queueBURL)
	if aws.ToString(msg.Body) != "allowed-cross" {
		t.Fatalf("cross-account SQS body = %q", aws.ToString(msg.Body))
	}

	select {
	case call := <-runtime.calls:
		if !strings.Contains(string(call.payload), "allowed-cross") {
			t.Fatalf("cross-account Lambda payload = %s", call.payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-account Lambda invocation")
	}

	// Firehose subscription with role authorization, raw delivery, and DLQ on failure.
	s3c := s3Client(s, "test", "us-east-1")
	if _, err = s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("sns-fh-bucket")}); err != nil {
		t.Fatal(err)
	}

	fh := firehoseClient(s)
	fhRoleARN := createSNSServiceRole(t, s, "test", "firehose-s3-role", "firehose.amazonaws.com",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`)
	createdStream, err := fh.CreateDeliveryStream(ctx, &firehose.CreateDeliveryStreamInput{
		DeliveryStreamName: new("sns-events"),
		ExtendedS3DestinationConfiguration: &fhtypes.ExtendedS3DestinationConfiguration{
			BucketARN: new("arn:aws:s3:::sns-fh-bucket"),
			RoleARN:   &fhRoleARN,
			BufferingHints: &fhtypes.BufferingHints{
				IntervalInSeconds: new(int32(1)),
				SizeInMBs:         new(int32(1)),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	streamARN := aws.ToString(createdStream.DeliveryStreamARN)

	// Missing SubscriptionRoleArn is rejected.
	_, err = acctA.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("firehose"), Endpoint: &streamARN,
	})
	wantCode(t, err, "InvalidParameter")

	snsRoleARN := createSNSServiceRole(t, s, "test", "sns-to-firehose-role", "sns.amazonaws.com",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"firehose:PutRecord","Resource":"*"}]}`)
	deniedRoleARN := createSNSServiceRole(t, s, "test", "sns-denied-fh-role", "sns.amazonaws.com",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"firehose:PutRecord","Resource":"*"}]}`)

	fhTopic, err := acctA.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("fh-topic")})
	if err != nil {
		t.Fatal(err)
	}

	acctAQueues := sqsClient(s, "test")
	fhDLQURL, fhDLQARN := queueARN(t, acctAQueues, "fh-dlq")
	allowDelivery(t, s, "test", fhDLQARN, "sns.amazonaws.com", *fhTopic.TopicArn)

	fhSub, err := acctA.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: fhTopic.TopicArn, Protocol: new("firehose"), Endpoint: &streamARN,
		Attributes: map[string]string{
			"SubscriptionRoleArn": deniedRoleARN,
			"RawMessageDelivery":  "true",
			"RedrivePolicy":       fmt.Sprintf(`{"deadLetterTargetArn":%q}`, fhDLQARN),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Denied role fails Firehose PutRecord and redrives to DLQ.
	if _, err = acctA.Publish(ctx, &sns.PublishInput{
		TopicArn: fhTopic.TopicArn, Message: new("fh-denied"),
	}); err != nil {
		t.Fatal(err)
	}

	fhDLQMsg := waitForSQSMessage(t, acctAQueues, fhDLQURL)
	if aws.ToString(fhDLQMsg.Body) != "fh-denied" {
		t.Fatalf("fh DLQ body = %q", aws.ToString(fhDLQMsg.Body))
	}

	// Switching to allowed role delivers to Firehose stream and flushes to S3.
	if _, err = acctA.SetSubscriptionAttributes(ctx, &sns.SetSubscriptionAttributesInput{
		SubscriptionArn: fhSub.SubscriptionArn,
		AttributeName:   new("SubscriptionRoleArn"),
		AttributeValue:  &snsRoleARN,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = acctA.Publish(ctx, &sns.PublishInput{
		TopicArn: fhTopic.TopicArn, Message: new("fh-hello"),
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	foundInS3 := false
	for time.Now().Before(deadline) {
		listed, listErr := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("sns-fh-bucket")})
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, obj := range listed.Contents {
			got, getErr := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: new("sns-fh-bucket"), Key: obj.Key})
			if getErr != nil {
				t.Fatal(getErr)
			}

			data, _ := io.ReadAll(got.Body)
			_ = got.Body.Close()
			if strings.Contains(string(data), "fh-hello") {
				foundInS3 = true
				break
			}
		}

		if foundInS3 {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if !foundInS3 {
		t.Fatal("Firehose delivery from SNS never flushed to S3")
	}
}

func allowServiceOnClient(t *testing.T, fnClient *lambda.Client, functionName, principal, sourceARN string) {
	t.Helper()
	if _, err := fnClient.AddPermission(context.Background(), &lambda.AddPermissionInput{
		FunctionName: new(functionName),
		StatementId:  new("allow-sns"),
		Action:       new("lambda:InvokeFunction"),
		Principal:    new(principal),
		SourceArn:    new(sourceARN),
	}); err != nil {
		t.Fatal(err)
	}
}

func createSNSServiceRole(t *testing.T, s *httptest.Server, key, name, servicePrincipal, inlinePolicy string) string {
	t.Helper()
	ctx := context.Background()
	ic := iamClient(s, key, "us-east-1")
	trust := fmt.Sprintf(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",`+
			`"Principal":{"Service":%q},"Action":"sts:AssumeRole"}]}`,
		servicePrincipal,
	)
	created, err := ic.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 new(name),
		AssumeRolePolicyDocument: new(trust),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = ic.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       new(name),
		PolicyName:     new("inline"),
		PolicyDocument: new(inlinePolicy),
	}); err != nil {
		t.Fatal(err)
	}

	return aws.ToString(created.Role.Arn)
}

func TestSNSPlatformApplicationsAndEndpoints(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")

	// Reject unsupported platform.
	_, err := c.CreatePlatformApplication(ctx, &sns.CreatePlatformApplicationInput{
		Name: new("bad"), Platform: new("UNKNOWN"), Attributes: map[string]string{"PlatformCredential": "secret"},
	})
	wantCode(t, err, "InvalidParameter")

	app, err := c.CreatePlatformApplication(ctx, &sns.CreatePlatformApplicationInput{
		Name:       new("mobile-app"),
		Platform:   new("GCM"),
		Attributes: map[string]string{"PlatformCredential": "api-key-123"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Idempotent create returns same ARN; conflicting attributes fail.
	again, err := c.CreatePlatformApplication(ctx, &sns.CreatePlatformApplicationInput{
		Name:       new("mobile-app"),
		Platform:   new("GCM"),
		Attributes: map[string]string{"PlatformCredential": "api-key-123"},
	})
	if err != nil || aws.ToString(again.PlatformApplicationArn) != aws.ToString(app.PlatformApplicationArn) {
		t.Fatalf("idempotent CreatePlatformApplication = %+v, %v", again, err)
	}

	_, err = c.CreatePlatformApplication(ctx, &sns.CreatePlatformApplicationInput{
		Name:       new("mobile-app"),
		Platform:   new("GCM"),
		Attributes: map[string]string{"PlatformCredential": "different"},
	})
	wantCode(t, err, "InvalidParameter")

	// GetPlatformApplicationAttributes omits credentials and reports Enabled=true.
	appAttrs, err := c.GetPlatformApplicationAttributes(ctx, &sns.GetPlatformApplicationAttributesInput{
		PlatformApplicationArn: app.PlatformApplicationArn,
	})
	if err != nil || appAttrs.Attributes["Enabled"] != "true" || appAttrs.Attributes["PlatformCredential"] != "" {
		t.Fatalf("appAttrs = %+v, %v", appAttrs, err)
	}

	apps, err := c.ListPlatformApplications(ctx, &sns.ListPlatformApplicationsInput{})
	if err != nil || len(apps.PlatformApplications) != 1 {
		t.Fatalf("ListPlatformApplications = %+v, %v", apps, err)
	}

	ep, err := c.CreatePlatformEndpoint(ctx, &sns.CreatePlatformEndpointInput{
		PlatformApplicationArn: app.PlatformApplicationArn,
		Token:                  new("device-token-1"),
		CustomUserData:         new("user-42"),
	})
	if err != nil {
		t.Fatal(err)
	}

	epAttrs, err := c.GetEndpointAttributes(ctx, &sns.GetEndpointAttributesInput{EndpointArn: ep.EndpointArn})
	if err != nil || epAttrs.Attributes["Token"] != "device-token-1" || epAttrs.Attributes["Enabled"] != "true" ||
		epAttrs.Attributes["CustomUserData"] != "user-42" {
		t.Fatalf("epAttrs = %+v, %v", epAttrs, err)
	}

	eps, err := c.ListEndpointsByPlatformApplication(ctx, &sns.ListEndpointsByPlatformApplicationInput{
		PlatformApplicationArn: app.PlatformApplicationArn,
	})
	if err != nil || len(eps.Endpoints) != 1 ||
		aws.ToString(eps.Endpoints[0].EndpointArn) != aws.ToString(ep.EndpointArn) {
		t.Fatalf("ListEndpointsByPlatformApplication = %+v, %v", eps, err)
	}

	// Direct Publish via TargetArn honors per-platform JSON structure.
	if _, err = c.Publish(ctx, &sns.PublishInput{
		TargetArn:        ep.EndpointArn,
		MessageStructure: new("json"),
		Message:          new(`{"default":"fallback","GCM":"{\"data\":{\"msg\":\"hi\"}}"}`),
	}); err != nil {
		t.Fatal(err)
	}

	recs := deliveries(t, s, 1)
	if recs[0].Target != aws.ToString(ep.EndpointArn) || recs[0].Payload != `{"data":{"msg":"hi"}}` {
		t.Fatalf("direct push delivery = %+v", recs[0])
	}

	// Publish via TargetArn also works for topics, and mutual exclusivity is enforced.
	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("push-topic")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, TargetArn: ep.EndpointArn, Message: new("conflict"),
	})
	wantCode(t, err, "InvalidParameter")

	dlqURL, dlqARN := queueARN(t, q, "push-dlq")
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("application"), Endpoint: ep.EndpointArn,
		Attributes: map[string]string{"RedrivePolicy": fmt.Sprintf(`{"deadLetterTargetArn":%q}`, dlqARN)},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{
		TargetArn:        topic.TopicArn,
		MessageStructure: new("json"),
		Message:          new(`{"default":"topic-default","GCM":"topic-gcm"}`),
	}); err != nil {
		t.Fatal(err)
	}

	recs = deliveries(t, s, 2)
	if recs[0].Target != aws.ToString(ep.EndpointArn) || recs[0].Payload != "topic-gcm" {
		t.Fatalf("topic application delivery = %+v", recs[0])
	}

	// Disabling endpoint returns EndpointDisabledException on direct Publish and triggers DLQ on topic Publish.
	if _, err = c.SetEndpointAttributes(ctx, &sns.SetEndpointAttributesInput{
		EndpointArn: ep.EndpointArn, Attributes: map[string]string{"Enabled": "false"},
	}); err != nil {
		t.Fatal(err)
	}

	var epDisabled *snstypes.EndpointDisabledException
	_, err = c.Publish(ctx, &sns.PublishInput{TargetArn: ep.EndpointArn, Message: new("blocked")})
	if !errors.As(err, &epDisabled) {
		t.Fatalf("disabled endpoint Publish err = %v", err)
	}

	if _, err = c.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("disabled-push")}); err != nil {
		t.Fatal(err)
	}

	dlqMsg := waitForSQSMessage(t, q, dlqURL)
	if aws.ToString(dlqMsg.MessageAttributes["ErrorCode"].StringValue) != "EndpointDisabled" {
		t.Fatalf("disabled endpoint DLQ attrs = %+v", dlqMsg.MessageAttributes)
	}

	// Disabling platform app returns PlatformApplicationDisabledException.
	if _, err = c.SetPlatformApplicationAttributes(ctx, &sns.SetPlatformApplicationAttributesInput{
		PlatformApplicationArn: app.PlatformApplicationArn, Attributes: map[string]string{"Enabled": "false"},
	}); err != nil {
		t.Fatal(err)
	}

	var appDisabled *snstypes.PlatformApplicationDisabledException
	_, err = c.Publish(ctx, &sns.PublishInput{TargetArn: ep.EndpointArn, Message: new("app-off")})
	if !errors.As(err, &appDisabled) {
		t.Fatalf("disabled app Publish err = %v", err)
	}

	// Inventory exposes platform-application and platform-endpoint.
	var inv struct {
		Resources []map[string]any `json:"resources"`
	}
	if internal(t, s, http.MethodGet, "/_tinystack/resources", &inv) != http.StatusOK {
		t.Fatal("failed to read inventory")
	}

	seenApp, seenEP := false, false
	for _, r := range inv.Resources {
		if r["arn"] == aws.ToString(app.PlatformApplicationArn) && r["type"] == "platform-application" {
			seenApp = true
		}

		if r["arn"] == aws.ToString(ep.EndpointArn) && r["type"] == "platform-endpoint" {
			seenEP = true
		}
	}

	if !seenApp || !seenEP {
		t.Fatalf("inventory missing platform resources: seenApp=%v seenEP=%v", seenApp, seenEP)
	}

	if _, err = c.DeleteEndpoint(ctx, &sns.DeleteEndpointInput{EndpointArn: ep.EndpointArn}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeletePlatformApplication(ctx, &sns.DeletePlatformApplicationInput{
		PlatformApplicationArn: app.PlatformApplicationArn,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSNSDataProtectionPolicy(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")

	validPolicy := `{"Name":"pii","Version":"2021-06-01","Statement":[{"DataDirection":"Inbound",` +
		`"Principal":["*"],"DataIdentifier":["arn:aws:dataprotection::aws:data-identifier/EmailAddress"],` +
		`"Operation":{"Deny":{}}}]}`
	updatedPolicy := `{"Name":"pii-v2","Version":"2021-06-01","Statement":[{"DataDirection":"Outbound",` +
		`"Principal":["*"],"DataIdentifier":["arn:aws:dataprotection::aws:data-identifier/CreditCardNumber"],` +
		`"Operation":{"Audit":{}}}]}`

	// Invalid policy fails CreateTopic.
	_, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name:                 new("bad-dpp"),
		DataProtectionPolicy: new(`{"Version":"2012-10-17","Statement":[]}`),
	})
	wantCode(t, err, "InvalidParameter")

	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name:                 new("dpp-topic"),
		DataProtectionPolicy: new(validPolicy),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetDataProtectionPolicy(ctx, &sns.GetDataProtectionPolicyInput{ResourceArn: topic.TopicArn})
	if err != nil || aws.ToString(got.DataProtectionPolicy) != validPolicy {
		t.Fatalf("GetDataProtectionPolicy = %+v, %v", got, err)
	}

	if _, err = c.PutDataProtectionPolicy(ctx, &sns.PutDataProtectionPolicyInput{
		ResourceArn: topic.TopicArn, DataProtectionPolicy: new(updatedPolicy),
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.GetDataProtectionPolicy(ctx, &sns.GetDataProtectionPolicyInput{ResourceArn: topic.TopicArn})
	if err != nil || aws.ToString(got.DataProtectionPolicy) != updatedPolicy {
		t.Fatalf("updated GetDataProtectionPolicy = %+v, %v", got, err)
	}

	if _, err = c.PutDataProtectionPolicy(ctx, &sns.PutDataProtectionPolicyInput{
		ResourceArn: topic.TopicArn, DataProtectionPolicy: new("{}"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.GetDataProtectionPolicy(ctx, &sns.GetDataProtectionPolicyInput{ResourceArn: topic.TopicArn})
	if err != nil || aws.ToString(got.DataProtectionPolicy) != "" {
		t.Fatalf("cleared GetDataProtectionPolicy = %+v, %v", got, err)
	}
}

func TestSNSSMSManagementAndSandbox(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")

	// SetSMSAttributes / GetSMSAttributes validation and filtering.
	_, err := c.SetSMSAttributes(ctx, &sns.SetSMSAttributesInput{
		Attributes: map[string]string{"DefaultSMSType": "Bogus"},
	})
	wantCode(t, err, "InvalidParameter")

	if _, err = c.SetSMSAttributes(ctx, &sns.SetSMSAttributesInput{
		Attributes: map[string]string{
			"DefaultSMSType":    "Transactional",
			"DefaultSenderID":   "TinyStack",
			"MonthlySpendLimit": "15.50",
		},
	}); err != nil {
		t.Fatal(err)
	}

	allAttrs, err := c.GetSMSAttributes(ctx, &sns.GetSMSAttributesInput{})
	if err != nil || allAttrs.Attributes["DefaultSMSType"] != "Transactional" ||
		allAttrs.Attributes["DefaultSenderID"] != "TinyStack" {
		t.Fatalf("GetSMSAttributes = %+v, %v", allAttrs, err)
	}

	filtered, err := c.GetSMSAttributes(ctx, &sns.GetSMSAttributesInput{Attributes: []string{"DefaultSenderID"}})
	if err != nil || len(filtered.Attributes) != 1 || filtered.Attributes["DefaultSenderID"] != "TinyStack" {
		t.Fatalf("filtered GetSMSAttributes = %+v, %v", filtered, err)
	}

	// Opt-out and origination numbers.
	opted, err := c.CheckIfPhoneNumberIsOptedOut(ctx, &sns.CheckIfPhoneNumberIsOptedOutInput{
		PhoneNumber: new("+15555550101"),
	})
	if err != nil || opted.IsOptedOut {
		t.Fatalf("CheckIfPhoneNumberIsOptedOut = %+v, %v", opted, err)
	}

	optedList, err := c.ListPhoneNumbersOptedOut(ctx, &sns.ListPhoneNumbersOptedOutInput{})
	if err != nil || len(optedList.PhoneNumbers) != 0 {
		t.Fatalf("ListPhoneNumbersOptedOut = %+v, %v", optedList, err)
	}

	if _, err = c.OptInPhoneNumber(ctx, &sns.OptInPhoneNumberInput{PhoneNumber: new("+15555550101")}); err != nil {
		t.Fatal(err)
	}

	orig, err := c.ListOriginationNumbers(ctx, &sns.ListOriginationNumbersInput{})
	if err != nil || len(orig.PhoneNumbers) != 0 {
		t.Fatalf("ListOriginationNumbers = %+v, %v", orig, err)
	}

	// Sandbox account status and OTP verification flow.
	status, err := c.GetSMSSandboxAccountStatus(ctx, &sns.GetSMSSandboxAccountStatusInput{})
	if err != nil || !status.IsInSandbox {
		t.Fatalf("GetSMSSandboxAccountStatus = %+v, %v", status, err)
	}

	phone := "+15555550123"
	if _, err = c.CreateSMSSandboxPhoneNumber(ctx, &sns.CreateSMSSandboxPhoneNumberInput{
		PhoneNumber:  new(phone),
		LanguageCode: snstypes.LanguageCodeStringEnUs,
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListSMSSandboxPhoneNumbers(ctx, &sns.ListSMSSandboxPhoneNumbersInput{})
	if err != nil || len(listed.PhoneNumbers) != 1 ||
		listed.PhoneNumbers[0].Status != snstypes.SMSSandboxPhoneNumberVerificationStatusPending {
		t.Fatalf("pending sandbox list = %+v, %v", listed, err)
	}

	// Read OTP from /_tinystack/messages.
	var msgs struct {
		Messages []struct {
			To   string `json:"to"`
			Body string `json:"body"`
		} `json:"messages"`
	}
	if internal(t, s, http.MethodGet, "/_tinystack/messages?channel=sms", &msgs) != http.StatusOK ||
		len(msgs.Messages) == 0 {
		t.Fatalf("sandbox OTP message missing: %+v", msgs)
	}

	otp := regexp.MustCompile(`\d{6}`).FindString(msgs.Messages[0].Body)
	if otp == "" {
		t.Fatalf("could not extract 6-digit OTP from %q", msgs.Messages[0].Body)
	}

	var verifyErr *snstypes.VerificationException
	_, err = c.VerifySMSSandboxPhoneNumber(ctx, &sns.VerifySMSSandboxPhoneNumberInput{
		PhoneNumber: new(phone), OneTimePassword: new("999999"),
	})
	if otp == "999999" {
		_, err = c.VerifySMSSandboxPhoneNumber(ctx, &sns.VerifySMSSandboxPhoneNumberInput{
			PhoneNumber: new(phone), OneTimePassword: new("000000"),
		})
	}

	if !errors.As(err, &verifyErr) {
		t.Fatalf("wrong OTP err = %v", err)
	}

	if _, err = c.VerifySMSSandboxPhoneNumber(ctx, &sns.VerifySMSSandboxPhoneNumberInput{
		PhoneNumber: new(phone), OneTimePassword: new(otp),
	}); err != nil {
		t.Fatal(err)
	}

	listed, err = c.ListSMSSandboxPhoneNumbers(ctx, &sns.ListSMSSandboxPhoneNumbersInput{})
	if err != nil || len(listed.PhoneNumbers) != 1 ||
		listed.PhoneNumbers[0].Status != snstypes.SMSSandboxPhoneNumberVerificationStatusVerified {
		t.Fatalf("verified sandbox list = %+v, %v", listed, err)
	}

	if _, err = c.DeleteSMSSandboxPhoneNumber(ctx, &sns.DeleteSMSSandboxPhoneNumberInput{
		PhoneNumber: new(phone),
	}); err != nil {
		t.Fatal(err)
	}

	var notFoundErr *snstypes.ResourceNotFoundException
	_, err = c.DeleteSMSSandboxPhoneNumber(ctx, &sns.DeleteSMSSandboxPhoneNumberInput{PhoneNumber: new(phone)})
	if !errors.As(err, &notFoundErr) {
		t.Fatalf("delete missing sandbox err = %v", err)
	}
}

func TestSNSMetricsExpanded(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := snsClient(s, "test")
	q := sqsClient(s, "test")
	cw := cloudwatchClient(s)

	topic, err := c.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("metrics-exp")})
	if err != nil {
		t.Fatal(err)
	}

	// Filtered subscription.
	_, filteredQueueARN := queueARN(t, q, "filtered-q")
	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &filteredQueueARN,
		Attributes: map[string]string{"FilterPolicy": `{"kind":["important"]}`},
	}); err != nil {
		t.Fatal(err)
	}

	// Failing subscription with working DLQ.
	_, goodDLQARN := queueARN(t, q, "good-dlq")
	missing1URL, missing1ARN := queueARN(t, q, "missing-1")
	if _, err = q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &missing1URL}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &missing1ARN,
		Attributes: map[string]string{"RedrivePolicy": fmt.Sprintf(`{"deadLetterTargetArn":%q}`, goodDLQARN)},
	}); err != nil {
		t.Fatal(err)
	}

	// Failing subscription with missing DLQ.
	badDLQURL, badDLQARN := queueARN(t, q, "bad-dlq")
	if _, err = q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &badDLQURL}); err != nil {
		t.Fatal(err)
	}

	missing2URL, missing2ARN := queueARN(t, q, "missing-2")
	if _, err = q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &missing2URL}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: &missing2ARN,
		Attributes: map[string]string{"RedrivePolicy": fmt.Sprintf(`{"deadLetterTargetArn":%q}`, badDLQARN)},
	}); err != nil {
		t.Fatal(err)
	}

	// Publish 1: no attributes -> drops on filtered-q (both FilteredOut & FilteredOut-NoMessageAttributes).
	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: new("12345"),
	}); err != nil {
		t.Fatal(err)
	}

	// Publish 2: non-matching attribute -> drops on filtered-q (FilteredOut only).
	if _, err = c.Publish(ctx, &sns.PublishInput{
		TopicArn: topic.TopicArn, Message: new("1234567890"),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"kind": {DataType: new("String"), StringValue: new("other")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	dims := []cwtypes.Dimension{{Name: new("TopicName"), Value: new("metrics-exp")}}
	checkSum := func(metricName string, unit cwtypes.StandardUnit, want float64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			out, statErr := cw.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
				Namespace:  new("AWS/SNS"),
				MetricName: new(metricName),
				Dimensions: dims,
				StartTime:  new(time.Now().Add(-5 * time.Minute)),
				EndTime:    new(time.Now().Add(5 * time.Minute)),
				Period:     new(int32(60)),
				Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
				Unit:       unit,
			})
			if statErr == nil {
				var sum float64
				for _, dp := range out.Datapoints {
					sum += aws.ToFloat64(dp.Sum)
				}

				if sum == want {
					return
				}
			}

			time.Sleep(25 * time.Millisecond)
		}

		t.Fatalf("metric %s (%s) did not reach expected sum %v", metricName, unit, want)
	}

	// PublishSize: 5 bytes + (10 + len("kind") + len("String") + len("other") = 25) = 30 bytes.
	checkSum("PublishSize", cwtypes.StandardUnitBytes, 30)
	checkSum("NumberOfNotificationsFilteredOut", cwtypes.StandardUnitCount, 2)
	checkSum("NumberOfNotificationsFilteredOut-NoMessageAttributes", cwtypes.StandardUnitCount, 1)
	checkSum("NumberOfNotificationsRedrivenToDlq", cwtypes.StandardUnitCount, 2)
	checkSum("NumberOfNotificationsFailedToRedriveToDlq", cwtypes.StandardUnitCount, 2)
}
