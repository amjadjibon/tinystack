package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	eventtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// tooMany is one tag over the per-resource limit.
func tooMany() map[string]string {
	tags := map[string]string{}
	for i := range 51 {
		tags[fmt.Sprintf("k%d", i)] = "v"
	}

	return tags
}

func TestSQSTags(t *testing.T) {
	s := server(t)
	c := sqsClient(s, "test")
	ctx := context.Background()
	created, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("tagged"), Tags: map[string]string{"team": "core"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: created.QueueUrl, Tags: map[string]string{"env": "dev"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagQueue(ctx, &sqs.UntagQueueInput{QueueUrl: created.QueueUrl, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: created.QueueUrl})
	if err != nil || len(listed.Tags) != 1 || listed.Tags["env"] != "dev" {
		t.Fatal(listed, err)
	}

	_, err = c.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: created.QueueUrl, Tags: tooMany()})
	code(t, err, "InvalidParameterValue")

	// AWS Query clients send tags as flattened Tag.N.Key and Tag.N.Value pairs.
	form := url.Values{
		"Action": {"ListQueueTags"}, "Version": {"2012-11-05"}, "QueueUrl": {*created.QueueUrl},
	}

	resp, err := http.Post(s.URL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<Tag><Key>env</Key><Value>dev</Value></Tag>") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestSNSTopicAttributesAndTags(t *testing.T) {
	s := server(t)
	c := snsClient(s, "test")
	ctx := context.Background()
	created, err := c.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String("tagged"), Tags: []snstypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := created.TopicArn
	_, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: arn, AttributeName: aws.String("SQSSuccessFeedbackSampleRate"), AttributeValue: aws.String("50"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn: arn, AttributeName: aws.String("SQSSuccessFeedbackRoleArn"), AttributeValue: aws.String("x"),
	})
	code(t, err, "InvalidParameter")

	attrs, err := c.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: arn})
	if err != nil {
		t.Fatal(err)
	}

	var policy struct{ Statement []struct{ Resource string } }
	if json.Unmarshal([]byte(attrs.Attributes["Policy"]), &policy) != nil || len(policy.Statement) != 1 ||
		policy.Statement[0].Resource != *arn || attrs.Attributes["Owner"] != "000000000000" ||
		attrs.Attributes["SQSSuccessFeedbackSampleRate"] != "50" {
		t.Fatal(attrs.Attributes)
	}

	_, err = c.TagResource(ctx, &sns.TagResourceInput{
		ResourceArn: arn, Tags: []snstypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagResource(ctx, &sns.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sns.ListTagsForResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "env" {
		t.Fatal(tags, err)
	}

	missing := aws.String("arn:aws:sns:us-east-1:000000000000:missing")
	_, err = c.ListTagsForResource(ctx, &sns.ListTagsForResourceInput{ResourceArn: missing})
	code(t, err, "ResourceNotFound")

	queueURL, queue := queueARN(t, sqsClient(s, "test"), "inbox")
	_ = queueURL
	sub, err := c.Subscribe(ctx, &sns.SubscribeInput{TopicArn: arn, Protocol: aws.String("sqs"), Endpoint: &queue})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.SetSubscriptionAttributes(ctx, &sns.SetSubscriptionAttributesInput{
		SubscriptionArn: sub.SubscriptionArn,
		AttributeName:   aws.String("RawMessageDelivery"),
		AttributeValue:  aws.String("true"),
	})
	if err != nil {
		t.Fatal(err)
	}

	subAttrs, err := c.GetSubscriptionAttributes(
		ctx,
		&sns.GetSubscriptionAttributesInput{SubscriptionArn: sub.SubscriptionArn},
	)
	if err != nil || subAttrs.Attributes["RawMessageDelivery"] != "true" ||
		subAttrs.Attributes["Endpoint"] != queue || subAttrs.Attributes["PendingConfirmation"] != "false" {
		t.Fatal(subAttrs, err)
	}
}

func TestDynamoDBTableSettingsAndTags(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	ctx := context.Background()
	created, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String("tagged"),
		BillingMode:          dynamotypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamotypes.AttributeDefinition{{AttributeName: aws.String("PK"), AttributeType: "S"}},
		KeySchema:            []dynamotypes.KeySchemaElement{{AttributeName: aws.String("PK"), KeyType: "HASH"}},
		Tags:                 []dynamotypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	backups, err := c.DescribeContinuousBackups(ctx, &dynamodb.DescribeContinuousBackupsInput{
		TableName: aws.String("tagged"),
	})
	if err != nil || backups.ContinuousBackupsDescription.PointInTimeRecoveryDescription.PointInTimeRecoveryStatus !=
		dynamotypes.PointInTimeRecoveryStatusDisabled {
		t.Fatal(backups, err)
	}

	ttl, err := c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String("tagged")})
	if err != nil || ttl.TimeToLiveDescription.TimeToLiveStatus != dynamotypes.TimeToLiveStatusDisabled {
		t.Fatal(ttl, err)
	}

	arn := created.TableDescription.TableArn
	_, err = c.TagResource(ctx, &dynamodb.TagResourceInput{
		ResourceArn: arn, Tags: []dynamotypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagResource(ctx, &dynamodb.UntagResourceInput{ResourceArn: arn, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{ResourceArn: arn})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "env" {
		t.Fatal(tags, err)
	}

	// Another region's table ARN names no table the caller can see.
	other := aws.String(strings.Replace(*arn, "us-east-1", "eu-west-1", 1))
	_, err = c.ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{ResourceArn: other})
	code(t, err, "ResourceNotFoundException")
}

func TestLambdaTagsAndVersions(t *testing.T) {
	s := server(t)
	c := lambdaClient(s, "test")
	ctx := context.Background()
	created := createFunction(t, c, "tagged", "app.echo")
	arn := created.FunctionArn
	_, err := c.TagResource(
		ctx,
		&sdklambda.TagResourceInput{Resource: arn, Tags: map[string]string{"a": "1", "b": "2"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagResource(ctx, &sdklambda.UntagResourceInput{Resource: arn, TagKeys: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTags(ctx, &sdklambda.ListTagsInput{Resource: arn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["b"] != "2" {
		t.Fatal(tags, err)
	}

	got, err := c.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: aws.String("tagged")})
	if err != nil || got.Tags["b"] != "2" {
		t.Fatal(got, err)
	}

	_, err = c.TagResource(ctx, &sdklambda.TagResourceInput{Resource: aws.String(*arn + ":$LATEST"), Tags: tooMany()})
	code(t, err, "InvalidParameterValueException")

	versions, err := c.ListVersionsByFunction(ctx, &sdklambda.ListVersionsByFunctionInput{
		FunctionName: aws.String("tagged"), MaxItems: aws.Int32(10000),
	})
	if err != nil || len(versions.Versions) != 1 || *versions.Versions[0].Version != "$LATEST" {
		t.Fatal(versions, err)
	}

	signing, err := c.GetFunctionCodeSigningConfig(ctx, &sdklambda.GetFunctionCodeSigningConfigInput{
		FunctionName: aws.String("tagged"),
	})
	if err != nil || *signing.CodeSigningConfigArn != "" || *signing.FunctionName != "tagged" {
		t.Fatal(signing, err)
	}
}

func TestEventBridgeRuleTags(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	rule, err := c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name:         aws.String("tagged"),
		EventPattern: aws.String(`{"source":["shop"]}`),
		Tags:         []eventtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := rule.RuleArn
	_, err = c.TagResource(ctx, &eventbridge.TagResourceInput{
		ResourceARN: arn, Tags: []eventtypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UntagResource(ctx, &eventbridge.UntagResourceInput{ResourceARN: arn, TagKeys: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{ResourceARN: arn})
	if err != nil || len(tags.Tags) != 1 || *tags.Tags[0].Key != "env" {
		t.Fatal(tags, err)
	}

	missing := aws.String("arn:aws:events:us-east-1:000000000000:rule/missing")
	_, err = c.ListTagsForResource(ctx, &eventbridge.ListTagsForResourceInput{ResourceARN: missing})
	code(t, err, "ResourceNotFoundException")
}

func TestS3BucketTaggingAndACL(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	_, err := c.CreateBucket(
		ctx,
		&s3.CreateBucketInput{Bucket: aws.String("tagged"), ACL: s3types.BucketCannedACLPrivate},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("public"), ACL: s3types.BucketCannedACLPublicRead,
	})
	if got, status := s3Code(err); got != "NotImplemented" || status != http.StatusNotImplemented {
		t.Fatal(err)
	}

	_, err = c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("tagged")})
	if got, status := s3Code(err); got != "NoSuchTagSet" || status != http.StatusNotFound {
		t.Fatal(err)
	}

	tag := func(k, v string) s3types.Tag { return s3types.Tag{Key: aws.String(k), Value: aws.String(v)} }
	_, err = c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: aws.String("tagged"), Tagging: &s3types.Tagging{TagSet: []s3types.Tag{tag("b", "2"), tag("a", "1")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("tagged")})
	if err != nil || len(got.TagSet) != 2 || *got.TagSet[0].Key != "a" || *got.TagSet[1].Value != "2" {
		t.Fatal(got, err)
	}

	_, err = c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: aws.String("tagged"), Tagging: &s3types.Tagging{TagSet: []s3types.Tag{tag("a", "1"), tag("a", "2")}},
	})
	if code, status := s3Code(err); code != "InvalidTag" || status != http.StatusBadRequest {
		t.Fatal(err)
	}

	if _, err = c.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: aws.String("tagged")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("tagged")})
	if got, _ := s3Code(err); got != "NoSuchTagSet" {
		t.Fatal(err)
	}
}
