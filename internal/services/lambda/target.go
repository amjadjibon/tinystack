package lambda

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
)

type targetCheck struct {
	params    map[string]any
	service   string
	operation string
}

func (s *Service) checkAsyncTarget(ctx context.Context, scope request.Scope, name, role, arn string) error {
	if arn == "" {
		return nil
	}

	service, _ := destinationService(arn)
	parts := strings.SplitN(arn, ":", 6)
	target := request.Scope{AccountID: parts[4], Region: parts[3]}
	var read targetCheck
	var writes []targetCheck
	switch service {
	case serviceSQS:
		url, ok := sqs.QueueURL(arn)
		if !ok || strings.HasSuffix(arn, ".fifo") {
			return invalid("Invalid destination ARN %s. A standard SQS queue is required.", arn)
		}

		read = targetCheck{service: serviceSQS, operation: "GetQueueAttributes", params: map[string]any{
			paramQueueURL: url, "AttributeNames": []any{"QueueArn"},
		}}
		writes = []targetCheck{{service: serviceSQS, operation: "SendMessage", params: map[string]any{
			paramQueueURL: url, "MessageBody": "{}",
		}}}
	case serviceSNS:
		if strings.HasSuffix(arn, ".fifo") {
			return invalid("Invalid destination ARN %s. A standard SNS topic is required.", arn)
		}

		read = targetCheck{
			service:   serviceSNS,
			operation: "GetTopicAttributes",
			params:    map[string]any{paramTopicARN: arn},
		}
		writes = []targetCheck{{service: serviceSNS, operation: "Publish", params: map[string]any{
			paramTopicARN: arn, "Message": "{}",
		}}}
	case serviceLambda:
		read = targetCheck{
			service:   serviceLambda,
			operation: "GetFunction",
			params:    map[string]any{paramFunctionName: arn},
		}
		writes = []targetCheck{{service: serviceLambda, operation: "Invoke", params: map[string]any{
			paramFunctionName: arn, "InvocationType": invocationEvent,
		}}}
	case "events":
		read = targetCheck{
			service:   serviceEventBridge,
			operation: "DescribeEventBus",
			params:    map[string]any{"Name": arn},
		}
		writes = []targetCheck{{service: serviceEventBridge, operation: "PutEvents", params: map[string]any{
			"Entries": []any{map[string]any{
				"EventBusName": arn, "Source": serviceLambda,
				"DetailType": "Lambda Function Invocation Result", "Detail": "{}",
			}},
		}}}
	default: // S3 is allowed only on failure and has no account or region in its ARN.
		target = scope
		bucket := parts[5]
		read = targetCheck{service: "s3", operation: "HeadBucket", params: map[string]any{paramBucket: bucket}}
		writes = []targetCheck{
			{service: "s3", operation: "ListObjectsV2", params: map[string]any{paramBucket: bucket}},
			{service: "s3", operation: "PutObject", params: map[string]any{
				paramBucket: bucket, "Key": "aws/lambda/async/" + name + "/validation", "Body": "",
			}},
		}
	}

	if s.caller == nil {
		return invalid("Destinations are not available")
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: target})
	if _, err := s.caller.Call(ctx, read.service, read.operation, read.params); err != nil {
		return invalid("Cannot access the Lambda delivery target %s: %v", arn, err)
	}

	for _, write := range writes {
		call := executionRoleDelivery(scope, name, role, &catalog.DeliveryRole{
			Service: write.service, Operation: write.operation, Params: write.params,
		})
		if err := s.authorizer.Authorize(ctx, call); err != nil {
			return invalid("The function's execution role does not have permissions to call %s on %s",
				write.operation, arn)
		}
	}

	return nil
}
