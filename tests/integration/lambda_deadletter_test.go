package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestLambdaDeadLetterDelivery(t *testing.T) {
	const worker = "arn:aws:lambda:us-east-1:" + deliveryAccount + ":function:broken"
	const payload = `{"original":"event"}`
	for _, target := range []string{"sqs", "sns"} {
		for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
			for _, choice := range []roleChoice{roleAllowed, roleNoPermission, roleNotTrusted} {
				t.Run(fmt.Sprintf("%s/%s/%s", target, mode, choice), func(t *testing.T) {
					t.Parallel()
					s, _ := deliveryServer(t, mode)
					ctx := context.Background()
					queues := sqsClient(s, "test")
					url, queue := queueARN(t, queues, "dead")
					arn, action := queue, "sqs:SendMessage"
					if target == "sns" {
						topic, err := snsClient(s, "test").CreateTopic(ctx, &sns.CreateTopicInput{Name: new("dead")})
						mustAuthorize(t, err)
						arn, action = *topic.TopicArn, "sns:Publish"
						// SNS's default owner grant would otherwise allow a same-account role even without an identity Allow.
						_, err = snsClient(s, "test").SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
							TopicArn: &arn, AttributeName: new("Policy"),
							AttributeValue: new(fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",`+
								`"Principal":{"AWS":"arn:aws:iam::000000000000:role/other"},"Action":"sns:Publish","Resource":%q}]}`, arn)),
						})
						mustAuthorize(t, err)
						allowDelivery(t, s, "test", queue, "sns.amazonaws.com", arn)
						_, err = snsClient(s, "test").Subscribe(ctx, &sns.SubscribeInput{
							TopicArn: &arn, Protocol: new("sqs"), Endpoint: &queue,
						})
						mustAuthorize(t, err)
					}

					trust, permissions := choice.documentsFor("lambda.amazonaws.com", worker, arn, action)
					role := createDeliveryRole(t, s, "execution", trust, permissions)
					functions := lambdaClient(s, "test")
					created, err := functions.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
						FunctionName: new(
							"broken",
						),
						Runtime: lambdatypes.RuntimePython312,
						Role:    &role,
						Handler: new("app.fail"),
						Code: &lambdatypes.FunctionCode{
							ZipFile: zipOf(t, "app.py", "def fail(e, c): raise ValueError('boom')\n"),
						},
						DeadLetterConfig: &lambdatypes.DeadLetterConfig{TargetArn: &arn},
						Publish:          true,
					})
					mustAuthorize(t, err)
					if created.DeadLetterConfig == nil || aws.ToString(created.DeadLetterConfig.TargetArn) != arn {
						t.Fatalf("created configuration = %+v", created)
					}

					_, err = functions.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
						FunctionName: new("broken"), MaximumRetryAttempts: new(int32(0)),
					})
					mustAuthorize(t, err)
					_, err = functions.Invoke(ctx, &sdklambda.InvokeInput{
						FunctionName: new(
							"broken",
						),
						InvocationType: lambdatypes.InvocationTypeEvent,
						Payload:        []byte(payload),
					})
					mustAuthorize(t, err)
					wantRoleChecks(t, s, mode, choice, role)
					want := choice == roleAllowed || mode == config.AuthorizationSoft
					if !want {
						// The failed delivery has been recorded by the authorization check above.
						if body := receiveOne(t, queues, url); body != "" {
							t.Fatalf("denied DLQ delivery reached queue: %s", body)
						}

						return
					}

					var message sqstypes.Message
					waitFor(t, "the original event in the DLQ", func() bool {
						out, readErr := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
							QueueUrl: &url, MessageAttributeNames: []string{"All"}, MaxNumberOfMessages: 10,
						})
						mustAuthorize(t, readErr)
						for _, m := range out.Messages {
							body := aws.ToString(m.Body)
							attrs := map[string]struct{ Type, Value string }{}
							if target == "sns" {
								var envelope struct {
									MessageAttributes map[string]struct{ Type, Value string }
									Message           string
								}

								if decodeErr := json.Unmarshal([]byte(body), &envelope); decodeErr != nil {
									t.Fatal(decodeErr)
								}

								body, attrs = envelope.Message, envelope.MessageAttributes
							}

							if body != payload {
								continue
							}

							message = m
							if target == "sns" {
								if attrs["RequestID"].Value == "" || attrs["ErrorCode"].Type != "Number" ||
									attrs["ErrorCode"].Value != "200" || attrs["ErrorMessage"].Value != "boom" {
									t.Fatalf("SNS attributes = %+v", attrs)
								}
							} else if aws.ToString(m.MessageAttributes["RequestID"].StringValue) == "" ||
								aws.ToString(m.MessageAttributes["ErrorCode"].DataType) != "Number" ||
								aws.ToString(m.MessageAttributes["ErrorCode"].StringValue) != "200" ||
								aws.ToString(m.MessageAttributes["ErrorMessage"].StringValue) != "boom" {
								t.Fatalf("SQS attributes = %+v", m.MessageAttributes)
							}

							return true
						}

						return false
					})
					if message.Body == nil {
						t.Fatal("missing original event")
					}
				})
			}
		}
	}
}

func TestLambdaDeadLetterConfiguration(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	functions := lambdaClient(s, "test")
	queues := sqsClient(s, "test")
	_, arn := queueARN(t, queues, "dead")
	createFunction(t, functions, "worker", "app.echo")
	for _, invalid := range []string{
		"bad", "arn:aws:s3:::bucket", "arn:aws:sqs:us-east-1:000000000000:",
		"arn:aws:sqs:us-east-1:000000000000:dead.fifo", "arn:aws:sns:us-east-1:000000000000:dead.fifo",
		"arn:aws:sns:us-east-1:000000000000:topic:extra", "arn::sqs:us-east-1:000000000000:queue",
	} {
		_, err := functions.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
			FunctionName: new("worker"), DeadLetterConfig: &lambdatypes.DeadLetterConfig{TargetArn: &invalid},
		})
		if !isType[*lambdatypes.InvalidParameterValueException](err) {
			t.Fatalf("invalid %s: %v", invalid, err)
		}
	}

	_, err := functions.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("worker"), DeadLetterConfig: &lambdatypes.DeadLetterConfig{TargetArn: &arn},
	})
	mustAuthorize(t, err)
	got, err := functions.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: new("worker"),
	})
	mustAuthorize(t, err)
	if got.DeadLetterConfig == nil || aws.ToString(got.DeadLetterConfig.TargetArn) != arn {
		t.Fatalf("configuration = %+v", got)
	}

	_, err = functions.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("worker"), Description: new("keeps DLQ"),
	})
	mustAuthorize(t, err)
	got, err = functions.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: new("worker"),
	})
	mustAuthorize(t, err)
	if got.DeadLetterConfig == nil || aws.ToString(got.DeadLetterConfig.TargetArn) != arn {
		t.Fatal("unrelated update removed dead-letter config")
	}

	cleared, err := functions.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("worker"), DeadLetterConfig: &lambdatypes.DeadLetterConfig{},
	})
	mustAuthorize(t, err)
	if cleared.DeadLetterConfig != nil && strings.TrimSpace(aws.ToString(cleared.DeadLetterConfig.TargetArn)) != "" {
		t.Fatalf("clear = %+v", cleared.DeadLetterConfig)
	}
}

// A failure after retries goes to both targets, with different message shapes. A published version uses the
// current function-level DLQ, even if the function had no DLQ when the version was published.
func TestLambdaDeadLetterAndDestination(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	functions := lambdaClient(s, "test")
	queues := sqsClient(s, "test")
	createFunction(t, functions, "broken", "app.fail")
	version, err := functions.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: new("broken")})
	mustAuthorize(t, err)
	url, arn := queueARN(t, queues, "dead")
	destinationURL, destinationARN := queueARN(t, queues, "destination")
	_, err = functions.UpdateFunctionConfiguration(ctx, &sdklambda.UpdateFunctionConfigurationInput{
		FunctionName: new("broken"), DeadLetterConfig: &lambdatypes.DeadLetterConfig{TargetArn: &arn},
	})
	mustAuthorize(t, err)
	_, err = functions.PutFunctionEventInvokeConfig(ctx, &sdklambda.PutFunctionEventInvokeConfigInput{
		FunctionName:         new("broken"),
		Qualifier:            version.Version,
		MaximumRetryAttempts: new(int32(1)),
		DestinationConfig: &lambdatypes.DestinationConfig{
			OnFailure: &lambdatypes.OnFailure{Destination: &destinationARN},
		},
	})
	mustAuthorize(t, err)
	const payload = `{"original":true}`
	_, err = functions.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: new("broken"), Qualifier: version.Version,
		InvocationType: lambdatypes.InvocationTypeEvent, Payload: []byte(payload),
	})
	mustAuthorize(t, err)
	if body := awaitBody(t, queues, url); body != payload {
		t.Fatalf("DLQ body = %s", body)
	}

	record := invocationRecord(t, awaitBody(t, queues, destinationURL))
	if string(record.RequestPayload) != payload || record.RequestContext.ApproximateInvokeCount != 2 ||
		record.RequestContext.Condition != "RetriesExhausted" {
		t.Fatalf("destination record = %+v", record)
	}
}

func TestLambdaDeadLetterCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	stacks := cfnClient(s)
	functions := lambdaClient(s, "test")
	_, target := queueARN(t, sqsClient(s, "test"), "dead")
	template := func(deadLetter string) string {
		return `Resources:
  Worker:
    Type: AWS::Lambda::Function
    Properties:
      FunctionName: cfn-worker
      Runtime: python3.12
      Handler: index.handler
      Role: arn:aws:iam::000000000000:role/execution
      Code:
        ZipFile: "def handler(event, context): return event"
` + deadLetter
	}

	_, err := stacks.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    new("dead-letter"),
		TemplateBody: new(template("      DeadLetterConfig:\n        TargetArn: " + target + "\n")),
	})
	mustAuthorize(t, err)
	wantStatus(ctx, t, stacks, "dead-letter", cfntypes.StackStatusCreateComplete)
	got, err := functions.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: new("cfn-worker"),
	})
	mustAuthorize(t, err)
	if got.DeadLetterConfig == nil || aws.ToString(got.DeadLetterConfig.TargetArn) != target {
		t.Fatalf("CFN-created DLQ = %+v", got.DeadLetterConfig)
	}

	_, err = stacks.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new("dead-letter"), TemplateBody: new(template("")),
	})
	mustAuthorize(t, err)
	wantStatus(ctx, t, stacks, "dead-letter", cfntypes.StackStatusUpdateComplete)
	got, err = functions.GetFunctionConfiguration(ctx, &sdklambda.GetFunctionConfigurationInput{
		FunctionName: new("cfn-worker"),
	})
	mustAuthorize(t, err)
	if got.DeadLetterConfig != nil && aws.ToString(got.DeadLetterConfig.TargetArn) != "" {
		t.Fatalf("CFN-removed DLQ = %+v", got.DeadLetterConfig)
	}
}
