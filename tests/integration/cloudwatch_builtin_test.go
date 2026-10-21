package integration_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	restapi "github.com/aws/aws-sdk-go-v2/service/apigateway"
	resttypes "github.com/aws/aws-sdk-go-v2/service/apigateway/types"
	apigw "github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apigwtypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	kintypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// builtin is the sum and sample count of a built-in metric over the last hour, and its unit.
type builtin struct {
	unit  string
	sum   float64
	count float64
}

func dimensions(pairs ...string) []cwtypes.Dimension {
	var dims []cwtypes.Dimension
	for i := 0; i < len(pairs); i += 2 {
		dims = append(dims, cwtypes.Dimension{Name: new(pairs[i]), Value: new(pairs[i+1])})
	}

	return dims
}

// builtinMetric waits for the services' next flush to CloudWatch until the metric satisfies done, then returns it.
func builtinMetric(
	t *testing.T, c *cloudwatch.Client, namespace, name string, dims []cwtypes.Dimension, done func(builtin) bool,
) builtin {
	t.Helper()
	var got builtin
	deadline := time.Now().Add(15 * time.Second)
	for {
		now := time.Now().UTC()
		out, err := c.GetMetricStatistics(context.Background(), &cloudwatch.GetMetricStatisticsInput{
			Namespace: new(namespace), MetricName: new(name), Dimensions: dims, Period: new(int32(60)),
			StartTime: new(now.Add(-time.Hour)), EndTime: new(now.Add(time.Minute)),
			Statistics: []cwtypes.Statistic{cwtypes.StatisticSum, cwtypes.StatisticSampleCount},
		})
		if err != nil {
			t.Fatal(err)
		}

		got = builtin{}
		for _, p := range out.Datapoints {
			got.sum += aws.ToFloat64(p.Sum)
			got.count += aws.ToFloat64(p.SampleCount)
			got.unit = string(p.Unit)
		}

		if done(got) {
			return got
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s %s %v: %+v", namespace, name, aws.ToString(dims[0].Value), got)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// expectBuiltin checks a built-in metric's sum and its unit once it has reached want.
func expectBuiltin(
	t *testing.T, c *cloudwatch.Client, namespace, name, unit string, dims []cwtypes.Dimension, want float64,
) builtin {
	t.Helper()
	got := builtinMetric(t, c, namespace, name, dims, func(b builtin) bool { return b.sum >= want && b.count > 0 })
	if got.sum != want || got.unit != unit {
		t.Fatalf("%s %s: sum %v unit %q, want %v %q", namespace, name, got.sum, got.unit, want, unit)
	}

	return got
}

func TestBuiltinLambdaMetrics(t *testing.T) {
	s := server(t)
	c := cloudwatchClient(s)
	functions := lambdaClient(s, "test")
	ctx := context.Background()
	createFunction(t, functions, "good", "app.echo")
	createFunction(t, functions, "bad", "app.fail")
	createFunction(t, functions, "capped", "app.echo")
	good, bad := dimensions("FunctionName", "good"), dimensions("FunctionName", "bad")
	_, err := c.PutMetricAlarm(ctx, &cloudwatch.PutMetricAlarmInput{
		AlarmName: new("bad-errors"), Namespace: new("AWS/Lambda"), MetricName: new("Errors"), Dimensions: bad,
		Statistic: cwtypes.StatisticSum, Period: new(int32(60)), EvaluationPeriods: new(int32(1)),
		Threshold: new(0.0), ComparisonOperator: cwtypes.ComparisonOperatorGreaterThanThreshold,
	})
	if err != nil {
		t.Fatal(err)
	}

	invoke := func(name string, kind lambdatypes.InvocationType, qualifier *string) *sdklambda.InvokeOutput {
		out, invokeErr := functions.Invoke(ctx, &sdklambda.InvokeInput{
			FunctionName: new(name), InvocationType: kind, Qualifier: qualifier, Payload: []byte(`{}`),
		})
		if invokeErr != nil {
			t.Fatal(invokeErr)
		}

		return out
	}

	invoke("good", lambdatypes.InvocationTypeRequestResponse, nil)
	invoke("good", lambdatypes.InvocationTypeRequestResponse, nil)
	invoke("good", lambdatypes.InvocationTypeEvent, nil)
	if out := invoke("bad", lambdatypes.InvocationTypeRequestResponse, nil); out.FunctionError == nil {
		t.Fatal("the failing function did not fail")
	}

	expectBuiltin(t, c, "AWS/Lambda", "Invocations", "Count", good, 3)
	expectBuiltin(t, c, "AWS/Lambda", "Errors", "Count", good, 0)
	expectBuiltin(t, c, "AWS/Lambda", "Errors", "Count", bad, 1)
	expectBuiltin(t, c, "AWS/Lambda", "Invocations", "Count", bad, 1)
	// Duration is one sample per invocation, in milliseconds.
	duration := builtinMetric(t, c, "AWS/Lambda", "Duration", good, func(b builtin) bool { return b.count == 3 })
	if duration.unit != "Milliseconds" || duration.sum < 0 {
		t.Fatalf("%+v", duration)
	}

	// An alias reports under Resource as well as under the function.
	version, err := functions.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: new("good")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = functions.CreateAlias(ctx, &sdklambda.CreateAliasInput{
		FunctionName: new("good"), Name: new("live"), FunctionVersion: version.Version,
	}); err != nil {
		t.Fatal(err)
	}

	invoke("good", lambdatypes.InvocationTypeRequestResponse, new("live"))
	expectBuiltin(t, c, "AWS/Lambda", "Invocations", "Count",
		dimensions("FunctionName", "good", "Resource", "good:live"), 1)
	expectBuiltin(t, c, "AWS/Lambda", "Invocations", "Count", good, 4)

	// A function whose reserved concurrency is zero throttles every invocation.
	if _, err = functions.PutFunctionConcurrency(ctx, &sdklambda.PutFunctionConcurrencyInput{
		FunctionName: new("capped"), ReservedConcurrentExecutions: new(int32(0)),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = functions.Invoke(ctx, &sdklambda.InvokeInput{FunctionName: new("capped")}); err == nil {
		t.Fatal("a function without concurrency ran")
	}

	capped := dimensions("FunctionName", "capped")
	expectBuiltin(t, c, "AWS/Lambda", "Throttles", "Count", capped, 1)
	if b := builtinMetric(t, c, "AWS/Lambda", "Invocations", capped, func(builtin) bool { return true }); b.count != 0 {
		t.Fatalf("throttled invocations counted: %+v", b)
	}

	listed, err := c.ListMetrics(
		ctx,
		&cloudwatch.ListMetricsInput{Namespace: new("AWS/Lambda"), MetricName: new("Errors")},
	)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, m := range listed.Metrics {
		names = append(names, aws.ToString(m.Dimensions[0].Value))
	}

	slices.Sort(names)
	if !slices.Contains(names, "good") || !slices.Contains(names, "bad") {
		t.Fatalf("listed %v", names)
	}

	// GetMetricData reads the same points.
	now := time.Now().UTC()
	data, err := c.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime: new(now.Add(-time.Hour)), EndTime: new(now.Add(time.Minute)),
		MetricDataQueries: []cwtypes.MetricDataQuery{{
			Id: new("errors"), MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{
					Namespace: new("AWS/Lambda"), MetricName: new("Errors"), Dimensions: bad,
				},
				Period: new(int32(60)), Stat: new("Sum"),
			},
		}},
	})
	if err != nil || len(data.MetricDataResults) != 1 || len(data.MetricDataResults[0].Values) == 0 ||
		data.MetricDataResults[0].Values[0] != 1 {
		t.Fatalf("%+v %v", data, err)
	}

	// The failing invocation puts the alarm in ALARM.
	deadline := time.Now().Add(15 * time.Second)
	for {
		alarms, describeErr := c.DescribeAlarms(
			ctx,
			&cloudwatch.DescribeAlarmsInput{AlarmNames: []string{"bad-errors"}},
		)
		if describeErr != nil || len(alarms.MetricAlarms) != 1 {
			t.Fatalf("%+v %v", alarms, describeErr)
		}

		if alarms.MetricAlarms[0].StateValue == cwtypes.StateValueAlarm {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf(
				"alarm state %s: %s",
				alarms.MetricAlarms[0].StateValue,
				aws.ToString(alarms.MetricAlarms[0].StateReason),
			)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func TestBuiltinSQSMetrics(t *testing.T) {
	s := server(t)
	c := cloudwatchClient(s)
	q := sqsClient(s, "test")
	ctx := context.Background()
	url := createQueue(t, q, "jobs", nil)
	for _, body := range []string{"hello", "world"} {
		if _, err := q.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &url, MessageBody: new(body)}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := q.SendMessageBatch(
		ctx,
		&sqs.SendMessageBatchInput{QueueUrl: &url, Entries: []sqstypes.SendMessageBatchRequestEntry{
			{Id: new("a"), MessageBody: new("again")}, {Id: new("b"), MessageBody: new("more!")},
		}},
	); err != nil {
		t.Fatal(err)
	}

	received, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &url, MaxNumberOfMessages: 10})
	if err != nil || len(received.Messages) != 4 {
		t.Fatalf("%+v %v", received, err)
	}

	// Everything is in flight now.
	if empty, receiveErr := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &url}); receiveErr != nil ||
		len(empty.Messages) != 0 {
		t.Fatalf("%+v %v", empty, receiveErr)
	}

	if _, err = q.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: &url, ReceiptHandle: received.Messages[0].ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = q.DeleteMessageBatch(
		ctx,
		&sqs.DeleteMessageBatchInput{QueueUrl: &url, Entries: []sqstypes.DeleteMessageBatchRequestEntry{
			{Id: new("a"), ReceiptHandle: received.Messages[1].ReceiptHandle},
			{Id: new("b"), ReceiptHandle: received.Messages[2].ReceiptHandle},
		}},
	); err != nil {
		t.Fatal(err)
	}

	queue := dimensions("QueueName", "jobs")
	expectBuiltin(t, c, "AWS/SQS", "NumberOfMessagesSent", "Count", queue, 4)
	expectBuiltin(t, c, "AWS/SQS", "NumberOfMessagesReceived", "Count", queue, 4)
	expectBuiltin(t, c, "AWS/SQS", "NumberOfMessagesDeleted", "Count", queue, 3)
	expectBuiltin(t, c, "AWS/SQS", "NumberOfEmptyReceives", "Count", queue, 1)
	sizes := expectBuiltin(t, c, "AWS/SQS", "SentMessageSize", "Bytes", queue, 20)
	if sizes.count != 4 {
		t.Fatalf("%+v", sizes)
	}
}

func TestBuiltinDynamoDBMetrics(t *testing.T) {
	s := server(t)
	c := cloudwatchClient(s)
	d := client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := create(ctx, d, "orders"); err != nil {
		t.Fatal(err)
	}

	item := map[string]ddbtypes.AttributeValue{
		"PK": &ddbtypes.AttributeValueMemberS{Value: "a"}, "note": &ddbtypes.AttributeValueMemberS{Value: "hi"},
	}
	if _, err := d.PutItem(ctx, &dynamodb.PutItemInput{TableName: new("orders"), Item: item}); err != nil {
		t.Fatal(err)
	}

	// An eventually consistent read costs half a unit, a strongly consistent one a whole unit.
	for _, consistent := range []bool{false, true} {
		got, err := d.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: new("orders"), Key: key("a"), ConsistentRead: new(consistent),
		})
		if err != nil || len(got.Item) != 2 {
			t.Fatalf("%+v %v", got, err)
		}
	}

	if _, err := d.Query(ctx, &dynamodb.QueryInput{
		TableName:              new("orders"),
		KeyConditionExpression: new("PK = :k"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":k": &ddbtypes.AttributeValueMemberS{Value: "a"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// A failed condition is a user error and consumes nothing.
	_, err := d.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: new("orders"), Item: item, ConditionExpression: new("attribute_not_exists(PK)"),
	})
	code(t, err, "ConditionalCheckFailedException")
	if _, err = d.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: new("orders"), Key: key("a")}); err != nil {
		t.Fatal(err)
	}

	table := dimensions("TableName", "orders")
	expectBuiltin(t, c, "AWS/DynamoDB", "ConsumedWriteCapacityUnits", "Count", table, 2)
	expectBuiltin(t, c, "AWS/DynamoDB", "ConsumedReadCapacityUnits", "Count", table, 2)
	expectBuiltin(t, c, "AWS/DynamoDB", "UserErrors", "Count", nil, 1)
	put := dimensions("TableName", "orders", "Operation", "PutItem")
	latency := builtinMetric(
		t,
		c,
		"AWS/DynamoDB",
		"SuccessfulRequestLatency",
		put,
		func(b builtin) bool { return b.count == 1 },
	)
	if latency.unit != "Milliseconds" {
		t.Fatalf("%+v", latency)
	}

	expectBuiltin(t, c, "AWS/DynamoDB", "ThrottledRequests", "Count", put, 0)
}

func TestBuiltinAPIGatewayMetrics(t *testing.T) {
	s := server(t)
	c := cloudwatchClient(s)
	functions := lambdaClient(s, "test")
	ctx := context.Background()
	respond := createFunction(t, functions, "respond", "app.respond")
	fail := createFunction(t, functions, "fail", "app.fail")

	// A REST API with one working and one failing proxy resource.
	rest := restClient(s)
	api, err := rest.CreateRestApi(ctx, &restapi.CreateRestApiInput{Name: new("shop")})
	if err != nil {
		t.Fatal(err)
	}

	for path, fn := range map[string]*sdklambda.CreateFunctionOutput{"ok": respond, "broken": fail} {
		resource, createErr := rest.CreateResource(ctx, &restapi.CreateResourceInput{
			RestApiId: api.Id, ParentId: api.RootResourceId, PathPart: new(path),
		})
		if createErr != nil {
			t.Fatal(createErr)
		}

		restMethod(t, rest, aws.ToString(api.Id), aws.ToString(resource.Id), "GET", &restapi.PutIntegrationInput{
			Type: resttypes.IntegrationTypeAwsProxy, IntegrationHttpMethod: new("POST"), Uri: lambdaURI(fn.FunctionArn),
		})
	}

	if _, err = rest.CreateDeployment(
		ctx,
		&restapi.CreateDeploymentInput{RestApiId: api.Id, StageName: new("prod")},
	); err != nil {
		t.Fatal(err)
	}

	id := aws.ToString(api.Id)
	statuses := map[string]int{"/prod/ok": 201, "/prod/ok?again": 201, "/prod/broken": 502, "/prod/missing": 403}
	for path, want := range statuses {
		if status, _, body := call(t, s, "GET", id, path, nil, ""); status != want {
			t.Fatalf("%s: %d %s", path, status, body)
		}
	}

	stage := dimensions("ApiName", "shop", "Stage", "prod")
	expectBuiltin(t, c, "AWS/ApiGateway", "Count", "Count", stage, 4)
	expectBuiltin(t, c, "AWS/ApiGateway", "4XXError", "Count", stage, 1)
	expectBuiltin(t, c, "AWS/ApiGateway", "5XXError", "Count", stage, 1)
	expectBuiltin(t, c, "AWS/ApiGateway", "Count", "Count", dimensions("ApiName", "shop"), 4)
	latency := builtinMetric(t, c, "AWS/ApiGateway", "Latency", stage, func(b builtin) bool { return b.count == 4 })
	integration := builtinMetric(
		t,
		c,
		"AWS/ApiGateway",
		"IntegrationLatency",
		stage,
		func(b builtin) bool { return b.count == 3 },
	)
	if latency.unit != "Milliseconds" || integration.unit != "Milliseconds" || integration.sum > latency.sum {
		t.Fatalf("%+v %+v", latency, integration)
	}

	// An HTTP API reports under ApiId, with the lower-case error metrics.
	http2 := apigwClient(s)
	quick, err := http2.CreateApi(ctx, &apigw.CreateApiInput{
		Name: new("quick"), ProtocolType: apigwtypes.ProtocolTypeHttp, Target: respond.FunctionArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	failing, err := http2.CreateApi(ctx, &apigw.CreateApiInput{
		Name: new("failing"), ProtocolType: apigwtypes.ProtocolTypeHttp, Target: fail.FunctionArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	call(t, s, "GET", aws.ToString(quick.ApiId), "/anything", nil, "")
	call(t, s, "GET", aws.ToString(quick.ApiId), "/anything", nil, "")
	if status, _, body := call(
		t,
		s,
		"GET",
		aws.ToString(failing.ApiId),
		"/anything",
		nil,
		"",
	); status < http.StatusInternalServerError {
		t.Fatalf("%d %s", status, body)
	}

	quickStage := dimensions("ApiId", aws.ToString(quick.ApiId), "Stage", "$default")
	expectBuiltin(t, c, "AWS/ApiGateway", "Count", "Count", quickStage, 2)
	expectBuiltin(t, c, "AWS/ApiGateway", "5xx", "Count", quickStage, 0)
	expectBuiltin(t, c, "AWS/ApiGateway", "5xx", "Count", dimensions("ApiId", aws.ToString(failing.ApiId)), 1)
	quickLatency := builtinMetric(t, c, "AWS/ApiGateway", "IntegrationLatency", quickStage,
		func(b builtin) bool { return b.count == 2 })
	if quickLatency.unit != "Milliseconds" {
		t.Fatalf("%+v", quickLatency)
	}
}

func TestBuiltinSNSAndKinesisMetrics(t *testing.T) {
	s := server(t)
	c := cloudwatchClient(s)
	topics := snsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	_, queue := queueARN(t, q, "inbox")
	topic, err := topics.CreateTopic(ctx, &sns.CreateTopicInput{Name: new("alerts")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: new(queue),
	}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if _, err = topics.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("fire")}); err != nil {
			t.Fatal(err)
		}
	}

	// Deleting the queue makes the next delivery fail.
	lost, lostARN := queueARN(t, q, "lost")
	if _, err = topics.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: new("sqs"), Endpoint: new(lostARN),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = q.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: &lost}); err != nil {
		t.Fatal(err)
	}

	if _, err = topics.Publish(ctx, &sns.PublishInput{TopicArn: topic.TopicArn, Message: new("fire")}); err != nil {
		t.Fatal(err)
	}

	alerts := dimensions("TopicName", "alerts")
	expectBuiltin(t, c, "AWS/SNS", "NumberOfMessagesPublished", "Count", alerts, 3)
	expectBuiltin(t, c, "AWS/SNS", "NumberOfNotificationsDelivered", "Count", alerts, 3)
	expectBuiltin(t, c, "AWS/SNS", "NumberOfNotificationsFailed", "Count", alerts, 1)

	k := kinesisClient(s)
	if _, err = k.CreateStream(
		ctx,
		&kinesis.CreateStreamInput{StreamName: new("clicks"), ShardCount: new(int32(1))},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = k.PutRecord(ctx, &kinesis.PutRecordInput{
		StreamName: new("clicks"), PartitionKey: new("k"), Data: []byte("one"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = k.PutRecords(
		ctx,
		&kinesis.PutRecordsInput{StreamName: new("clicks"), Records: []kintypes.PutRecordsRequestEntry{
			{PartitionKey: new("k"), Data: []byte("two")}, {PartitionKey: new("k"), Data: []byte("three")},
		}},
	); err != nil {
		t.Fatal(err)
	}

	iterator, err := k.GetShardIterator(ctx, &kinesis.GetShardIteratorInput{
		StreamName: new(
			"clicks",
		),
		ShardId:           new("shardId-000000000000"),
		ShardIteratorType: kintypes.ShardIteratorTypeTrimHorizon,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := k.GetRecords(ctx, &kinesis.GetRecordsInput{ShardIterator: iterator.ShardIterator})
	if err != nil || len(got.Records) != 3 {
		t.Fatalf("%+v %v", got, err)
	}

	stream := dimensions("StreamName", "clicks")
	expectBuiltin(t, c, "AWS/Kinesis", "IncomingRecords", "Count", stream, 3)
	expectBuiltin(t, c, "AWS/Kinesis", "IncomingBytes", "Bytes", stream, float64(len("one")+len("two")+len("three")+3))
	expectBuiltin(t, c, "AWS/Kinesis", "GetRecords.Records", "Count", stream, 3)
}
