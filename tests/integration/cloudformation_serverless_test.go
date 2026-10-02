package integration_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/smithy-go"
)

const serverlessTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Transform: AWS::Serverless-2016-10-31
Parameters:
  Release: {Type: String, Default: one}
Globals:
  Function:
    Runtime: python3.12
    Timeout: 10
    Environment:
      Variables: {RELEASE: !Ref Release}
Resources:
  Queue: {Type: AWS::SQS::Queue}
  Table:
    Type: AWS::Serverless::SimpleTable
    Properties:
      PrimaryKey: {Name: orderId, Type: String}
  Function:
    Type: AWS::Serverless::Function
    Properties:
      Handler: app.respond
      InlineCode: "def respond(event, context): pass  # CODE"
      AutoPublishAlias: live
      Policies:
        - DynamoDBCrudPolicy: {TableName: !Ref Table}
        - SQSPollerPolicy: {QueueName: !GetAtt Queue.QueueName}
      Events:
        Get:
          Type: Api
          Properties: {Path: "/orders/{id}", Method: get}
        Http:
          Type: HttpApi
          Properties: {Path: /orders, Method: GET}
        Messages:
          Type: SQS
          Properties: {Queue: !GetAtt Queue.Arn, BatchSize: 5}
        Tick:
          Type: Schedule
          Properties: {Schedule: rate(1 day)}
  Machine:
    Type: AWS::Serverless::StateMachine
    Properties:
      Definition: {StartAt: Done, States: {Done: {Type: Pass, End: true}}}
      Policies:
        - CloudWatchPutMetricPolicy: {}
Outputs:
  Api: {Value: !Ref ServerlessRestApi}
  HttpApi: {Value: !Ref ServerlessHttpApi}
  Alias: {Value: !Ref Function.Alias}
  Table: {Value: !Ref Table}
  Machine: {Value: !Ref Machine}
`

func TestCloudFormationServerless(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)

	// CreateStack needs CAPABILITY_AUTO_EXPAND to expand a transform.
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{
		StackName: aws.String("sam"), TemplateBody: aws.String(serverlessTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InsufficientCapabilitiesException" {
		t.Fatalf("CreateStack without CAPABILITY_AUTO_EXPAND = %v", err)
	}

	summary, err := c.GetTemplateSummary(ctx, &cfn.GetTemplateSummaryInput{
		TemplateBody: aws.String(serverlessTemplate),
	})
	if err != nil || len(summary.DeclaredTransforms) != 1 ||
		summary.DeclaredTransforms[0] != "AWS::Serverless-2016-10-31" {
		t.Fatalf("summary = %+v, %v", summary, err)
	}

	// sam deploy creates and executes a change set, acknowledging only IAM.
	deploy := func(body string, typ cfntypes.ChangeSetType, want cfntypes.StackStatus) cfntypes.Stack {
		t.Helper()
		name := aws.String("deploy-" + strings.ToLower(string(typ)))
		_, createErr := c.CreateChangeSet(ctx, &cfn.CreateChangeSetInput{
			StackName: aws.String("sam"), ChangeSetName: name, ChangeSetType: typ, TemplateBody: aws.String(body),
			Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}

		waitFor(t, "the change set", func() bool {
			out, describeErr := c.DescribeChangeSet(ctx, &cfn.DescribeChangeSetInput{
				StackName: aws.String("sam"), ChangeSetName: name,
			})
			if describeErr != nil {
				t.Fatal(describeErr)
			}

			if out.Status == cfntypes.ChangeSetStatusFailed {
				t.Fatalf("change set failed: %s", aws.ToString(out.StatusReason))
			}

			return out.Status == cfntypes.ChangeSetStatusCreateComplete
		})

		_, executeErr := c.ExecuteChangeSet(ctx, &cfn.ExecuteChangeSetInput{
			StackName: aws.String("sam"), ChangeSetName: name,
		})
		if executeErr != nil {
			t.Fatal(executeErr)
		}

		return wantStatus(ctx, t, c, "sam", want)
	}

	st := deploy(serverlessTemplate, cfntypes.ChangeSetTypeCreate, cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)

	status, header, body := call(t, s, http.MethodGet, outputs["Api"], "/Prod/orders/7", nil, "")
	if status != http.StatusCreated || header.Get("X-Handler") != "respond" || !strings.Contains(body, `"/orders/7"`) {
		t.Fatalf("REST API = %d %v %s", status, header, body)
	}

	status, _, body = call(t, s, http.MethodGet, outputs["HttpApi"], "/orders", nil, "")
	if status != http.StatusCreated || !strings.Contains(body, `"GET /orders"`) {
		t.Fatalf("HTTP API = %d %s", status, body)
	}

	functions := lambdaClient(s, "test")
	function := aws.String(outputs["Alias"][:strings.LastIndex(outputs["Alias"], ":")])
	alias, err := functions.GetAlias(ctx, &sdklambda.GetAliasInput{FunctionName: function, Name: aws.String("live")})
	if err != nil || aws.ToString(alias.FunctionVersion) != "1" {
		t.Fatalf("alias = %+v, %v", alias, err)
	}

	mappings, err := functions.ListEventSourceMappings(ctx, &sdklambda.ListEventSourceMappingsInput{
		FunctionName: aws.String(outputs["Alias"]),
	})
	if err != nil || len(mappings.EventSourceMappings) != 1 ||
		aws.ToInt32(mappings.EventSourceMappings[0].BatchSize) != 5 {
		t.Fatalf("mappings = %+v, %v", mappings, err)
	}

	resource, err := c.DescribeStackResource(ctx, &cfn.DescribeStackResourceInput{
		StackName: aws.String("sam"), LogicalResourceId: aws.String("FunctionTick"),
	})
	if err != nil {
		t.Fatal(err)
	}

	targets, err := eventsClient(s, "test").ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{
		Rule: resource.StackResourceDetail.PhysicalResourceId,
	})
	if err != nil || len(targets.Targets) != 1 || aws.ToString(targets.Targets[0].Arn) != outputs["Alias"] {
		t.Fatalf("targets = %+v, %v", targets, err)
	}

	table, err := client(s, "test", "us-east-1").DescribeTable(ctx, &sdk.DescribeTableInput{
		TableName: aws.String(outputs["Table"]),
	})
	if err != nil || aws.ToString(table.Table.KeySchema[0].AttributeName) != "orderId" {
		t.Fatalf("table = %+v, %v", table, err)
	}

	processed, err := c.GetTemplate(ctx, &cfn.GetTemplateInput{
		StackName: aws.String("sam"), TemplateStage: cfntypes.TemplateStageProcessed,
	})
	if err != nil || strings.Contains(aws.ToString(processed.TemplateBody), "AWS::Serverless") ||
		!strings.Contains(aws.ToString(processed.TemplateBody), "ServerlessRestApiProdStage") {
		t.Fatalf("processed = %v, %v", aws.ToString(processed.TemplateBody), err)
	}

	original, err := c.GetTemplate(ctx, &cfn.GetTemplateInput{StackName: aws.String("sam")})
	if err != nil || aws.ToString(original.TemplateBody) != serverlessTemplate {
		t.Fatalf("original = %v, %v", aws.ToString(original.TemplateBody), err)
	}

	// New code publishes a new version under a new logical ID, and the alias
	// and the API, which invoke the alias, move to it.
	deploy(strings.Replace(serverlessTemplate, "# CODE", "# NEW CODE", 1), cfntypes.ChangeSetTypeUpdate,
		cfntypes.StackStatusUpdateComplete)
	alias, err = functions.GetAlias(ctx, &sdklambda.GetAliasInput{FunctionName: function, Name: aws.String("live")})
	if err != nil || aws.ToString(alias.FunctionVersion) != "2" {
		t.Fatalf("alias = %+v, %v", alias, err)
	}

	if status, _, _ = call(t, s, http.MethodGet, outputs["Api"], "/Prod/orders/8", nil, ""); status !=
		http.StatusCreated {
		t.Fatalf("REST API after the update = %d", status)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("sam")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack sam to be deleted", func() bool {
		_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("sam")})
		return describeErr != nil
	})

	if _, err = functions.GetFunction(ctx, &sdklambda.GetFunctionInput{FunctionName: function}); err == nil {
		t.Fatal("the function outlived its stack")
	}
}
