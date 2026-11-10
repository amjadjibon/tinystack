package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestCloudFormationOpenAPIDefinitions(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	objects := s3Client(s, "test", "us-east-1")
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("openapi-definitions")}); err != nil {
		t.Fatal(err)
	}

	spec := `{"openapi":"3.0.1","info":{"title":"template-api","version":"1.0"},"paths":{"/pets":{"get":{
 "responses":{"200":{"description":"OK"}},"x-amazon-apigateway-integration":{
 "type":"http_proxy","httpMethod":"GET","uri":"https://example.com","payloadFormatVersion":"1.0"}}}}}`
	// REST does not accept payloadFormatVersion.
	restSpec := strings.ReplaceAll(spec, `,"payloadFormatVersion":"1.0"`, "")
	for _, http := range []bool{false, true} {
		body := restSpec
		typ := "AWS::ApiGateway::RestApi"
		samType := "AWS::Serverless::Api"
		if http {
			body = spec
			typ = "AWS::ApiGatewayV2::Api"
			samType = "AWS::Serverless::HttpApi"
		}

		key := fmt.Sprintf("%t.json", http)
		if _, err := objects.PutObject(
			ctx,
			&s3.PutObjectInput{Bucket: new("openapi-definitions"), Key: new(key), Body: strings.NewReader(body)},
		); err != nil {
			t.Fatal(err)
		}

		for _, sam := range []bool{false, true} {
			for _, fromS3 := range []bool{false, true} {
				name := fmt.Sprintf("openapi-%t-%t-%t", http, sam, fromS3)
				t.Run(name, func(t *testing.T) {
					properties := map[string]any{}
					resourceType := typ
					bodyKey := "Body"
					if sam {
						resourceType = samType
						bodyKey = "DefinitionBody"
						properties["StageName"] = "prod"
					}

					if fromS3 {
						bodyKey = "BodyS3Location"
						if sam {
							bodyKey = "DefinitionUri"
						}

						properties[bodyKey] = map[string]any{"Bucket": "openapi-definitions", "Key": key}
					} else {
						var definition map[string]any
						if err := json.Unmarshal([]byte(body), &definition); err != nil {
							t.Fatal(err)
						}

						properties[bodyKey] = definition
					}

					template := map[string]any{
						"Resources": map[string]any{
							"Api": map[string]any{"Type": resourceType, "Properties": properties},
						},
					}

					if sam {
						template["Transform"] = "AWS::Serverless-2016-10-31"
					}

					encoded, err := json.Marshal(template)
					if err != nil {
						t.Fatal(err)
					}

					_, err = cf.CreateStack(ctx, &cfn.CreateStackInput{
						StackName: new(name), TemplateBody: new(string(encoded)),
						Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityAutoExpand},
					})
					if err != nil {
						t.Fatal(err)
					}

					stack := settled(ctx, t, cf, name)
					if stack.StackStatus != cfntypes.StackStatusCreateComplete {
						events, _ := cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: new(name)})
						t.Fatalf("stack: %+v; events: %+v", stack, events)
					}
				})
			}
		}
	}
}
