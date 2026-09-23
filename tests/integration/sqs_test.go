package integration_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func sqsClient(s *httptest.Server, key string) *sqs.Client {
	return sqs.New(
		sqs.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(s.URL),
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

func createQueue(t *testing.T, c *sqs.Client, name string, attributes map[string]string) string {
	t.Helper()
	out, err := c.CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: &name, Attributes: attributes})
	if err != nil {
		t.Fatal(err)
	}

	return *out.QueueUrl
}

func TestSQSLifecycle(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "orders", map[string]string{"VisibilityTimeout": "60"})
	if queueURL != s.URL+"/000000000000/orders" {
		t.Fatalf("queue URL %s", queueURL)
	}

	if again := createQueue(t, c, "orders", map[string]string{"VisibilityTimeout": "60"}); again != queueURL {
		t.Fatal("identical CreateQueue was not idempotent")
	}

	_, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("orders")})
	var exists *types.QueueNameExists
	if !errors.As(err, &exists) || exists.ErrorCode() != "QueueAlreadyExists" {
		t.Fatalf("conflicting CreateQueue: %v", err)
	}

	_, err = c.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("fifo"),
		Attributes: map[string]string{"FifoQueue": "true"},
	})
	var badName *types.InvalidAttributeName
	if !errors.As(err, &badName) {
		t.Fatalf("unsupported attribute accepted: %v", err)
	}

	createQueue(t, c, "orders-dlq", nil)
	createQueue(t, c, "emails", nil)

	got, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("orders")})
	if err != nil || *got.QueueUrl != queueURL {
		t.Fatal(got, err)
	}

	page, err := c.ListQueues(
		ctx,
		&sqs.ListQueuesInput{QueueNamePrefix: aws.String("orders"), MaxResults: aws.Int32(1)},
	)
	if err != nil || len(page.QueueUrls) != 1 || page.QueueUrls[0] != queueURL || page.NextToken == nil {
		t.Fatal(page, err)
	}

	page, err = c.ListQueues(ctx, &sqs.ListQueuesInput{
		QueueNamePrefix: aws.String("orders"),
		MaxResults:      aws.Int32(1),
		NextToken:       page.NextToken,
	})
	if err != nil || len(page.QueueUrls) != 1 || !strings.HasSuffix(page.QueueUrls[0], "/orders-dlq") {
		t.Fatal(page, err)
	}

	sent, err := c.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &queueURL,
		MessageBody: aws.String("hello ✓"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"color":  {DataType: aws.String("String"), StringValue: aws.String("blue")},
			"count":  {DataType: aws.String("Number"), StringValue: aws.String("3")},
			"blob":   {DataType: aws.String("Binary"), BinaryValue: []byte{0, 1, 2}},
			"ignore": {DataType: aws.String("String.custom"), StringValue: aws.String("x")},
		},
	})
	if err != nil || sent.MD5OfMessageAttributes == nil {
		t.Fatal(sent, err)
	}

	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    &queueURL,
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
	})
	if err != nil || len(received.Messages) != 1 {
		t.Fatal(received, err)
	}

	msg := received.Messages[0]
	if *msg.MessageId != *sent.MessageId || *msg.Body != "hello ✓" || len(msg.MessageAttributes) != 4 ||
		*msg.MD5OfMessageAttributes != *sent.MD5OfMessageAttributes ||
		string(msg.MessageAttributes["blob"].BinaryValue) != "\x00\x01\x02" ||
		msg.Attributes["ApproximateReceiveCount"] != "1" || msg.Attributes["SenderId"] != "test" {
		t.Fatalf("received %#v", msg)
	}

	empty, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(empty.Messages) != 0 {
		t.Fatal("in-flight message was delivered again", err)
	}

	if _, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 0,
	}); err != nil {
		t.Fatal(err)
	}

	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              &queueURL,
		MessageAttributeNames: []string{"co.*", "color"},
		//nolint:staticcheck // The deprecated member is still sent by older clients.
		AttributeNames: []types.QueueAttributeName{"ApproximateReceiveCount"},
	})
	if err != nil || len(again.Messages) != 1 || again.Messages[0].Attributes["ApproximateReceiveCount"] != "2" ||
		len(again.Messages[0].MessageAttributes) != 1 {
		t.Fatal(again, err)
	}

	_, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 5,
	})
	var stale *types.ReceiptHandleIsInvalid
	if !errors.As(err, &stale) {
		t.Fatalf("stale receipt changed visibility: %v", err)
	}
	// Deleting with a stale receipt handle succeeds without deleting.
	if _, err = c.DeleteMessage(
		ctx,
		&sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: msg.ReceiptHandle},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: &queueURL, ReceiptHandle: again.Messages[0].ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: again.Messages[0].ReceiptHandle, VisibilityTimeout: 0,
	})
	if !errors.As(err, &stale) {
		t.Fatalf("deleted message changed visibility: %v", err)
	}

	_, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: aws.String("garbage")})
	if !errors.As(err, &stale) {
		t.Fatalf("malformed receipt handle accepted: %v", err)
	}

	if _, err = c.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &queueURL}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("orders")})
	var gone *types.QueueDoesNotExist
	if !errors.As(err, &gone) || gone.ErrorCode() != "AWS.SimpleQueueService.NonExistentQueue" {
		t.Fatalf("deleted queue still resolves: %v", err)
	}
}

func TestSQSValidation(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "limits", map[string]string{"MaximumMessageSize": "1024"})
	for name, in := range map[string]*sqs.SendMessageInput{
		"too large":         {MessageBody: aws.String(strings.Repeat("x", 1025))},
		"invalid character": {MessageBody: aws.String("bad \x00")},
		"reserved name": {MessageBody: aws.String("ok"), MessageAttributes: map[string]types.MessageAttributeValue{
			"AWS.trace": {DataType: aws.String("String"), StringValue: aws.String("x")},
		}},
		"bad number": {MessageBody: aws.String("ok"), MessageAttributes: map[string]types.MessageAttributeValue{
			"n": {DataType: aws.String("Number"), StringValue: aws.String("NaN")},
		}},
	} {
		in.QueueUrl = &queueURL
		if _, err := c.SendMessage(ctx, in); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}

	if _, err := c.ReceiveMessage(
		ctx,
		&sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 11},
	); err == nil {
		t.Fatal("MaxNumberOfMessages 11 accepted")
	}

	team := sqsClient(s, "team-b")
	isolated := createQueue(t, team, "limits", nil)
	if isolated == queueURL {
		t.Fatal("accounts share queue URLs")
	}

	_, err := c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &isolated, MessageBody: aws.String("x")})
	var gone *types.QueueDoesNotExist
	if !errors.As(err, &gone) {
		t.Fatalf("cross-account queue URL accepted: %v", err)
	}

	listed, err := c.ListQueues(ctx, &sqs.ListQueuesInput{})
	if err != nil || len(listed.QueueUrls) != 1 {
		t.Fatal(listed, err)
	}
}

func TestSQSLongPolling(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "poll", nil)

	start := time.Now()
	done := make(chan *sqs.ReceiveMessageOutput, 1)
	go func() {
		out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 10})
		if err != nil {
			t.Error(err)
		}

		done <- out
	}()

	time.Sleep(200 * time.Millisecond)
	if _, err := c.SendMessage(
		ctx,
		&sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: aws.String("wake")},
	); err != nil {
		t.Fatal(err)
	}

	out := <-done
	if out == nil || len(out.Messages) != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("long poll did not wake on send: %v", out)
	}

	// An expiring visibility timeout wakes a waiting receiver.
	start = time.Now()
	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 1})
	if err != nil || len(again.Messages) != 0 {
		t.Fatal("in-flight message delivered", err)
	}

	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("long poll returned before its wait time")
	}

	if _, err = c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: &queueURL, ReceiptHandle: out.Messages[0].ReceiptHandle, VisibilityTimeout: 1,
	}); err != nil {
		t.Fatal(err)
	}

	start = time.Now()
	again, err = c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, WaitTimeSeconds: 10})
	if err != nil || len(again.Messages) != 1 || time.Since(start) > 5*time.Second {
		t.Fatal("expired visibility did not wake the long poll", err)
	}
}

func TestSQSConcurrentReceivers(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	queueURL := createQueue(t, c, "work", nil)
	const total = 60
	for i := range total {
		if _, err := c.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: &queueURL, MessageBody: aws.String(fmt.Sprint(i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				out, err := c.ReceiveMessage(
					ctx,
					&sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 10},
				)
				if err != nil {
					t.Error(err)
					return
				}

				if len(out.Messages) == 0 {
					return
				}

				mu.Lock()
				for _, m := range out.Messages {
					seen[*m.Body]++
				}

				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	if len(seen) != total {
		t.Fatalf("received %d distinct messages", len(seen))
	}

	for body, n := range seen {
		if n != 1 {
			t.Fatalf("message %s claimed %d times", body, n)
		}
	}
}

type queryErrorXML struct {
	Error struct {
		Type, Code, Message string
	}

	RequestID string `xml:"RequestId"`
}

type receiveXML struct {
	Result struct {
		Messages []struct {
			MessageID              string `xml:"MessageId"`
			ReceiptHandle          string
			MD5OfBody              string
			Body                   string
			MD5OfMessageAttributes string
			Attributes             []struct{ Name, Value string } `xml:"Attribute"`
			MessageAttributes      []struct {
				Name  string
				Value struct{ StringValue, DataType string }
			} `xml:"MessageAttribute"`
		} `xml:"Message"`
	} `xml:"ReceiveMessageResult"`
}

func queryCall(
	t *testing.T,
	s *httptest.Server,
	path string,
	params url.Values,
	headers map[string]string,
) (status int, header http.Header, body []byte) {
	t.Helper()
	if params.Get("Version") == "" {
		params.Set("Version", "2012-11-05")
	}

	req, err := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(params.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	if body, err = io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header, body
}

func TestSQSQueryProtocol(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()

	status, headers, body := queryCall(t, s, "/", url.Values{
		"Action":            {"CreateQueue"},
		"QueueName":         {"legacy"},
		"Attribute.1.Name":  {"VisibilityTimeout"},
		"Attribute.1.Value": {"45"},
	}, nil)
	var created struct {
		QueueURL  string `xml:"CreateQueueResult>QueueUrl"`
		RequestID string `xml:"ResponseMetadata>RequestId"`
	}

	if status != 200 || headers.Get("Content-Type") != "text/xml" || xml.Unmarshal(body, &created) != nil ||
		created.QueueURL != s.URL+"/000000000000/legacy" || created.RequestID != headers.Get("x-amzn-RequestId") ||
		!strings.Contains(string(body), `xmlns="http://queue.amazonaws.com/doc/2012-11-05/"`) {
		t.Fatalf("%d %s", status, body)
	}
	// JSON and Query share state and attributes.
	if again := createQueue(t, c, "legacy", map[string]string{"VisibilityTimeout": "45"}); again != created.QueueURL {
		t.Fatal(again)
	}

	// Legacy SDKs post to the queue URL; flattened message attributes use Name/Value entries.
	status, _, body = queryCall(t, s, "/000000000000/legacy", url.Values{
		"Action":                               {"SendMessage"},
		"QueueUrl":                             {created.QueueURL},
		"MessageBody":                          {"from query"},
		"MessageAttribute.1.Name":              {"color"},
		"MessageAttribute.1.Value.DataType":    {"String"},
		"MessageAttribute.1.Value.StringValue": {"blue"},
		"MessageAttribute.2.Name":              {"raw"},
		"MessageAttribute.2.Value.DataType":    {"Binary"},
		"MessageAttribute.2.Value.BinaryValue": {"AAEC"},
	}, nil)
	var sent struct {
		MessageID     string `xml:"SendMessageResult>MessageId"`
		BodyMD5       string `xml:"SendMessageResult>MD5OfMessageBody"`
		AttributesMD5 string `xml:"SendMessageResult>MD5OfMessageAttributes"`
	}

	if status != 200 || xml.Unmarshal(body, &sent) != nil || sent.AttributesMD5 == "" {
		t.Fatalf("%d %s", status, body)
	}

	received, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &created.QueueURL, MessageAttributeNames: []string{"All"},
	})
	if err != nil || len(received.Messages) != 1 || *received.Messages[0].MessageId != sent.MessageID ||
		*received.Messages[0].MD5OfBody != sent.BodyMD5 ||
		*received.Messages[0].MD5OfMessageAttributes != sent.AttributesMD5 ||
		string(received.Messages[0].MessageAttributes["raw"].BinaryValue) != "\x00\x01\x02" {
		t.Fatal(received, err)
	}

	if _, err = c.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &created.QueueURL,
		MessageBody: aws.String("to query <&>"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"size": {DataType: aws.String("Number"), StringValue: aws.String("7")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	status, _, body = queryCall(t, s, "/", url.Values{
		"Action":                 {"ReceiveMessage"},
		"QueueUrl":               {created.QueueURL},
		"AttributeName.1":        {"All"},
		"MessageAttributeName.1": {"All"},
		"MaxNumberOfMessages":    {"10"},
	}, nil)
	var polled receiveXML
	if status != 200 || xml.Unmarshal(body, &polled) != nil || len(polled.Result.Messages) != 1 {
		t.Fatalf("%d %s", status, body)
	}

	got := polled.Result.Messages[0]
	if got.Body != "to query <&>" || len(got.Attributes) != 4 || len(got.MessageAttributes) != 1 ||
		got.MessageAttributes[0].Name != "size" || got.MessageAttributes[0].Value.StringValue != "7" {
		t.Fatalf("%s", body)
	}

	status, _, body = queryCall(t, s, "/", url.Values{
		"Action": {"DeleteMessage"}, "QueueUrl": {created.QueueURL}, "ReceiptHandle": {got.ReceiptHandle},
	}, nil)
	if status != 200 || !strings.Contains(string(body), "<DeleteMessageResponse") ||
		strings.Contains(string(body), "DeleteMessageResult") {
		t.Fatalf("%d %s", status, body)
	}

	for _, tc := range []struct {
		params  url.Values
		headers map[string]string
		name    string
		code    string
		status  int
	}{
		{
			name:   "missing queue",
			params: url.Values{"Action": {"GetQueueUrl"}, "QueueName": {"nope"}},
			code:   "AWS.SimpleQueueService.NonExistentQueue", status: 400,
		},
		{
			name:   "invalid receipt",
			params: url.Values{"Action": {"DeleteMessage"}, "QueueUrl": {created.QueueURL}, "ReceiptHandle": {"x"}},
			code:   "ReceiptHandleIsInvalid", status: 404,
		},
		{name: "unknown action", params: url.Values{"Action": {"PurgeQueue"}}, code: "InvalidAction", status: 400},
		{name: "missing action", params: url.Values{"QueueName": {"x"}}, code: "MissingAction", status: 400},
		{
			name:   "unknown parameter",
			params: url.Values{"Action": {"ListQueues"}, "Bogus": {"1"}},
			code:   "InvalidParameterValue", status: 400,
		},
		{
			name:   "list gap",
			params: url.Values{"Action": {"ReceiveMessage"}, "QueueUrl": {created.QueueURL}, "AttributeName.2": {"All"}},
			code:   "InvalidParameterValue", status: 400,
		},
		{
			name:   "wrong version",
			params: url.Values{"Action": {"ListQueues"}, "Version": {"2008-01-01"}},
			code:   "NoSuchVersion", status: 400,
		},
		{
			name:   "signed for another service",
			params: url.Values{"Action": {"ListQueues"}},
			headers: map[string]string{
				"Authorization": "AWS4-HMAC-SHA256 Credential=test/20260923/us-east-1/dynamodb/aws4_request",
			},
			code: "InvalidAction", status: 400,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, headers, body := queryCall(t, s, "/", tc.params, tc.headers)
			var e queryErrorXML
			if err := xml.Unmarshal(body, &e); err != nil || status != tc.status || e.Error.Code != tc.code ||
				e.Error.Type != "Sender" || headers.Get("Content-Type") != "text/xml" || e.RequestID == "" {
				t.Fatalf("%d %s", status, body)
			}
		})
	}
}

func TestSQSJSONQueryCompatibleErrors(t *testing.T) {
	s := server(t)
	req, err := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(`{"QueueName":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS.GetQueueUrl")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	var e struct {
		Type string `json:"__type"`
	}

	if err = json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 400 || e.Type != "QueueDoesNotExist" ||
		resp.Header.Get("X-Amzn-Query-Error") != "AWS.SimpleQueueService.NonExistentQueue;Sender" {
		t.Fatalf("%d %s %v", resp.StatusCode, e.Type, resp.Header)
	}

	// A JSON target with a form body is contradictory and fails before dispatch.
	req, err = http.NewRequest(http.MethodPost, s.URL, strings.NewReader("Action=ListQueues"))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Amz-Target", "AmazonSQS.ListQueues")
	resp, err = s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != 400 || resp.Header.Get("Content-Type") != "application/x-amz-json-1.0" {
		t.Fatalf("contradictory request: %d", resp.StatusCode)
	}
}
