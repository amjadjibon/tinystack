package integration_test

import (
	"context"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3LoggingCloudFormation(t *testing.T) {
	s := server(t)
	c, files := cfnClient(s), s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	const body = `{"Resources":{"Bucket":{"Type":"AWS::S3::Bucket","Properties":{
 "BucketName":"cfn-logging","LoggingConfiguration":{"LogFilePrefix":"logs/",
 "TargetObjectKeyFormat":{"SimplePrefix":{}}}}}}}`
	deployWithChangeSet(ctx, t, c, "logging-stack", body)
	got, err := files.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: new("cfn-logging")})
	if err != nil || got.LoggingEnabled == nil || *got.LoggingEnabled.TargetBucket != "cfn-logging" {
		t.Fatalf("stack logging: %+v %v", got, err)
	}

	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new("logging-stack"),
		TemplateBody: new(
			`{"Resources":{"Bucket":{"Type":"AWS::S3::Bucket","Properties":{"BucketName":"cfn-logging"}}}}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "logging-stack", cfntypes.StackStatusUpdateComplete)
	got, err = files.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: new("cfn-logging")})
	if err != nil || got.LoggingEnabled != nil {
		t.Fatalf("removed logging: %+v %v", got, err)
	}
}
