package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// destinationRecord is the part of an asynchronous invocation record the
// tests check.
type destinationRecord struct {
	ResponseContext struct {
		FunctionError string `json:"functionError"`
	} `json:"responseContext"`
	RequestPayload  json.RawMessage `json:"requestPayload"`
	ResponsePayload json.RawMessage `json:"responsePayload"`
	RequestContext  struct {
		FunctionArn            string `json:"functionArn"`
		Condition              string `json:"condition"`
		ApproximateInvokeCount int    `json:"approximateInvokeCount"`
	} `json:"requestContext"`
}

// awaitBody waits for one message on a queue and returns its body.
func awaitBody(t *testing.T, c *sqs.Client, queueURL string) string {
	t.Helper()
	var body string
	waitFor(t, "a message on "+queueURL, func() bool {
		body = receiveOne(t, c, queueURL)
		return body != ""
	})

	return body
}

func invocationRecord(t *testing.T, body string) destinationRecord {
	t.Helper()
	var r destinationRecord
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("record %s: %v", body, err)
	}

	return r
}

func TestLambdaDestinations(t *testing.T) {
	s := server(t)
	c := lambdaClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "ok", "app.echo")
	createFunction(t, c, "broken", "app.fail")
	successURL, successARN := queueARN(t, q, "success")
	failureURL, failureARN := queueARN(t, q, "failure")

	for name, in := range map[string]*sdklambda.PutFunctionEventInvokeConfigInput{
		"retries": {MaximumRetryAttempts: aws.Int32(3)},
		"age":     {MaximumEventAgeInSeconds: aws.Int32(59)},
		"destination": {DestinationConfig: &lambdatypes.DestinationConfig{
			OnSuccess: &lambdatypes.OnSuccess{Destination: aws.String("arn:aws:s3:::bucket")},
		}},
		"itself": {DestinationConfig: &lambdatypes.DestinationConfig{
			OnFailure: &lambdatypes.OnFailure{
				Destination: aws.String("arn:aws:lambda:us-east-1:000000000000:function:ok"),
			},
		}},
	} {
		in.FunctionName = aws.String("ok")
		if _, err := c.PutFunctionEventInvokeConfig(
			ctx,
			in,
		); !isType[*lambdatypes.InvalidParameterValueException](
			err,
		) {
			t.Fatalf("%s: %v", name, err)
		}
	}

	_, err := c.GetFunctionEventInvokeConfig(ctx, &sdklambda.GetFunctionEventInvokeConfigInput{
		FunctionName: aws.String("ok"),
	})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("no config: %v", err)
	}

	_, err = c.UpdateFunctionEventInvokeConfig(ctx, &sdklambda.UpdateFunctionEventInvokeConfigInput{
		FunctionName: aws.String("ok"), MaximumRetryAttempts: aws.Int32(1),
	})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("update without config: %v", err)
	}

	put, err := c.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
		FunctionName: aws.String("ok"),
		DestinationConfig: &lambdatypes.DestinationConfig{
			OnSuccess: &lambdatypes.OnSuccess{Destination: &successARN},
		},
	})
	if err != nil || aws.ToString(put.FunctionArn) != "arn:aws:lambda:us-east-1:000000000000:function:ok:$LATEST" ||
		aws.ToString(put.DestinationConfig.OnSuccess.Destination) != successARN || put.MaximumRetryAttempts != nil {
		t.Fatalf("put %+v, %v", put, err)
	}

	if _, err = c.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: aws.String("ok"), InvocationType: lambdatypes.InvocationTypeEvent,
		Payload: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatal(err)
	}

	success := invocationRecord(t, awaitBody(t, q, successURL))
	if success.RequestContext.Condition != "Success" || string(success.RequestPayload) != `{"n":1}` ||
		success.RequestContext.FunctionArn != "arn:aws:lambda:us-east-1:000000000000:function:ok" ||
		success.RequestContext.ApproximateInvokeCount != 1 || len(success.ResponsePayload) == 0 {
		t.Fatalf("success %+v", success)
	}

	// A failure is retried as configured and then sent to OnFailure.
	if _, err = c.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
		FunctionName: aws.String("broken"), MaximumRetryAttempts: aws.Int32(1),
		DestinationConfig: &lambdatypes.DestinationConfig{
			OnFailure: &lambdatypes.OnFailure{Destination: &failureARN},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: aws.String("broken"), InvocationType: lambdatypes.InvocationTypeEvent,
		Payload: []byte(`{"n":2}`),
	}); err != nil {
		t.Fatal(err)
	}

	failure := invocationRecord(t, awaitBody(t, q, failureURL))
	if failure.RequestContext.Condition != "RetriesExhausted" || failure.RequestContext.ApproximateInvokeCount != 2 ||
		failure.ResponseContext.FunctionError != "Unhandled" ||
		string(failure.ResponsePayload) != `{"errorType":"ValueError","errorMessage":"boom"}` {
		t.Fatalf("failure %+v", failure)
	}

	// Update changes only what it names; an empty destination removes it.
	updated, err := c.UpdateFunctionEventInvokeConfig(ctx, &sdklambda.UpdateFunctionEventInvokeConfigInput{
		FunctionName:             aws.String("broken"),
		MaximumEventAgeInSeconds: aws.Int32(120),
		DestinationConfig: &lambdatypes.DestinationConfig{
			OnFailure: &lambdatypes.OnFailure{Destination: aws.String("")},
		},
	})
	if err != nil || aws.ToInt32(updated.MaximumRetryAttempts) != 1 ||
		aws.ToInt32(updated.MaximumEventAgeInSeconds) != 120 ||
		updated.DestinationConfig.OnFailure.Destination != nil {
		t.Fatalf("updated %+v, %v", updated, err)
	}

	listed, err := c.ListFunctionEventInvokeConfigs(ctx, &sdklambda.ListFunctionEventInvokeConfigsInput{
		FunctionName: aws.String("broken"),
	})
	if err != nil || len(listed.FunctionEventInvokeConfigs) != 1 {
		t.Fatalf("listed %+v, %v", listed, err)
	}

	if _, err = c.DeleteFunctionEventInvokeConfig(ctx, &sdklambda.DeleteFunctionEventInvokeConfigInput{
		FunctionName: aws.String("broken"),
	}); err != nil {
		t.Fatal(err)
	}

	// Deleting a function deletes its configurations.
	if _, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{FunctionName: aws.String("ok")}); err != nil {
		t.Fatal(err)
	}

	createFunction(t, c, "ok", "app.echo")
	_, err = c.GetFunctionEventInvokeConfig(ctx, &sdklambda.GetFunctionEventInvokeConfigInput{
		FunctionName: aws.String("ok"),
	})
	if !isType[*lambdatypes.ResourceNotFoundException](err) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestLambdaDestinationsSNSAndEventBridge(t *testing.T) {
	s := server(t)
	c := lambdaClient(s, "test")
	q := sqsClient(s, "test")
	topics := snsClient(s, "test")
	events := eventsClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "ok", "app.echo")
	topicURL, topicQueue := queueARN(t, q, "from-topic")
	busURL, busQueue := queueARN(t, q, "from-bus")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("results")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &topicQueue,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = events.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: aws.String("results"), EventPattern: aws.String(`{"source":["lambda"]}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = events.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule:    aws.String("results"),
		Targets: []ebtypes.Target{{Id: aws.String("queue"), Arn: &busQueue}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, destination := range []string{
		aws.ToString(topic.TopicArn), "arn:aws:events:us-east-1:000000000000:event-bus/default",
	} {
		if _, err = c.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
			FunctionName: aws.String("ok"),
			DestinationConfig: &lambdatypes.DestinationConfig{
				OnSuccess: &lambdatypes.OnSuccess{Destination: &destination},
			},
		}); err != nil {
			t.Fatal(err)
		}

		if _, err = c.Invoke(ctx, &sdklambda.InvokeInput{
			FunctionName: aws.String("ok"), InvocationType: lambdatypes.InvocationTypeEvent,
		}); err != nil {
			t.Fatal(err)
		}

		// Wait for the delivery before the next configuration replaces it.
		if destination == aws.ToString(topic.TopicArn) {
			if r := invocationRecord(t, awaitBody(t, q, topicURL)); r.RequestContext.Condition != "Success" {
				t.Fatalf("topic record %+v", r)
			}
		}
	}

	var event struct {
		Source     string            `json:"source"`
		DetailType string            `json:"detail-type"`
		Detail     destinationRecord `json:"detail"`
	}
	if err = json.Unmarshal([]byte(awaitBody(t, q, busURL)), &event); err != nil ||
		event.DetailType != "Lambda Function Invocation Result - Success" ||
		event.Detail.RequestContext.Condition != "Success" {
		t.Fatalf("event %+v, %v", event, err)
	}
}
