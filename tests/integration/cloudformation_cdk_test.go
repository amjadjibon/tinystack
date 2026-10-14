package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// toolkitTemplate is the part of the CDK bootstrap stack that stores the
// bootstrap version and the asset bucket.
const toolkitTemplate = `{
  "Resources": {
    "Version": {"Type": "AWS::SSM::Parameter", "Properties": {
      "Name": "/cdk-bootstrap/hnb659fds/version", "Type": "String", "Value": "%s"}},
    "Staging": {"Type": "AWS::S3::Bucket", "Properties": {
      "BucketName": "cdk-assets-bucket",
      "AccessControl": "Private",
      "BucketEncryption": {"ServerSideEncryptionConfiguration": [
        {"ServerSideEncryptionByDefault": {"SSEAlgorithm": "aws:kms"}}]},
      "PublicAccessBlockConfiguration": {
        "BlockPublicAcls": true, "BlockPublicPolicy": true, "IgnorePublicAcls": true, "RestrictPublicBuckets": true},
      "LifecycleConfiguration": {"Rules": [
        {"Id": "old", "Status": "Enabled", "NoncurrentVersionExpiration": {"NoncurrentDays": 30}},
        {"Id": "parts", "Status": "Enabled", "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}}]}}},
    "StagingPolicy": {"Type": "AWS::S3::BucketPolicy", "Properties": {
      "Bucket": {"Ref": "Staging"},
      "PolicyDocument": {"Version": "2012-10-17", "Statement": [{
        "Effect": "Deny", "Principal": "*", "Action": "s3:*",
        "Resource": [{"Fn::Sub": "${Staging.Arn}"}, {"Fn::Sub": "${Staging.Arn}/*"}],
        "Condition": {"Bool": {"aws:SecureTransport": "false"}}}]}}},
    "CDKMetadata": {"Type": "AWS::CDK::Metadata", "Properties": {"Analytics": "v2:deflate64:abc"}}
  }
}`

// cdkAppTemplate is what the CDK CLI synthesizes: a bootstrap version read from
// Systems Manager and a rule that demands version 6 or later.
const cdkAppTemplate = `{
  "Parameters": {"BootstrapVersion": {
    "Type": "AWS::SSM::Parameter::Value<String>", "Default": "/cdk-bootstrap/hnb659fds/version"}},
  "Resources": {"Jobs": {"Type": "AWS::SQS::Queue", "Properties": {"QueueName": "cdk-jobs"}}},
  "Rules": {"CheckBootstrapVersion": {"Assertions": [{
    "Assert": {"Fn::Not": [{"Fn::Contains": [["1", "2", "3", "4", "5"], {"Ref": "BootstrapVersion"}]}]},
    "AssertDescription": "CDK bootstrap stack version 6 required."}]}}
}`

// deployWithChangeSet deploys a template the way the CDK CLI does.
func deployWithChangeSet(ctx context.Context, t *testing.T, c *cfn.Client, name, body string) {
	t.Helper()
	created, err := c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
		StackName: aws.String(name), ChangeSetName: aws.String("cdk"), ChangeSetType: cfntypes.ChangeSetTypeCreate,
		TemplateBody: aws.String(body), Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		ImportExistingResources: aws.Bool(true), IncludeNestedStacks: aws.Bool(true),
		DeploymentConfig: &cfntypes.DeploymentConfig{Mode: cfntypes.DeploymentConfigModeStandard},
	})
	if err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeChangeSet(ctx, &cfn.DescribeChangeSetInput{
		ChangeSetName: created.Id, IncludePropertyValues: aws.Bool(true),
	})
	if err != nil || described.Status != cfntypes.ChangeSetStatusCreateComplete {
		t.Fatalf("change set = %+v, %v", described, err)
	}

	if _, err = c.ExecuteChangeSet(ctx, &cfn.ExecuteChangeSetInput{ChangeSetName: created.Id}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, name, cfntypes.StackStatusCreateComplete)
}

func TestCloudFormationCDKBootstrapAndDeploy(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)

	deployWithChangeSet(ctx, t, c, "CDKToolkit", strings.Replace(toolkitTemplate, "%s", "6", 1))

	bucket := aws.String("cdk-assets-bucket")
	files := s3Client(s, "test", "us-east-1")
	encryption, err := files.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}

	rule := encryption.ServerSideEncryptionConfiguration.Rules[0]
	if rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm != s3types.ServerSideEncryptionAwsKms {
		t.Fatalf("encryption = %+v", rule)
	}

	if _, err = files.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	lifecycle, err := files.GetBucketLifecycleConfiguration(
		ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: bucket},
	)
	if err != nil || len(lifecycle.Rules) != 2 || aws.ToString(lifecycle.Rules[0].ID) != "old" {
		t.Fatalf("lifecycle = %+v, %v", lifecycle, err)
	}

	policy, err := files.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: bucket})
	if err != nil || !strings.Contains(aws.ToString(policy.Policy), "aws:SecureTransport") {
		t.Fatalf("policy = %+v, %v", policy, err)
	}

	// The CDK CLI writes to the encrypted bucket asking for KMS.
	if _, err = files.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("asset.zip"), Body: strings.NewReader("zip"),
		ServerSideEncryption: s3types.ServerSideEncryptionAwsKms,
	}); err != nil {
		t.Fatal(err)
	}

	deployWithChangeSet(ctx, t, c, "App", cdkAppTemplate)
	queues := sqsClient(s, "test")
	if _, err = queues.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("cdk-jobs")}); err != nil {
		t.Fatal(err)
	}

	// Express mode is not supported; a stack is deleted with the standard one.
	_, err = c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
		StackName: aws.String("App"), ChangeSetName: aws.String("fast"), TemplateBody: aws.String(cdkAppTemplate),
		DeploymentConfig: &cfntypes.DeploymentConfig{Mode: cfntypes.DeploymentConfigModeExpress},
	})
	code(t, err, "ValidationError")

	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName:        aws.String("App"),
		DeploymentConfig: &cfntypes.DeploymentConfig{Mode: cfntypes.DeploymentConfigModeStandard},
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "App to be deleted", func() bool {
		_, getErr := queues.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("cdk-jobs")})
		return getErr != nil
	})
}

func TestCloudFormationRulesCheckBootstrapVersion(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)

	// With an old bootstrap version the rule refuses the stack.
	if _, err := ssmClient(s, "test", "us-east-1").PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/cdk-bootstrap/hnb659fds/version"), Type: "String", Value: aws.String("4"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: aws.String("Old"), TemplateBody: aws.String(cdkAppTemplate)},
	)
	code(t, err, "ValidationError")
	if err == nil || !strings.Contains(err.Error(), "CDK bootstrap stack version 6 required.") {
		t.Fatalf("err = %v", err)
	}

	if _, err = ssmClient(s, "test", "us-east-1").PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/cdk-bootstrap/hnb659fds/version"), Type: "String", Value: aws.String("30"),
		Overwrite: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("New"), TemplateBody: aws.String(cdkAppTemplate),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "New", cfntypes.StackStatusCreateComplete)
}
