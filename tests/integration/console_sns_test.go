package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func consoleSNSRequest(
	t *testing.T,
	s *httptest.Server,
	operation, account, region string,
	input map[string]any,
) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": "sns", "operation": operation, "account": account, "region": region, "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		s.URL+consoleActionsPath,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.URL)
	return r
}

func TestConsoleSNSLifecycle(t *testing.T) {
	s := server(t)
	const account, region = "000000000000", "eu-west-1"
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		response := consoleResponse(t, s, consoleSNSRequest(t, s, operation, account, region, input), http.StatusOK)
		return response["output"].(map[string]any)
	}
	var capabilities map[string]any
	if internal(t, s, http.MethodGet, consoleActionsPath, &capabilities) != http.StatusOK ||
		capabilities["sns"] != true {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	arn := write("CreateTopic", map[string]any{"Name": "console-topic", "Attributes": map[string]any{
		"DisplayName": "Console topic", "FifoTopic": "false",
	}, "Tags": []any{map[string]any{"Key": "team", "Value": "console"}}})["TopicArn"].(string)
	write(
		"SetTopicAttributes",
		map[string]any{"TopicArn": arn, "AttributeName": "DisplayName", "AttributeValue": "Updated"},
	)
	write("SetTopicAttributes", map[string]any{
		"TopicArn": arn, "AttributeName": "Policy",
		"AttributeValue": `{"Statement":{"Effect":"Allow","Principal":"*","Action":"sns:*","Resource":"*"}}`,
	})
	write(
		"TagResource",
		map[string]any{"ResourceArn": arn, "Tags": []any{map[string]any{"Key": "env", "Value": "test"}}},
	)
	write("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []any{"team"}})
	tags := write("ListTagsForResource", map[string]any{"ResourceArn": arn})["Tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["Key"] != "env" {
		t.Fatalf("tags = %+v", tags)
	}

	options := sqsClient(s, "test").Options()
	options.Region = region
	queueClient := sqs.New(options)
	url, queueARN := queueARN(t, queueClient, "console-sns-queue")
	sub := write("Subscribe", map[string]any{
		"TopicArn": arn, "Protocol": "sqs", "Endpoint": queueARN,
		"ReturnSubscriptionArn": true, "Attributes": map[string]any{
			"RawMessageDelivery": "true",
			"FilterPolicy":       `{"event":["created"]}`, "FilterPolicyScope": "MessageAttributes",
		},
	})["SubscriptionArn"].(string)
	write("Publish", map[string]any{"TopicArn": arn, "Message": "filtered", "MessageAttributes": map[string]any{
		"event": map[string]any{"DataType": "String", "StringValue": "ignored"},
	}})
	sent := write(
		"Publish",
		map[string]any{"TopicArn": arn, "Message": "delivered", "MessageAttributes": map[string]any{
			"event": map[string]any{"DataType": "String", "StringValue": "created"},
			"blob":  map[string]any{"DataType": "Binary", "BinaryValue": "AP+A"},
		}},
	)
	if sent["MessageId"] == "" {
		t.Fatal("missing message ID")
	}

	received, err := queueClient.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl: new(url), MaxNumberOfMessages: 10, MessageAttributeNames: []string{"All"},
	})
	if err != nil || len(received.Messages) != 1 {
		t.Fatalf("messages = %+v, %v", received, err)
	}

	message := received.Messages[0]
	if *message.Body != "delivered" ||
		!bytes.Equal(message.MessageAttributes["blob"].BinaryValue, []byte{0, 255, 128}) {
		t.Fatalf("message = %+v", message)
	}

	write("SetSubscriptionAttributes", map[string]any{
		"SubscriptionArn": sub, "AttributeName": "FilterPolicy",
		"AttributeValue": "{}",
	})
	write("SetSubscriptionAttributes", map[string]any{
		"SubscriptionArn": sub, "AttributeName": "FilterPolicyScope",
		"AttributeValue": "MessageBody",
	})
	write("SetSubscriptionAttributes", map[string]any{
		"SubscriptionArn": sub, "AttributeName": "FilterPolicy",
		"AttributeValue": `{"detail":{"event":["created"]}}`,
	})
	write("Publish", map[string]any{"TopicArn": arn, "Message": `{"detail":{"event":"created"}}`})
	output := write("GetSubscriptionAttributes", map[string]any{"SubscriptionArn": sub})["Attributes"].(map[string]any)
	if output["FilterPolicyScope"] != "MessageBody" || output["RawMessageDelivery"] != "true" {
		t.Fatalf("subscription attributes = %+v", output)
	}

	// The log is read through the same diagnostic endpoint used by the topic UI.
	var records map[string]any
	if internal(t, s, http.MethodGet, "/_tinystack/deliveries", &records) != http.StatusOK {
		t.Fatal("cannot load deliveries")
	}
	rows := records["deliveries"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["source"] != arn {
		t.Fatalf("deliveries = %+v", rows)
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(
			t,
			s,
			consoleSNSRequest(t, s, "GetTopicAttributes", scope.account, scope.region, map[string]any{"TopicArn": arn}),
			http.StatusNotFound,
		)
	}

	sdkOptions := snsClient(s, "test").Options()
	sdkOptions.Region = region
	client := sns.New(sdkOptions)
	attrs, err := client.GetTopicAttributes(context.Background(), &sns.GetTopicAttributesInput{TopicArn: new(arn)})
	if err != nil || attrs.Attributes["DisplayName"] != "Updated" {
		t.Fatalf("SDK topic = %+v, %v", attrs, err)
	}

	write("Unsubscribe", map[string]any{"SubscriptionArn": sub})
	write("DeleteTopic", map[string]any{"TopicArn": arn})
}

func TestConsoleSNSConfirmationAndInventory(t *testing.T) {
	s := server(t)
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		response := consoleResponse(
			t,
			s,
			consoleSNSRequest(t, s, operation, "000000000000", "us-east-1", input),
			http.StatusOK,
		)
		return response["output"].(map[string]any)
	}
	received := make(chan map[string]any, 2)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message map[string]any
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- message
	}))
	t.Cleanup(endpoint.Close)
	arn := write("CreateTopic", map[string]any{"Name": "http-console"})["TopicArn"].(string)
	sub := write("Subscribe", map[string]any{
		"TopicArn": arn, "Protocol": "http", "Endpoint": endpoint.URL,
		"ReturnSubscriptionArn": true,
	})["SubscriptionArn"].(string)
	var confirmation map[string]any
	select {
	case confirmation = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("missing HTTP confirmation")
	}
	if !strings.HasPrefix(confirmation["SubscribeURL"].(string), s.URL+"/?Action=ConfirmSubscription") {
		t.Fatalf("confirmation URL = %+v", confirmation)
	}

	attrs := write("GetTopicAttributes", map[string]any{"TopicArn": arn})["Attributes"].(map[string]any)
	if attrs["SubscriptionsConfirmed"] != "0" || attrs["SubscriptionsPending"] != "1" {
		t.Fatalf("counts = %+v", attrs)
	}

	var resources struct {
		Resources []map[string]any `json:"resources"`
	}
	if internal(t, s, http.MethodGet, "/_tinystack/resources", &resources) != http.StatusOK {
		t.Fatal("cannot load inventory")
	}
	found := false
	for _, resource := range resources.Resources {
		if resource["arn"] != arn {
			continue
		}
		row := resource["details"].(map[string]any)["Subscriptions"].([]any)[0].(map[string]any)
		if row["PendingConfirmation"] != true || row["SubscriptionArn"] != sub || row["FilterPolicyScope"] != "" {
			t.Fatalf("inventory subscription = %+v", row)
		}
		if _, ok := row["Token"]; ok {
			t.Fatal("inventory exposed confirmation token")
		}
		found = true
	}
	if !found {
		t.Fatal("missing topic inventory")
	}

	confirmed := write("ConfirmSubscription", map[string]any{"TopicArn": arn, "Token": confirmation["Token"]})
	if confirmed["SubscriptionArn"] != sub {
		t.Fatalf("confirmed = %+v", confirmed)
	}

	attrs = write("GetTopicAttributes", map[string]any{"TopicArn": arn})["Attributes"].(map[string]any)
	if attrs["SubscriptionsConfirmed"] != "1" || attrs["SubscriptionsPending"] != "0" {
		t.Fatalf("confirmed counts = %+v", attrs)
	}

	write("Publish", map[string]any{"TopicArn": arn, "Message": "hello http"})
	select {
	case notification := <-received:
		if notification["Type"] != "Notification" || notification["Message"] != "hello http" {
			t.Fatal(notification)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing HTTP notification")
	}
	write("Unsubscribe", map[string]any{"SubscriptionArn": sub})
	write("DeleteTopic", map[string]any{"TopicArn": arn})
}

func TestConsoleSNSValidationAndSignatures(t *testing.T) {
	s := server(t)
	for _, tc := range []struct {
		input             map[string]any
		operation, region string
	}{
		{map[string]any{"Name": "invalid"}, "CreateTopic", ""},
		{map[string]any{"Name": "invalid"}, "CreateTopic", "../us-east-1"},
		{map[string]any{}, "PublishBatch", "us-east-1"},
		{map[string]any{"Name": "invalid", "Unknown": true}, "CreateTopic", "us-east-1"},
	} {
		consoleResponse(
			t,
			s,
			consoleSNSRequest(t, s, tc.operation, "000000000000", tc.region, tc.input),
			http.StatusBadRequest,
		)
	}

	req := consoleSNSRequest(t, s, "CreateTopic", "000000000000", "us-east-1", map[string]any{"Name": "cross-site"})
	req.Header.Set("Origin", "https://evil.example")
	consoleResponse(t, s, req, http.StatusForbidden)
	verified := verifyingServer(t)
	for _, tc := range []struct {
		name, service, region, account string
		signed                         bool
		status                         int
	}{
		{"unsigned", "sns", "us-east-1", "111111111111", false, http.StatusForbidden},
		{"wrong-service", "sqs", "us-east-1", "111111111111", true, http.StatusForbidden},
		{"wrong-region", "sns", "eu-west-1", "111111111111", true, http.StatusForbidden},
		{"wrong-account", "sns", "us-east-1", "000000000000", true, http.StatusForbidden},
		{"valid", "sns", "us-east-1", "111111111111", true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := consoleSNSRequest(
				t,
				verified,
				"CreateTopic",
				tc.account,
				"us-east-1",
				map[string]any{"Name": "signed-" + tc.name},
			)
			if tc.signed {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				sum := sha256.Sum256(body)
				credentials := aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"}
				err = v4.NewSigner().
					SignHTTP(context.Background(), credentials, r, hex.EncodeToString(sum[:]), tc.service, tc.region, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			consoleResponse(t, verified, r, tc.status)
		})
	}
}
