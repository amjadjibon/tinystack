package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/configservice"
)

const configTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Resources:
  Bucket:
    Type: AWS::S3::Bucket
  Recorder:
    Type: AWS::Config::ConfigurationRecorder
    Properties:
      Name: default
      RoleARN: arn:aws:iam::000000000000:role/config-role
      RecordingGroup:
        AllSupported: false
        ResourceTypes: ["AWS::S3::Bucket"]
  Channel:
    Type: AWS::Config::DeliveryChannel
    DependsOn: Recorder
    Properties:
      S3BucketName: !Ref Bucket
      S3KeyPrefix: first
      ConfigSnapshotDeliveryProperties:
        DeliveryFrequency: Six_Hours
  Rule:
    Type: AWS::Config::ConfigRule
    Properties:
      Description: first
      Source: {Owner: AWS, SourceIdentifier: REQUIRED_TAGS}
      Scope: {ComplianceResourceTypes: ["AWS::S3::Bucket"]}
      InputParameters: {tag1Key: Owner}
Outputs:
  Recorder: {Value: !Ref Recorder}
  RecorderARN: {Value: !GetAtt Recorder.ResourceARN}
  Channel: {Value: !Ref Channel}
  Rule: {Value: !Ref Rule}
  RuleARN: {Value: !GetAtt Rule.Arn}
  RuleID: {Value: !GetAtt Rule.ConfigRuleId}
`

func TestCloudFormationConfigRecords(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := cfnClient(s)
	config := configClient(s, "test", "us-east-1")
	_, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("config-records"), TemplateBody: new(configTemplate)},
	)
	if err != nil {
		t.Fatal(err)
	}
	stack := wantStatus(ctx, t, c, "config-records", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&stack)
	if outputs["Recorder"] != "default" || outputs["Channel"] != "default" || outputs["RuleARN"] == "" ||
		outputs["RuleID"] == "" ||
		outputs["RecorderARN"] == "" {
		t.Fatalf("Config refs/attributes: %+v", outputs)
	}
	rules, err := config.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil || len(rules.ConfigRules) != 1 ||
		aws.ToString(rules.ConfigRules[0].InputParameters) != `{"tag1Key":"Owner"}` {
		t.Fatalf("created rule: %+v %v", rules, err)
	}
	status, err := config.DescribeConfigurationRecorderStatus(ctx, &sdk.DescribeConfigurationRecorderStatusInput{})
	if err != nil || len(status.ConfigurationRecordersStatus) != 1 || status.ConfigurationRecordersStatus[0].Recording {
		t.Fatalf("CFN recorder should be stopped: %+v %v", status, err)
	}
	updated := strings.ReplaceAll(configTemplate, "first", "second")
	updated = strings.ReplaceAll(updated, "Six_Hours", "Twelve_Hours")
	updated = strings.ReplaceAll(updated, "config-role", "updated-role")
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("config-records"), TemplateBody: new(updated)})
	if err != nil {
		t.Fatal(err)
	}
	stack = wantStatus(ctx, t, c, "config-records", cfntypes.StackStatusUpdateComplete)
	after := outputsOf(&stack)
	if after["RuleARN"] != outputs["RuleARN"] || after["RuleID"] != outputs["RuleID"] ||
		after["RecorderARN"] != outputs["RecorderARN"] {
		t.Fatalf("identities replaced: before %+v after %+v", outputs, after)
	}
	rules, err = config.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil || aws.ToString(rules.ConfigRules[0].Description) != "second" {
		t.Fatalf("rule update: %+v %v", rules, err)
	}
	recorders, err := config.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	if err != nil ||
		aws.ToString(recorders.ConfigurationRecorders[0].RoleARN) != "arn:aws:iam::000000000000:role/updated-role" {
		t.Fatalf("recorder update: %+v %v", recorders, err)
	}
	channels, err := config.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	if err != nil || aws.ToString(channels.DeliveryChannels[0].S3KeyPrefix) != "second" ||
		channels.DeliveryChannels[0].ConfigSnapshotDeliveryProperties.DeliveryFrequency != "Twelve_Hours" {
		t.Fatalf("channel update: %+v %v", channels, err)
	}
	// Unknown top-level and nested properties are rejected, rather than dropped.
	badRecordingMode := "      RecordingMode: {RecordingFrequency: DAILY}\n      RecordingGroup:"
	badFrequency := "        DeliveryFrequency: Twelve_Hours\n        Unsupported: ignored"
	for _, template := range []string{
		strings.Replace(updated, "      RecordingGroup:", badRecordingMode, 1),
		strings.Replace(updated, "SourceIdentifier: REQUIRED_TAGS", "SourceIdentifier: REQUIRED_TAGS, SourceDetails: []", 1),
		strings.Replace(updated, "        DeliveryFrequency: Twelve_Hours", badFrequency, 1),
	} {
		// Top-level unsupported properties fail validation; nested ones fail the
		// owning service before publication and CloudFormation rolls back.
		_, err = c.UpdateStack(
			ctx,
			&cfn.UpdateStackInput{StackName: new("config-records"), TemplateBody: new(template)},
		)
		if err == nil {
			wantStatus(ctx, t, c, "config-records", cfntypes.StackStatusUpdateRollbackComplete)
		} else {
			code(t, err, "ValidationError")
		}
	}
	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("config-records")})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(ctx, t, c, aws.ToString(stack.StackId), cfntypes.StackStatusDeleteComplete)
	rules, err = config.DescribeConfigRules(ctx, &sdk.DescribeConfigRulesInput{})
	if err != nil || len(rules.ConfigRules) != 0 {
		t.Fatalf("rules not deleted: %+v %v", rules, err)
	}
	recorders, err = config.DescribeConfigurationRecorders(ctx, &sdk.DescribeConfigurationRecordersInput{})
	if err != nil || len(recorders.ConfigurationRecorders) != 0 {
		t.Fatalf("recorders not deleted: %+v %v", recorders, err)
	}
	channels, err = config.DescribeDeliveryChannels(ctx, &sdk.DescribeDeliveryChannelsInput{})
	if err != nil || len(channels.DeliveryChannels) != 0 {
		t.Fatalf("channels not deleted: %+v %v", channels, err)
	}
}
