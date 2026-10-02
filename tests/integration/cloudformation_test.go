package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"
)

func cfnClient(s *httptest.Server) *cfn.Client {
	return cfn.New(cfn.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// settled waits until a stack's operation ends and returns the stack.
func settled(ctx context.Context, t *testing.T, c *cfn.Client, name string) cfntypes.Stack {
	t.Helper()
	var st cfntypes.Stack
	waitFor(t, "stack "+name+" to settle", func() bool {
		out, err := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: &name})
		if err != nil {
			t.Fatal(err)
		}

		st = out.Stacks[0]
		return !strings.HasSuffix(string(st.StackStatus), "_IN_PROGRESS")
	})

	return st
}

func wantStatus(
	ctx context.Context,
	t *testing.T,
	c *cfn.Client,
	name string,
	want cfntypes.StackStatus,
) cfntypes.Stack {
	t.Helper()
	st := settled(ctx, t, c, name)
	if st.StackStatus != want {
		events, _ := c.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: &name})
		var reasons []string
		for i := range events.StackEvents {
			e := &events.StackEvents[i]
			reasons = append(reasons, aws.ToString(e.LogicalResourceId)+" "+string(e.ResourceStatus)+" "+
				aws.ToString(e.ResourceStatusReason))
		}

		t.Fatalf("stack %s is %s (%s), want %s; events:\n%s", name, st.StackStatus,
			aws.ToString(st.StackStatusReason), want, strings.Join(reasons, "\n"))
	}

	return st
}

func outputsOf(st *cfntypes.Stack) map[string]string {
	out := map[string]string{}
	for _, o := range st.Outputs {
		out[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}

	return out
}

const appTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Description: An application
Parameters:
  Env:
    Type: String
    Default: dev
    AllowedValues: [dev, prod]
  Retention:
    Type: Number
    Default: 7
Conditions:
  IsDev: !Equals [!Ref Env, dev]
Mappings:
  Sizes:
    dev: {Timeout: 30}
    prod: {Timeout: 60}
Resources:
  DeadLetters:
    Type: AWS::SQS::Queue
  Queue:
    Type: AWS::SQS::Queue
    Properties:
      VisibilityTimeout: !FindInMap [Sizes, !Ref Env, Timeout]
      RedrivePolicy:
        deadLetterTargetArn: !GetAtt DeadLetters.Arn
        maxReceiveCount: 3
      Tags:
        - Key: env
          Value: !Ref Env
  Topic:
    Type: AWS::SNS::Topic
    Properties:
      DisplayName: !Sub "${AWS::StackName} alerts"
  Subscription:
    Type: AWS::SNS::Subscription
    Properties:
      TopicArn: !Ref Topic
      Protocol: sqs
      Endpoint: !GetAtt Queue.Arn
  Bucket:
    Type: AWS::S3::Bucket
    Properties:
      BucketName: !Sub "${AWS::StackName}-${Env}-data"
      VersioningConfiguration: {Status: Enabled}
  Table:
    Type: AWS::DynamoDB::Table
    Properties:
      AttributeDefinitions: [{AttributeName: id, AttributeType: S}]
      KeySchema: [{AttributeName: id, KeyType: HASH}]
      BillingMode: PAY_PER_REQUEST
      StreamSpecification: {StreamViewType: NEW_IMAGE}
  QueueName:
    Type: AWS::SSM::Parameter
    Properties:
      Name: !Sub "/${AWS::StackName}/queue"
      Type: String
      Value: !GetAtt Queue.QueueName
  Logs:
    Type: AWS::Logs::LogGroup
    Condition: IsDev
    Properties:
      RetentionInDays: !Ref Retention
  Role:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Version: "2012-10-17"
        Statement:
          - Effect: Allow
            Principal: {Service: lambda.amazonaws.com}
            Action: sts:AssumeRole
      Policies:
        - PolicyName: queue
          PolicyDocument:
            Version: "2012-10-17"
            Statement: [{Effect: Allow, Action: "sqs:*", Resource: !GetAtt Queue.Arn}]
  Function:
    Type: AWS::Lambda::Function
    Properties:
      Runtime: python3.12
      Handler: index.handler
      Role: !GetAtt Role.Arn
      Environment:
        Variables:
          QUEUE: !Ref Queue
      Code:
        ZipFile: |
          def handler(event, context):
              return {"ok": True}
  Mapping:
    Type: AWS::Lambda::EventSourceMapping
    Properties:
      EventSourceArn: !GetAtt Queue.Arn
      FunctionName: !Ref Function
      BatchSize: 5
  Rule:
    Type: AWS::Events::Rule
    Properties:
      EventPattern: {source: [app]}
      Targets:
        - Id: queue
          Arn: !GetAtt Queue.Arn
  Secret:
    Type: AWS::SecretsManager::Secret
    Properties:
      GenerateSecretString:
        SecretStringTemplate: '{"user": "app"}'
        GenerateStringKey: password
        PasswordLength: 20
        ExcludePunctuation: true
  Key:
    Type: AWS::KMS::Key
    Properties:
      Description: app key
      EnableKeyRotation: true
  Alias:
    Type: AWS::KMS::Alias
    Properties:
      AliasName: !Sub "alias/${AWS::StackName}"
      TargetKeyId: !Ref Key
Outputs:
  QueueUrl:
    Value: !Ref Queue
    Export:
      Name: !Sub "${AWS::StackName}-queue"
  FunctionArn:
    Value: !GetAtt Function.Arn
  Joined:
    Value: !Join [",", [!Ref Env, !Select [1, !Split ["-", "a-b-c"]], !Select [0, !GetAZs ""]]]
  LogGroup:
    Condition: IsDev
    Value: !Ref Logs
`

func TestCloudFormationStackLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)

	_, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: aws.String("app"), TemplateBody: aws.String(appTemplate)},
	)
	wantCode(t, err, "InsufficientCapabilitiesException")

	created, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("app"), TemplateBody: aws.String(appTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Tags:         []cfntypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("app"), TemplateBody: aws.String(appTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	})
	wantCode(t, err, "AlreadyExistsException")

	st := wantStatus(ctx, t, c, "app", cfntypes.StackStatusCreateComplete)
	if aws.ToString(st.StackId) != aws.ToString(created.StackId) || aws.ToString(st.Description) != "An application" ||
		len(st.Tags) != 1 || len(st.Parameters) != 2 {
		t.Fatalf("stack = %+v", st)
	}

	outputs := outputsOf(&st)
	if !strings.Contains(outputs["QueueUrl"], "/000000000000/app-Queue-") ||
		!strings.HasPrefix(outputs["FunctionArn"], "arn:aws:lambda:us-east-1:000000000000:function:app-Function-") ||
		outputs["Joined"] != "dev,b,us-east-1a" || !strings.HasPrefix(outputs["LogGroup"], "app-Logs-") {
		t.Fatalf("outputs = %v", outputs)
	}

	// The resources exist in the services that own them.
	param, err := ssmClient(
		s,
		"test",
		"us-east-1",
	).GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/app/queue")})
	if err != nil || !strings.HasPrefix(aws.ToString(param.Parameter.Value), "app-Queue-") {
		t.Fatalf("parameter = %+v, %v", param, err)
	}

	queues := sqsClient(s, "test")
	attrs, err := queues.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(outputs["QueueUrl"]), AttributeNames: []sqstypes.QueueAttributeName{"All"},
	})
	if err != nil || attrs.Attributes["VisibilityTimeout"] != "30" ||
		!strings.Contains(attrs.Attributes["RedrivePolicy"], "app-DeadLetters-") {
		t.Fatalf("queue attributes = %v, %v", attrs, err)
	}

	if _, err = s3Client(s, "test", "us-east-1").HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String("app-dev-data"),
	}); err != nil {
		t.Fatal(err)
	}

	fn, err := lambdaClient(s, "test").GetFunction(ctx, &sdklambda.GetFunctionInput{
		FunctionName: aws.String(outputs["FunctionArn"]),
	})
	if err != nil || fn.Configuration.Environment.Variables["QUEUE"] != outputs["QueueUrl"] {
		t.Fatalf("function = %+v, %v", fn, err)
	}

	resources, err := c.DescribeStackResources(ctx, &cfn.DescribeStackResourcesInput{StackName: aws.String("app")})
	if err != nil || len(resources.StackResources) != 15 {
		t.Fatalf("resources = %+v, %v", resources, err)
	}

	var secretID string
	for _, r := range resources.StackResources {
		if r.ResourceStatus != cfntypes.ResourceStatusCreateComplete {
			t.Fatalf("resource = %+v", r)
		}

		if aws.ToString(r.LogicalResourceId) == "Secret" {
			secretID = aws.ToString(r.PhysicalResourceId)
		}
	}

	secret, err := secretsClient(s, "test", "us-east-1").GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &secretID,
	})
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]string
	if err = json.Unmarshal([]byte(aws.ToString(secret.SecretString)), &doc); err != nil || doc["user"] != "app" ||
		len(doc["password"]) != 20 || strings.ContainsAny(doc["password"], "!#$%") {
		t.Fatalf("secret = %v, %v", doc, err)
	}

	events, err := c.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: aws.String("app")})
	if err != nil || aws.ToString(events.StackEvents[0].LogicalResourceId) != "app" ||
		events.StackEvents[0].ResourceStatus != cfntypes.ResourceStatusCreateComplete {
		t.Fatalf("events = %+v, %v", events, err)
	}

	template, err := c.GetTemplate(ctx, &cfn.GetTemplateInput{StackName: aws.String("app")})
	if err != nil || aws.ToString(template.TemplateBody) != appTemplate {
		t.Fatalf("template = %v", err)
	}

	// An update to prod changes the queue in place, renames the bucket, and
	// deletes the log group, whose condition no longer holds.
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("app"), UsePreviousTemplate: aws.Bool(true),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Parameters: []cfntypes.Parameter{
			{ParameterKey: aws.String("Env"), ParameterValue: aws.String("prod")},
			{ParameterKey: aws.String("Retention"), UsePreviousValue: aws.Bool(true)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	st = wantStatus(ctx, t, c, "app", cfntypes.StackStatusUpdateComplete)
	if updated := outputsOf(&st); updated["QueueUrl"] != outputs["QueueUrl"] || updated["LogGroup"] != "" {
		t.Fatalf("outputs = %v", updated)
	}

	attrs, err = queues.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(outputs["QueueUrl"]), AttributeNames: []sqstypes.QueueAttributeName{"VisibilityTimeout"},
	})
	if err != nil || attrs.Attributes["VisibilityTimeout"] != "60" {
		t.Fatalf("queue attributes = %v, %v", attrs, err)
	}

	if _, err = s3Client(s, "test", "us-east-1").HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String("app-dev-data"),
	}); err == nil {
		t.Fatal("the replaced bucket still exists")
	}

	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("app"), UsePreviousTemplate: aws.Bool(true),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Parameters: []cfntypes.Parameter{
			{ParameterKey: aws.String("Env"), UsePreviousValue: aws.Bool(true)},
			{ParameterKey: aws.String("Retention"), UsePreviousValue: aws.Bool(true)},
		},
	})
	wantMessage(t, err, "No updates are to be performed.")

	// Another stack imports the queue's URL, which keeps the first stack.
	_, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("consumer"), TemplateBody: aws.String(`{
		  "Resources": {"Url": {"Type": "AWS::SSM::Parameter", "Properties": {
		    "Type": "String", "Value": {"Fn::ImportValue": "app-queue"}}}}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "consumer", cfntypes.StackStatusCreateComplete)
	imports, err := c.ListImports(ctx, &cfn.ListImportsInput{ExportName: aws.String("app-queue")})
	if err != nil || !slices.Equal(imports.Imports, []string{"consumer"}) {
		t.Fatalf("imports = %+v, %v", imports, err)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("app")})
	wantMessage(t, err, "Export app-queue cannot be deleted as it is in use by consumer")

	for _, name := range []string{"consumer", "app"} {
		if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String(name)}); err != nil {
			t.Fatal(err)
		}

		waitFor(t, "stack "+name+" to be deleted", func() bool {
			_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String(name)})
			return describeErr != nil
		})
	}

	deleted, err := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: created.StackId})
	if err != nil || deleted.Stacks[0].StackStatus != cfntypes.StackStatusDeleteComplete {
		t.Fatalf("deleted = %+v, %v", deleted, err)
	}

	if _, err = queues.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(outputs["QueueUrl"]),
	}); err == nil {
		t.Fatal("the stack's queue still exists")
	}

	listed, err := c.ListStacks(ctx, &cfn.ListStacksInput{
		StackStatusFilter: []cfntypes.StackStatus{cfntypes.StackStatusDeleteComplete},
	})
	if err != nil || len(listed.StackSummaries) != 2 {
		t.Fatalf("listed = %+v, %v", listed, err)
	}

	// Deleting a stack that does not exist succeeds.
	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("app")}); err != nil {
		t.Fatal(err)
	}
}

// wantMessage fails unless err is an API error whose message is message.
func wantMessage(t *testing.T, err error, message string) {
	t.Helper()
	apiErr, ok := errors.AsType[smithy.APIError](err)
	if !ok || apiErr.ErrorMessage() != message {
		t.Fatalf("got %v, want %q", err, message)
	}
}

func TestCloudFormationRollback(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	broken := `Resources:
  Bucket:
    Type: AWS::S3::Bucket
    Properties: {BucketName: kept-data}
  Queue:
    Type: AWS::SQS::Queue
    DependsOn: Bucket
    Properties: {DelaySeconds: 9999}
`
	if _, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("broken"), TemplateBody: aws.String(broken),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "broken", cfntypes.StackStatusRollbackComplete)
	buckets := s3Client(s, "test", "us-east-1")
	if _, err := buckets.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("kept-data")}); err == nil {
		t.Fatal("the rollback kept the bucket")
	}

	events, err := c.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: aws.String("broken")})
	if err != nil {
		t.Fatal(err)
	}

	failed := slices.IndexFunc(events.StackEvents, func(e cfntypes.StackEvent) bool {
		return aws.ToString(e.LogicalResourceId) == "Queue" && e.ResourceStatus == cfntypes.ResourceStatusCreateFailed
	})
	if failed < 0 || !strings.Contains(aws.ToString(events.StackEvents[failed].ResourceStatusReason), "DelaySeconds") {
		t.Fatalf("events = %+v", events.StackEvents)
	}

	rollback := slices.IndexFunc(events.StackEvents, func(e cfntypes.StackEvent) bool {
		return e.ResourceStatus == cfntypes.ResourceStatusRollbackInProgress
	})
	if rollback < 0 || aws.ToString(events.StackEvents[rollback].ResourceStatusReason) !=
		"The following resource(s) failed to create: [Queue]. Rollback requested by user." {
		t.Fatalf("events = %+v", events.StackEvents)
	}

	// A stack in ROLLBACK_COMPLETE can only be deleted.
	_, err = c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: aws.String("broken"), TemplateBody: aws.String(broken)},
	)
	wantCode(t, err, "ValidationError")
	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("broken")}); err != nil {
		t.Fatal(err)
	}

	// Without rollback the stack keeps what it created.
	if _, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("kept"), TemplateBody: aws.String(broken), OnFailure: cfntypes.OnFailureDoNothing,
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "kept", cfntypes.StackStatusCreateFailed)
	if _, err = buckets.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("kept-data")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("kept")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack kept to be deleted", func() bool {
		_, loadErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("kept")})
		return loadErr != nil
	})

	// A failed update rolls back to the previous template.
	good := `Resources:
  Queue:
    Type: AWS::SQS::Queue
    Properties: {DelaySeconds: 5}
  Param:
    Type: AWS::SSM::Parameter
    Properties: {Name: /rollback/value, Type: String, Value: before}
`
	bad := `Resources:
  Queue:
    Type: AWS::SQS::Queue
    Properties: {DelaySeconds: 9999}
  Param:
    Type: AWS::SSM::Parameter
    DependsOn: Queue
    Properties: {Name: /rollback/value, Type: String, Value: after}
  Extra:
    Type: AWS::SNS::Topic
`
	if _, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("updated"), TemplateBody: aws.String(good),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "updated", cfntypes.StackStatusCreateComplete)
	if _, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: aws.String("updated"), TemplateBody: aws.String(bad),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "updated", cfntypes.StackStatusUpdateRollbackComplete)
	template, err := c.GetTemplate(ctx, &cfn.GetTemplateInput{StackName: aws.String("updated")})
	if err != nil || aws.ToString(template.TemplateBody) != good {
		t.Fatalf("template = %v", err)
	}

	resources, err := c.ListStackResources(ctx, &cfn.ListStackResourcesInput{StackName: aws.String("updated")})
	if err != nil || len(resources.StackResourceSummaries) != 2 {
		t.Fatalf("resources = %+v, %v", resources, err)
	}

	param, err := ssmClient(s, "test", "us-east-1").GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String("/rollback/value"),
	})
	if err != nil || aws.ToString(param.Parameter.Value) != "before" {
		t.Fatalf("parameter = %+v, %v", param, err)
	}
}

func TestCloudFormationChangeSets(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	first := `{"Resources": {"Topic": {"Type": "AWS::SNS::Topic"}}}`
	created, err := c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
		StackName: aws.String(
			"sets",
		),
		ChangeSetName: aws.String("initial"),
		ChangeSetType: cfntypes.ChangeSetTypeCreate,
		TemplateBody:  aws.String(first),
	})
	if err != nil {
		t.Fatal(err)
	}

	review, err := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("sets")})
	if err != nil || review.Stacks[0].StackStatus != cfntypes.StackStatusReviewInProgress {
		t.Fatalf("review = %+v, %v", review, err)
	}

	described, err := c.DescribeChangeSet(ctx, &cfn.DescribeChangeSetInput{ChangeSetName: created.Id})
	if err != nil || described.Status != cfntypes.ChangeSetStatusCreateComplete ||
		described.ExecutionStatus != cfntypes.ExecutionStatusAvailable || len(described.Changes) != 1 ||
		described.Changes[0].ResourceChange.Action != cfntypes.ChangeActionAdd {
		t.Fatalf("change set = %+v, %v", described, err)
	}

	if _, err = c.ExecuteChangeSet(ctx, &cfn.ExecuteChangeSetInput{
		ChangeSetName: aws.String("initial"), StackName: aws.String("sets"),
	}); err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, c, "sets", cfntypes.StackStatusCreateComplete)
	if aws.ToString(st.ChangeSetId) != aws.ToString(created.Id) {
		t.Fatalf("stack = %+v", st)
	}

	executed, err := c.DescribeChangeSet(ctx, &cfn.DescribeChangeSetInput{ChangeSetName: created.Id})
	if err != nil || executed.ExecutionStatus != cfntypes.ExecutionStatusExecuteComplete {
		t.Fatalf("executed = %+v, %v", executed, err)
	}

	// A change set without changes fails, as "aws cloudformation deploy"
	// expects.
	same, err := c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
		StackName: aws.String("sets"), ChangeSetName: aws.String("same"), TemplateBody: aws.String(first),
	})
	if err != nil {
		t.Fatal(err)
	}

	unchanged, err := c.DescribeChangeSet(ctx, &cfn.DescribeChangeSetInput{ChangeSetName: same.Id})
	if err != nil || unchanged.Status != cfntypes.ChangeSetStatusFailed ||
		!strings.Contains(aws.ToString(unchanged.StatusReason), "didn't contain changes") {
		t.Fatalf("unchanged = %+v, %v", unchanged, err)
	}

	_, err = c.ExecuteChangeSet(ctx, &cfn.ExecuteChangeSetInput{ChangeSetName: same.Id})
	wantCode(t, err, "InvalidChangeSetStatus")

	second := `{"Resources": {
	  "Topic": {"Type": "AWS::SNS::Topic", "Properties": {"TopicName": "renamed"}},
	  "Queue": {"Type": "AWS::SQS::Queue"}}}`
	if _, err = c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
		StackName: aws.String("sets"), ChangeSetName: aws.String("rename"), TemplateBody: aws.String(second),
	}); err != nil {
		t.Fatal(err)
	}

	renamed, err := c.DescribeChangeSet(ctx, &cfn.DescribeChangeSetInput{
		StackName: aws.String("sets"), ChangeSetName: aws.String("rename"),
	})
	if err != nil || len(renamed.Changes) != 2 {
		t.Fatalf("renamed = %+v, %v", renamed, err)
	}

	actions := map[string]cfntypes.ResourceChange{}
	for _, change := range renamed.Changes {
		actions[aws.ToString(change.ResourceChange.LogicalResourceId)] = *change.ResourceChange
	}

	if actions["Queue"].Action != cfntypes.ChangeActionAdd || actions["Topic"].Action != cfntypes.ChangeActionModify ||
		actions["Topic"].Replacement != cfntypes.ReplacementTrue {
		t.Fatalf("changes = %+v", actions)
	}

	sets, err := c.ListChangeSets(ctx, &cfn.ListChangeSetsInput{StackName: aws.String("sets")})
	if err != nil || len(sets.Summaries) != 3 {
		t.Fatalf("change sets = %+v, %v", sets, err)
	}

	if _, err = c.ExecuteChangeSet(ctx, &cfn.ExecuteChangeSetInput{
		StackName: aws.String("sets"), ChangeSetName: aws.String("rename"),
	}); err != nil {
		t.Fatal(err)
	}

	wantStatus(ctx, t, c, "sets", cfntypes.StackStatusUpdateComplete)
	topic, err := c.DescribeStackResource(ctx, &cfn.DescribeStackResourceInput{
		StackName: aws.String("sets"), LogicalResourceId: aws.String("Topic"),
	})
	if err != nil || aws.ToString(topic.StackResourceDetail.PhysicalResourceId) !=
		"arn:aws:sns:us-east-1:000000000000:renamed" {
		t.Fatalf("topic = %+v, %v", topic, err)
	}

	// Executing a change set removes the stack's other change sets.
	sets, err = c.ListChangeSets(ctx, &cfn.ListChangeSetsInput{StackName: aws.String("sets")})
	if err != nil || len(sets.Summaries) != 1 {
		t.Fatalf("change sets = %+v, %v", sets, err)
	}

	if _, err = c.DeleteChangeSet(ctx, &cfn.DeleteChangeSetInput{
		StackName: aws.String("sets"), ChangeSetName: aws.String("rename"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeChangeSet(ctx, &cfn.DescribeChangeSetInput{
		StackName: aws.String("sets"), ChangeSetName: aws.String("rename"),
	})
	wantCode(t, err, "ChangeSetNotFound")
}

func TestCloudFormationTemplates(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)
	for _, tc := range []struct{ name, body, message string }{
		{
			"resource type", `{"Resources": {"Vpc": {"Type": "AWS::EC2::NatGateway"}}}`,
			"TinyStack does not support these resource types: [AWS::EC2::NatGateway]",
		},
		{
			"property", `{"Resources": {"Q": {"Type": "AWS::SQS::Queue", "Properties": {"Policy": "x"}}}}`,
			"TinyStack does not support the Policy property of AWS::SQS::Queue (resource Q)",
		},
		{
			"function", `{"Resources": {"Q": {"Type": "AWS::SQS::Queue", "Properties": {
		  "QueueName": {"Fn::Cidr": ["10.0.0.0/16", 2, 8]}}}}}`,
			"TinyStack does not support the intrinsic function Fn::Cidr (/Resources/Q)",
		},
		{
			"transform", "Transform: AWS::LanguageExtensions\nResources:\n  Q: {Type: AWS::SQS::Queue}\n",
			"TinyStack does not support template transforms: [AWS::LanguageExtensions]",
		},
		{
			"cycle", `{"Resources": {
		  "A": {"Type": "AWS::SQS::Queue", "DependsOn": "B"},
		  "B": {"Type": "AWS::SQS::Queue", "DependsOn": "A"}}}`,
			"Circular dependency between resources: [A, B, A]",
		},
		{
			"attribute", `{"Resources": {"Q": {"Type": "AWS::SQS::Queue"}},
		  "Outputs": {"O": {"Value": {"Fn::GetAtt": ["Q", "Nope"]}}}}`,
			"Template error: resource Q does not support attribute type Nope in Fn::GetAtt",
		},
		{
			"reference", `{"Resources": {"Q": {"Type": "AWS::SQS::Queue", "Properties": {"QueueName": {"Ref": "Nope"}}}}}`,
			"Template error: Unresolved resource dependencies [Nope] in /Resources/Q",
		},
		{"no resources", `{"Resources": {}}`, "Template format error: At least one Resources member must be defined."},
		{
			"policy", `{"Resources": {"Q": {"Type": "AWS::SQS::Queue", "UpdatePolicy": {}}}}`,
			"TinyStack does not support the UpdatePolicy attribute (resource Q)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ValidateTemplate(ctx, &cfn.ValidateTemplateInput{TemplateBody: aws.String(tc.body)})
			wantMessage(t, err, tc.message)
		})
	}

	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("params"), TemplateBody: aws.String(appTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		Parameters:   []cfntypes.Parameter{{ParameterKey: aws.String("Env"), ParameterValue: aws.String("qa")}},
	})
	wantMessage(t, err, "Parameter 'Env' must be one of AllowedValues")

	validated, err := c.ValidateTemplate(ctx, &cfn.ValidateTemplateInput{TemplateBody: aws.String(appTemplate)})
	if err != nil || len(validated.Parameters) != 2 || aws.ToString(validated.Description) != "An application" ||
		!slices.Equal(validated.Capabilities, []cfntypes.Capability{cfntypes.CapabilityCapabilityIam}) {
		t.Fatalf("validated = %+v, %v", validated, err)
	}

	summary, err := c.GetTemplateSummary(ctx, &cfn.GetTemplateSummaryInput{TemplateBody: aws.String(appTemplate)})
	if err != nil || len(summary.ResourceTypes) != 14 || aws.ToString(summary.Version) != "2010-09-09" {
		t.Fatalf("summary = %+v, %v", summary, err)
	}

	// Templates can come from S3, and named IAM resources need
	// CAPABILITY_NAMED_IAM.
	buckets := s3Client(s, "test", "us-east-1")
	if _, err = buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("templates")}); err != nil {
		t.Fatal(err)
	}

	named := `Parameters:
  Secret:
    Type: String
    NoEcho: true
Resources:
  Role:
    Type: AWS::IAM::Role
    Properties:
      RoleName: named-role
      AssumeRolePolicyDocument: {Version: "2012-10-17", Statement: []}
`
	if _, err = buckets.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("templates"), Key: aws.String("iam/role.yaml"), Body: strings.NewReader(named),
	}); err != nil {
		t.Fatal(err)
	}

	location := aws.String("https://templates.s3.us-east-1.amazonaws.com/iam/role.yaml")
	secret := []cfntypes.Parameter{{ParameterKey: aws.String("Secret"), ParameterValue: aws.String("hunter2")}}
	_, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("named"), TemplateURL: location, Parameters: secret,
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	})
	wantMessage(t, err, "Requires capabilities : [CAPABILITY_NAMED_IAM]")

	if _, err = c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:   aws.String("named"),
		TemplateURL: location,
		Parameters:  secret,
		Capabilities: []cfntypes.Capability{
			cfntypes.CapabilityCapabilityNamedIam,
		},
		EnableTerminationProtection: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	st := wantStatus(ctx, t, c, "named", cfntypes.StackStatusCreateComplete)
	if aws.ToString(st.Parameters[0].ParameterValue) != "****" {
		t.Fatalf("parameters = %+v", st.Parameters)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("named")})
	wantMessage(t, err, "Stack [named] cannot be deleted while TerminationProtection is enabled")
	if _, err = c.UpdateTerminationProtection(ctx, &cfn.UpdateTerminationProtectionInput{
		StackName: aws.String("named"), EnableTerminationProtection: aws.Bool(false),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("named")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack named to be deleted", func() bool {
		_, loadErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("named")})
		return loadErr != nil
	})

	_, err = c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("named")})
	wantMessage(t, err, "Stack with id named does not exist")
}
