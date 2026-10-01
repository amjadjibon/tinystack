package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

const apisTemplate = `AWSTemplateFormatVersion: "2010-09-09"
Parameters:
  Path: {Type: String, Default: orders}
  Stage: {Type: String, Default: one}
Resources:
  Role:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Version: "2012-10-17"
        Statement:
          - {Effect: Allow, Principal: {Service: lambda.amazonaws.com}, Action: sts:AssumeRole}
  Function:
    Type: AWS::Lambda::Function
    Properties:
      Runtime: python3.12
      Handler: app.respond
      Role: !GetAtt Role.Arn
      Code: {ZipFile: "def respond(event, context): pass"}
  Rest:
    Type: AWS::ApiGateway::RestApi
    Properties:
      Name: !Sub "${AWS::StackName}-rest"
      BinaryMediaTypes: [image/png]
  Orders:
    Type: AWS::ApiGateway::Resource
    Properties:
      RestApiId: !Ref Rest
      ParentId: !GetAtt Rest.RootResourceId
      PathPart: !Ref Path
  Get:
    Type: AWS::ApiGateway::Method
    Properties:
      RestApiId: !Ref Rest
      ResourceId: !Ref Orders
      HttpMethod: GET
      AuthorizationType: NONE
      Integration:
        Type: AWS_PROXY
        IntegrationHttpMethod: POST
        Uri: !Sub "arn:aws:apigateway:${AWS::Region}:lambda:path/2015-03-31/functions/${Function.Arn}/invocations"
  Deployment:
    Type: AWS::ApiGateway::Deployment
    DependsOn: Get
    Properties:
      RestApiId: !Ref Rest
      Description: !Ref Stage
  Prod:
    Type: AWS::ApiGateway::Stage
    Properties:
      RestApiId: !Ref Rest
      StageName: prod
      DeploymentId: !Ref Deployment
      Variables: {release: !Ref Stage}
  Http:
    Type: AWS::ApiGatewayV2::Api
    Properties:
      Name: !Sub "${AWS::StackName}-http"
      ProtocolType: HTTP
  Integration:
    Type: AWS::ApiGatewayV2::Integration
    Properties:
      ApiId: !Ref Http
      IntegrationType: AWS_PROXY
      IntegrationUri: !GetAtt Function.Arn
      PayloadFormatVersion: "2.0"
  Route:
    Type: AWS::ApiGatewayV2::Route
    Properties:
      ApiId: !Ref Http
      RouteKey: !Sub "GET /${Path}"
      Target: !Sub "integrations/${Integration}"
  Default:
    Type: AWS::ApiGatewayV2::Stage
    Properties:
      ApiId: !Ref Http
      StageName: $default
      AutoDeploy: true
  Invoke:
    Type: AWS::Lambda::Permission
    Properties:
      FunctionName: !Ref Function
      Action: lambda:InvokeFunction
      Principal: apigateway.amazonaws.com
Outputs:
  Rest: {Value: !Ref Rest}
  Http: {Value: !Ref Http}
  Endpoint: {Value: !GetAtt Http.ApiEndpoint}
`

func TestCloudFormationAPIs(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfnClient(s)

	deploy := func(update bool, params ...cfntypes.Parameter) cfntypes.Stack {
		t.Helper()
		var err error
		if update {
			_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{
				StackName: aws.String("apis"), TemplateBody: aws.String(apisTemplate), Parameters: params,
				Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
			})
		} else {
			_, err = c.CreateStack(ctx, &cfn.CreateStackInput{
				StackName: aws.String("apis"), TemplateBody: aws.String(apisTemplate), Parameters: params,
				Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
			})
		}

		if err != nil {
			t.Fatal(err)
		}

		want := cfntypes.StackStatusCreateComplete
		if update {
			want = cfntypes.StackStatusUpdateComplete
		}

		return wantStatus(ctx, t, c, "apis", want)
	}

	st := deploy(false)
	outputs := outputsOf(&st)
	if !strings.HasPrefix(outputs["Endpoint"], "http") {
		t.Fatalf("outputs = %v", outputs)
	}

	status, header, body := call(t, s, http.MethodGet, outputs["Rest"], "/prod/orders", nil, "")
	if status != http.StatusCreated || header.Get("X-Handler") != "respond" || !strings.Contains(body, `"/orders"`) {
		t.Fatalf("REST API = %d %v %s", status, header, body)
	}

	status, _, body = call(t, s, http.MethodGet, outputs["Http"], "/orders", nil, "")
	if status != http.StatusCreated || !strings.Contains(body, `"GET /orders"`) {
		t.Fatalf("HTTP API = %d %s", status, body)
	}

	// A new path moves the resource and the route; a new deployment
	// replaces the old one, which the stage moves off before it is deleted.
	deploy(true,
		cfntypes.Parameter{ParameterKey: aws.String("Path"), ParameterValue: aws.String("invoices")},
		cfntypes.Parameter{ParameterKey: aws.String("Stage"), ParameterValue: aws.String("two")},
	)

	if status, _, body = call(t, s, http.MethodGet, outputs["Rest"], "/prod/invoices", nil, ""); status !=
		http.StatusCreated || !strings.Contains(body, `"release":"two"`) {
		t.Fatalf("REST API = %d %s", status, body)
	}

	if status, _, _ = call(t, s, http.MethodGet, outputs["Rest"], "/prod/orders", nil, ""); status !=
		http.StatusForbidden && status != http.StatusNotFound {
		t.Fatalf("old REST path = %d", status)
	}

	if status, _, _ = call(t, s, http.MethodGet, outputs["Http"], "/invoices", nil, ""); status != http.StatusCreated {
		t.Fatalf("HTTP API = %d", status)
	}

	_, err := c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String("apis")})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "stack apis to be deleted", func() bool {
		_, describeErr := c.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String("apis")})
		return describeErr != nil
	})

	if status, _, _ = call(t, s, http.MethodGet, outputs["Http"], "/invoices", nil, ""); status == http.StatusCreated {
		t.Fatal("the HTTP API outlived its stack")
	}
}
