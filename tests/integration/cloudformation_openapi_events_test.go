package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

func TestSAMOpenAPIEventMerging(t *testing.T) {
	for _, http := range []bool{false, true} {
		t.Run(fmt.Sprint(http), func(t *testing.T) {
			s := server(t)
			c := cfnClient(s)
			ctx := context.Background()
			typ, event, id := "Api", "Api", "RestApiId"
			if http {
				typ, event, id = "HttpApi", "HttpApi", "ApiId"
			}

			template := fmt.Sprintf(`Transform: AWS::Serverless-2016-10-31
Resources:
  Api:
    Type: AWS::Serverless::%s
    Properties:
      StageName: prod
      DefinitionBody:
        openapi: 3.0.1
        info: {title: merging, version: '1.0'}
        paths:
          /existing:
            get:
              responses: {'200': {description: OK}}
              x-amazon-apigateway-integration:
                type: aws_proxy
                httpMethod: POST
                payloadFormatVersion: '2.0'
                uri: !Sub >-
                  arn:aws:apigateway:${AWS::Region}:lambda:path/2015-03-31/functions/${Function.Arn}/invocations
  Function:
    Type: AWS::Serverless::Function
    Properties:
      Runtime: python3.12
      Handler: app.respond
      InlineCode: 'def respond(event, context): pass'
      Events:
        Existing:
          Type: %s
          Properties: {%s: !Ref Api, Path: /existing, Method: GET}
        Added:
          Type: %s
          Properties: {%s: !Ref Api, Path: /added, Method: POST}
Outputs:
  Api: {Value: !Ref Api}
`, typ, event, id, event, id)
			if !http {
				template = strings.ReplaceAll(template, "                payloadFormatVersion: '2.0'\n", "")
			}

			name := "merging"
			_, err := c.CreateStack(ctx, &cfn.CreateStackInput{
				StackName:    new(name),
				TemplateBody: new(template),
				Capabilities: []cfntypes.Capability{
					cfntypes.CapabilityCapabilityAutoExpand,
					cfntypes.CapabilityCapabilityIam,
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			stack := settled(ctx, t, c, name)
			if stack.StackStatus != cfntypes.StackStatusCreateComplete {
				events, _ := c.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: new(name)})
				for _, e := range events.StackEvents {
					if e.ResourceStatusReason != nil {
						t.Log(*e.ResourceStatusReason)
					}
				}

				t.Fatalf("stack: %s", stack.StackStatus)
			}

			outputs := outputsOf(&stack)
			for _, route := range []struct{ method, path string }{{"GET", "/existing"}, {"POST", "/added"}} {
				status, _, body := call(t, s, route.method, outputs["Api"], "/prod"+route.path, nil, "")
				if status != 201 {
					t.Fatalf("%s: %d %s", route.path, status, body)
				}
			}
		})
	}
}
