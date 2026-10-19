package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// servicePolicy lets the service principal perform action on resource when source is the resource delivering.
func servicePolicy(action, resource, principal, source string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":%q},`+
		`"Action":%q,"Resource":%q,"Condition":{"ArnEquals":{"aws:SourceArn":%q}}}]}`,
		principal, action, resource, source)
}

// allowDelivery lets the service principal deliver to the queue, topic, or function target when source delivers.
// key is the access key of the target's account.
func allowDelivery(t *testing.T, s *httptest.Server, key, target, principal, source string) {
	t.Helper()
	ctx := context.Background()
	parts := strings.Split(target, ":")
	switch parts[2] {
	case "sqs":
		queues := sqsClient(s, key)
		queueURL, err := queues.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: &parts[5]})
		mustAuthorize(t, err)
		_, err = queues.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl:   queueURL.QueueUrl,
			Attributes: map[string]string{"Policy": servicePolicy("sqs:SendMessage", target, principal, source)},
		})
		mustAuthorize(t, err)
	case "sns":
		_, err := snsClient(s, key).SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
			TopicArn: &target, AttributeName: new("Policy"),
			AttributeValue: new(servicePolicy("sns:Publish", target, principal, source)),
		})
		mustAuthorize(t, err)
	case "lambda":
		digest := sha256.Sum256([]byte(principal + source))
		_, err := lambdaClient(s, key).AddPermission(ctx, &lambda.AddPermissionInput{
			FunctionName: &target, StatementId: new("service-" + hex.EncodeToString(digest[:6])),
			Principal: &principal, SourceArn: &source, Action: new("lambda:InvokeFunction"),
		})
		mustAuthorize(t, err)
	default:
		t.Fatalf("no delivery to %s", target)
	}
}

// allowService lets the service principal invoke the function named name when source invokes it.
func allowService(t *testing.T, s *httptest.Server, name, principal, source string) {
	t.Helper()
	allowDelivery(t, s, "test", "arn:aws:lambda:us-east-1:000000000000:function:"+name, principal, source)
}

// executeSource matches the aws:SourceArn API Gateway sends for any stage, method, and route of the API.
func executeSource(apiID string) string {
	return "arn:aws:execute-api:us-east-1:000000000000:" + apiID + "/*"
}
