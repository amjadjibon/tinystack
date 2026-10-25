package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
)

func TestRedshiftServerlessCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := func(team string) *string {
		doc := map[string]any{"Resources": map[string]any{
			"Namespace": map[string]any{
				"Type": "AWS::RedshiftServerless::Namespace",
				"Properties": map[string]any{
					"NamespaceName": "cfn-warehouse",
					"Tags":          []any{map[string]any{"Key": "team", "Value": team}},
				},
			},
			"Workgroup": map[string]any{
				"Type": "AWS::RedshiftServerless::Workgroup",
				"Properties": map[string]any{
					"WorkgroupName": "cfn-warehouse",
					"NamespaceName": map[string]any{"Ref": "Namespace"},
					"Tags":          []any{map[string]any{"Key": "team", "Value": team}},
				},
			},
		}, "Outputs": map[string]any{
			"Endpoint": map[string]any{"Value": map[string]any{
				"Fn::GetAtt": []string{"Workgroup", "Workgroup.Endpoint.Address"},
			}},
		}}

		raw, _ := json.Marshal(doc)
		return new(string(raw))
	}

	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("serverless"), TemplateBody: template("core")})
	if err != nil {
		t.Fatal(err)
	}

	stack := settled(ctx, t, c, "serverless")
	if outputsOf(new(stack))["Endpoint"] == "" {
		t.Fatal(stack)
	}

	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("serverless"), TemplateBody: template("platform")})
	if err != nil {
		t.Fatal(err)
	}

	settled(ctx, t, c, "serverless")
	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("serverless")})
	if err != nil {
		t.Fatal(err)
	}
}
