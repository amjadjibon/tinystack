// Package eventsource lists what a service must be permitted to read an event source, for Lambda event source
// mappings and Pipes, which read SQS queues, DynamoDB streams and Kinesis streams under a role.
package eventsource

import (
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
)

const (
	serviceSQS     = "sqs"
	serviceKinesis = "kinesis"
	serviceStreams = "dynamodbstreams"
	paramStreamARN = "StreamARN"
	// startOfStream is the iterator type the reads are made with.
	startOfStream = "TRIM_HORIZON"
)

// Reads are the calls that read the source named by arn, as the documentation of AWS Lambda and EventBridge Pipes
// lists the actions the role needs:
// https://docs.aws.amazon.com/lambda/latest/dg/with-sqs.html
// https://docs.aws.amazon.com/lambda/latest/dg/with-ddb.html
// https://docs.aws.amazon.com/lambda/latest/dg/with-kinesis.html
// Only the service and the call are set; the role and session are the reader's.
func Reads(arn string) []catalog.DeliveryRole {
	switch strings.Split(arn, ":")[2] {
	case serviceSQS:
		url, _ := sqs.QueueURL(arn)
		queue := map[string]any{"QueueUrl": url}

		return []catalog.DeliveryRole{
			{Service: serviceSQS, Operation: "ReceiveMessage", Params: queue},
			{Service: serviceSQS, Operation: "DeleteMessage", Params: map[string]any{
				"QueueUrl": url, "ReceiptHandle": "-",
			}},
			{Service: serviceSQS, Operation: "GetQueueAttributes", Params: queue},
		}
	case serviceKinesis:
		stream := map[string]any{paramStreamARN: arn}

		return []catalog.DeliveryRole{
			{Service: serviceKinesis, Operation: "DescribeStream", Params: stream},
			{Service: serviceKinesis, Operation: "ListShards", Params: stream},
			{Service: serviceKinesis, Operation: "GetShardIterator", Params: map[string]any{
				paramStreamARN: arn, "ShardId": "-", "ShardIteratorType": startOfStream,
			}},
			{Service: serviceKinesis, Operation: "GetRecords", Params: map[string]any{
				paramStreamARN: arn, "ShardIterator": "-",
			}},
		}
	}

	return []catalog.DeliveryRole{
		{Service: serviceStreams, Operation: "DescribeStream", Params: map[string]any{"StreamArn": arn}},
		{Service: serviceStreams, Operation: "GetShardIterator", Params: map[string]any{
			"StreamArn": arn, "ShardId": "-", "ShardIteratorType": startOfStream,
		}},
		{Service: serviceStreams, Operation: "GetRecords", Params: map[string]any{"ShardIterator": "-"}},
	}
}
