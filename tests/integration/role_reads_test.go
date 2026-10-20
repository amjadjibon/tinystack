package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
)

// createWorker creates the function "worker" that runs as role.
func createWorker(t *testing.T, c *sdklambda.Client, role string) {
	t.Helper()
	_, err := c.CreateFunction(context.Background(), &sdklambda.CreateFunctionInput{
		FunctionName: new("worker"), Runtime: lambdatypes.RuntimePython312, Role: &role, Handler: new("app.echo"),
		Code: &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.py", "def echo(e, c): return e\n")},
	})
	mustAuthorize(t, err)
}

// wantRoleChecks checks that every check recorded for role is of the mode and decided as the choice sets the role
// up to be, and that there is one.
func wantRoleChecks(t *testing.T, s *httptest.Server, mode string, choice roleChoice, role string) []gateway.Record {
	t.Helper()
	var records []gateway.Record
	waitFor(t, "a check under the role", func() bool {
		records = roleChecks(t, s, role)
		return len(records) > 0
	})

	for i := range records {
		auth := records[i].Authorization
		if auth == nil || auth.Mode != mode || auth.Allowed != (choice == roleAllowed) {
			t.Fatalf("check %d = %+v, want allowed = %t in %s mode", i, auth, choice == roleAllowed, mode)
		}
	}

	return records
}

// A function's event source mapping reads its queue as the function's execution role, which Lambda checks as the
// mapping is created, as AWS does.
func TestRoleReadsMappingQueue(t *testing.T) {
	const worker = "arn:aws:lambda:us-east-1:" + deliveryAccount + ":function:worker"
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
		for _, choice := range roleChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, runtime := deliveryServer(t, mode)
				queues := sqsClient(s, "test")
				queueURL, queue := queueARN(t, queues, "work")
				trust, permissions := choice.documentsFor("lambda.amazonaws.com", worker, queue,
					"sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes")
				role := createDeliveryRole(t, s, "execution", trust, permissions)
				functions := lambdaClient(s, "test")
				createWorker(t, functions, role)
				_, err := functions.CreateEventSourceMapping(
					context.Background(),
					&sdklambda.CreateEventSourceMappingInput{
						FunctionName: new("worker"), EventSourceArn: &queue,
					},
				)
				if mode == config.AuthorizationEnforce && choice != roleAllowed {
					if err == nil || !strings.Contains(err.Error(),
						"The provided execution role does not have permissions to call ReceiveMessage on SQS") {
						t.Fatalf("CreateEventSourceMapping: %v", err)
					}

					wantRoleChecks(t, s, mode, choice, role)
					return
				}

				mustAuthorize(t, err)
				_, err = queues.SendMessage(context.Background(), &sqs.SendMessageInput{
					QueueUrl: &queueURL, MessageBody: new("work"),
				})
				mustAuthorize(t, err)
				if !functionInvoked(runtime) {
					t.Fatal("the function was not invoked")
				}

				wantRoleChecks(t, s, mode, choice, role)
			})
		}
	}
}

// A mapping created while its role could read keeps polling only while it can: a role that loses its permissions is
// reported in the mapping's last processing result, and nothing more is read.
func TestRoleReadsMappingRevoked(t *testing.T) {
	const worker = "arn:aws:lambda:us-east-1:" + deliveryAccount + ":function:worker"
	s, runtime := deliveryServer(t, config.AuthorizationEnforce)
	ctx := context.Background()
	queues := sqsClient(s, "test")
	queueURL, queue := queueARN(t, queues, "work")
	trust, permissions := roleAllowed.documentsFor("lambda.amazonaws.com", worker, queue,
		"sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes")
	role := createDeliveryRole(t, s, "execution", trust, permissions)
	functions := lambdaClient(s, "test")
	createWorker(t, functions, role)
	mapping, err := functions.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
		FunctionName: new("worker"), EventSourceArn: &queue,
	})
	mustAuthorize(t, err)
	send := func() {
		_, sendErr := queues.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &queueURL, MessageBody: new("work")})
		mustAuthorize(t, sendErr)
	}

	// The poll in flight already read with the permission, so it takes one more message.
	send()
	if !functionInvoked(runtime) {
		t.Fatal("the function was not invoked")
	}

	// The mapping's creation and two polls checked the three reads each; the second is waiting for a message.
	waitFor(t, "the second poll", func() bool { return len(roleChecks(t, s, role)) >= 9 })
	_, err = iamClient(s, "test", "us-east-1").DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
		RoleName: new("execution"), PolicyName: new("send"),
	})
	mustAuthorize(t, err)
	send()
	if !functionInvoked(runtime) {
		t.Fatal("the message the poll in flight read was not processed")
	}

	waitFor(t, "the problem", func() bool {
		got, getErr := functions.GetEventSourceMapping(ctx, &sdklambda.GetEventSourceMappingInput{UUID: mapping.UUID})
		mustAuthorize(t, getErr)
		return strings.Contains(aws.ToString(got.LastProcessingResult),
			"PROBLEM: The provided execution role does not have permissions to call ReceiveMessage on SQS")
	})
	send()
	if functionInvoked(runtime) {
		t.Fatal("a role without permissions read the queue")
	}
}

// Stream mappings need the role to describe the stream, get its iterators and read its records.
func TestRoleReadsMappingStreams(t *testing.T) {
	const worker = "arn:aws:lambda:us-east-1:" + deliveryAccount + ":function:worker"
	for _, source := range []string{"kinesis", "dynamodb"} {
		actions := []string{"dynamodb:DescribeStream", "dynamodb:GetShardIterator", "dynamodb:GetRecords"}
		if source == "kinesis" {
			actions = []string{
				"kinesis:DescribeStream", "kinesis:ListShards", "kinesis:GetShardIterator", "kinesis:GetRecords",
			}
		}

		for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
			for _, choice := range []roleChoice{roleAllowed, roleNoPermission, roleNotTrusted} {
				t.Run(fmt.Sprintf("%s/%s/%s", source, mode, choice), func(t *testing.T) {
					s, _ := deliveryServer(t, mode)
					ctx := context.Background()
					var stream string
					if source == "kinesis" {
						_, err := kinesisClient(s).CreateStream(ctx, &kinesis.CreateStreamInput{
							StreamName: new("events"), ShardCount: new(int32(1)),
						})
						mustAuthorize(t, err)
						stream = "arn:aws:kinesis:us-east-1:" + deliveryAccount + ":stream/events"
					} else {
						stream = streamTable(
							t,
							client(s, "test", "us-east-1"),
							"orders",
							dynamotypes.StreamViewTypeNewImage,
						)
					}

					trust, permissions := choice.documentsFor("lambda.amazonaws.com", worker, "*", actions...)
					role := createDeliveryRole(t, s, "execution", trust, permissions)
					functions := lambdaClient(s, "test")
					createWorker(t, functions, role)
					_, err := functions.CreateEventSourceMapping(ctx, &sdklambda.CreateEventSourceMappingInput{
						FunctionName: new("worker"), EventSourceArn: &stream,
						StartingPosition: lambdatypes.EventSourcePositionTrimHorizon,
					})
					if mode == config.AuthorizationEnforce && choice != roleAllowed {
						if err == nil || !strings.Contains(err.Error(), "Cannot access stream "+stream) {
							t.Fatalf("CreateEventSourceMapping: %v", err)
						}
					} else {
						mustAuthorize(t, err)
					}

					wantRoleChecks(t, s, mode, choice, role)
				})
			}
		}
	}
}

// A pipe reads its source as its role, so a role that may send to the target but not read the source moves nothing.
func TestRoleReadsPipes(t *testing.T) {
	const pipe = "arn:aws:pipes:us-east-1:" + deliveryAccount + ":pipe/orders"
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
		for _, choice := range roleChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, _ := deliveryServer(t, mode)
				queues := sqsClient(s, "test")
				sourceURL, source := queueARN(t, queues, "source")
				targetURL, target := queueARN(t, queues, "target")
				_, err := queues.SendMessage(context.Background(), &sqs.SendMessageInput{
					QueueUrl: &sourceURL, MessageBody: new("piped"),
				})
				mustAuthorize(t, err)
				trust, permissions := choice.documentsFor(
					"pipes.amazonaws.com", pipe, "*",
					"sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes", "sqs:SendMessage",
				)
				role := createDeliveryRole(t, s, "pipe", trust, permissions)
				in := sqsPipe("orders", source, target)
				in.RoleArn = &role
				_, err = pipesClient(s).CreatePipe(context.Background(), in)
				mustAuthorize(t, err)

				wantRoleChecks(t, s, mode, choice, role)
				want := choice == roleAllowed || mode == config.AuthorizationSoft
				if got := awaitMessage(t, queues, targetURL, 3*time.Second); got != want {
					t.Fatalf("delivered = %t, want %t", got, want)
				}

				if choice == roleAllowed {
					waitFor(t, "the checks of the reads", func() bool {
						var actions []string
						for _, record := range roleChecks(t, s, role) {
							for _, check := range record.Authorization.Checks {
								actions = append(actions, check.Action)
							}
						}

						return slices.Contains(actions, "sqs:ReceiveMessage") &&
							slices.Contains(
								actions,
								"sqs:DeleteMessage",
							) && slices.Contains(actions, "sqs:GetQueueAttributes")
					})
				}
			})
		}
	}
}

// A subscription filter writes to Kinesis as its role, which logs.amazonaws.com must be allowed to assume for the
// log group. AWS tests the destination as the filter is saved.
func TestRoleLogsSubscriptionKinesis(t *testing.T) {
	const group = "arn:aws:logs:us-east-1:" + deliveryAccount + ":log-group:app:*"
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
		for _, choice := range roleChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, _ := deliveryServer(t, mode)
				ctx := context.Background()
				_, err := kinesisClient(s).CreateStream(ctx, &kinesis.CreateStreamInput{
					StreamName: new("events"), ShardCount: new(int32(1)),
				})
				mustAuthorize(t, err)
				trust, permissions := choice.documentsFor("logs.amazonaws.com", group, "*", "kinesis:PutRecord")
				role := createDeliveryRole(t, s, "logs", trust, permissions)
				c := logsClient(s, "test")
				_, err = c.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{LogGroupName: new("app")})
				mustAuthorize(t, err)
				_, err = c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
					LogGroupName: new("app"), LogStreamName: new("web"),
				})
				mustAuthorize(t, err)
				_, err = c.PutSubscriptionFilter(ctx, &cloudwatchlogs.PutSubscriptionFilterInput{
					LogGroupName:  new("app"),
					FilterName:    new("all"),
					FilterPattern: new(""),
					DestinationArn: new(
						"arn:aws:kinesis:us-east-1:" + deliveryAccount + ":stream/events",
					),
					RoleArn: &role,
				})
				if mode == config.AuthorizationEnforce && choice != roleAllowed {
					if err == nil || !strings.Contains(err.Error(), "Could not deliver test message") {
						t.Fatalf("PutSubscriptionFilter: %v", err)
					}

					wantRoleChecks(t, s, mode, choice, role)
					return
				}

				mustAuthorize(t, err)
				_, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
					LogGroupName: new("app"), LogStreamName: new("web"),
					LogEvents: []logstypes.InputLogEvent{{Timestamp: new(time.Now().UnixMilli()), Message: new("hi")}},
				})
				mustAuthorize(t, err)
				// The test as the filter is saved, and the delivery of the event.
				waitFor(t, "both checks", func() bool { return len(roleChecks(t, s, role)) >= 2 })
				wantRoleChecks(t, s, mode, choice, role)
			})
		}
	}
}

// A rule's republish action publishes with the action's role, which must be allowed iot:Publish on the topic.
func TestRoleDeliveryIoTRepublish(t *testing.T) {
	const rule = "arn:aws:iot:us-east-1:" + deliveryAccount + ":rule/orders"
	const topic = "arn:aws:iot:us-east-1:" + deliveryAccount + ":topic/out/orders"
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
		for _, choice := range roleChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, _ := deliveryServer(t, mode)
				trust, permissions := choice.documentsFor("iot.amazonaws.com", rule, topic, "iot:Publish")
				role := createDeliveryRole(t, s, "republish", trust, permissions)
				createRule(t, iotClient(s, "test"), "orders", "SELECT * FROM 'shop/orders'", []iottypes.Action{{
					Republish: &iottypes.RepublishAction{Topic: new("out/orders"), RoleArn: &role},
				}}, nil)
				_, err := dataClient(s, "test").Publish(context.Background(), &iotdataplane.PublishInput{
					Topic: new("shop/orders"), Payload: []byte(`{"id":1}`),
				})
				mustAuthorize(t, err)
				records := wantRoleChecks(t, s, mode, choice, role)
				if choice == roleAllowed {
					check := records[0].Authorization.Checks[0]
					if check.Action != "iot:Publish" || check.Resource != topic {
						t.Fatalf("check = %+v", check)
					}
				}
			})
		}
	}
}

// A delivery stream writes to its bucket as the destination's role, which firehose.amazonaws.com must be allowed to
// assume for the stream; a write that is denied is not delivered.
func TestRoleDeliveryFirehose(t *testing.T) {
	const stream = "arn:aws:firehose:us-east-1:" + deliveryAccount + ":deliverystream/orders"
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft} {
		for _, choice := range roleChoices {
			t.Run(fmt.Sprintf("%s/%s", mode, choice), func(t *testing.T) {
				s, _ := deliveryServer(t, mode)
				ctx := context.Background()
				buckets := s3Client(s, "test", "us-east-1")
				_, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("lake")})
				mustAuthorize(t, err)
				trust, permissions := choice.documentsFor("firehose.amazonaws.com", stream, "arn:aws:s3:::lake/*",
					"s3:PutObject")
				role := createDeliveryRole(t, s, "firehose", trust, permissions)
				fh := firehoseClient(s)
				_, err = fh.CreateDeliveryStream(ctx, &firehose.CreateDeliveryStreamInput{
					DeliveryStreamName: new("orders"),
					ExtendedS3DestinationConfiguration: &fhtypes.ExtendedS3DestinationConfiguration{
						BucketARN: new("arn:aws:s3:::lake"),
						RoleARN:   &role,
						Prefix:    new("orders/"),
						BufferingHints: &fhtypes.BufferingHints{
							IntervalInSeconds: new(int32(0)),
							SizeInMBs:         new(int32(1)),
						},
					},
				})
				mustAuthorize(t, err)
				_, err = fh.PutRecord(ctx, &firehose.PutRecordInput{
					DeliveryStreamName: new("orders"), Record: &fhtypes.Record{Data: []byte("order")},
				})
				mustAuthorize(t, err)
				records := wantRoleChecks(t, s, mode, choice, role)
				if choice == roleAllowed && records[0].Authorization.Checks[0].Action != "s3:PutObject" {
					t.Fatalf("check = %+v", records[0].Authorization.Checks[0])
				}

				listed := func() int {
					out, listErr := buckets.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
						Bucket: new("lake"), Prefix: new("orders/"),
					})
					mustAuthorize(t, listErr)
					return len(out.Contents)
				}

				if want := choice == roleAllowed || mode == config.AuthorizationSoft; want {
					waitFor(t, "the object", func() bool { return listed() > 0 })
				} else {
					time.Sleep(1500 * time.Millisecond)
					if listed() != 0 {
						t.Fatal("a denied write was delivered")
					}
				}
			})
		}
	}
}
