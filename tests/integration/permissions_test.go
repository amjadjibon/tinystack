package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// statements decodes a policy's statement IDs and the principal of each.
func statements(t *testing.T, policy string) map[string]any {
	t.Helper()
	var doc struct {
		Statement []struct {
			Principal map[string]any
			Sid       string
		}
	}
	if policy == "" {
		return map[string]any{}
	}

	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatalf("%s: %v", policy, err)
	}

	out := map[string]any{}
	for _, st := range doc.Statement {
		out[st.Sid] = st.Principal["AWS"]
	}

	return out
}

func TestSQSAndSNSPermissions(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	q := sqsClient(s, "test")
	queueURL, _ := queueARN(t, q, "shared")
	queuePolicy := func() map[string]any {
		out, err := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: &queueURL, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNamePolicy},
		})
		if err != nil {
			t.Fatal(err)
		}

		return statements(t, out.Attributes["Policy"])
	}

	if _, err := q.AddPermission(ctx, &sqs.AddPermissionInput{
		QueueUrl: &queueURL, Label: aws.String("partner"), AWSAccountIds: []string{"111122223333"},
		Actions: []string{"SendMessage", "ReceiveMessage"},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := q.AddPermission(ctx, &sqs.AddPermissionInput{
		QueueUrl: &queueURL, Label: aws.String("partner"), AWSAccountIds: []string{"111122223333"},
		Actions: []string{"SendMessage"},
	})
	code(t, err, "InvalidParameterValue")
	_, err = q.AddPermission(ctx, &sqs.AddPermissionInput{
		QueueUrl: &queueURL, Label: aws.String("owner"), AWSAccountIds: []string{"111122223333"},
		Actions: []string{"DeleteQueue"},
	})
	code(t, err, "InvalidParameterValue")

	if got := queuePolicy(); got["partner"] != "arn:aws:iam::111122223333:root" {
		t.Fatalf("queue policy %v", got)
	}

	if _, err = q.RemovePermission(ctx, &sqs.RemovePermissionInput{
		QueueUrl: &queueURL, Label: aws.String("partner"),
	}); err != nil {
		t.Fatal(err)
	}

	if got := queuePolicy(); len(got) != 0 {
		t.Fatalf("queue policy %v", got)
	}

	_, err = q.RemovePermission(ctx, &sqs.RemovePermissionInput{QueueUrl: &queueURL, Label: aws.String("partner")})
	code(t, err, "InvalidParameterValue")

	// A topic's statements join its default policy.
	n := snsClient(s, "test")
	topic, err := n.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("shared")})
	if err != nil {
		t.Fatal(err)
	}

	topicPolicy := func() map[string]any {
		out, attrErr := n.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: topic.TopicArn})
		if attrErr != nil {
			t.Fatal(attrErr)
		}

		return statements(t, out.Attributes["Policy"])
	}

	if _, err = n.AddPermission(ctx, &sns.AddPermissionInput{
		TopicArn: topic.TopicArn, Label: aws.String("partners"),
		AWSAccountId: []string{"111122223333", "444455556666"}, ActionName: []string{"Publish"},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = n.AddPermission(ctx, &sns.AddPermissionInput{
		TopicArn: topic.TopicArn, Label: aws.String("partners"), AWSAccountId: []string{"111122223333"},
		ActionName: []string{"Publish"},
	})
	code(t, err, "InvalidParameter")

	got := topicPolicy()
	if principals, _ := got["partners"].([]any); len(got) != 2 || len(principals) != 2 {
		t.Fatalf("topic policy %v", got)
	}

	if _, err = n.RemovePermission(ctx, &sns.RemovePermissionInput{
		TopicArn: topic.TopicArn, Label: aws.String("partners"),
	}); err != nil {
		t.Fatal(err)
	}

	if got = topicPolicy(); len(got) != 1 {
		t.Fatalf("topic policy %v", got)
	}

	_, err = n.RemovePermission(ctx, &sns.RemovePermissionInput{TopicArn: topic.TopicArn, Label: aws.String("x")})
	code(t, err, "InvalidParameter")
}
