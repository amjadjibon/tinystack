package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
)

func TestS3ControlCloudFormation(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(srv.URL), Credentials: creds("test")})
	template := func(team string) *string {
		doc := map[string]any{"Resources": map[string]any{
			"Bucket": map[string]any{
				"Type":       "AWS::S3::Bucket",
				"Properties": map[string]any{"BucketName": "s3control-cfn-bucket"},
			},
			"Point": map[string]any{
				"Type": "AWS::S3::AccessPoint",
				"Properties": map[string]any{
					"Name":                           "s3control-cfn-point",
					"Bucket":                         map[string]string{"Ref": "Bucket"},
					"PublicAccessBlockConfiguration": map[string]bool{"BlockPublicPolicy": team == "core"},
					"Tags":                           []map[string]string{{"Key": "team", "Value": team}},
				},
			},
			"Multi": map[string]any{
				"Type": "AWS::S3::MultiRegionAccessPoint",
				"Properties": map[string]any{
					"Name":    "s3control-cfn-mrap",
					"Regions": []map[string]any{{"Bucket": map[string]string{"Ref": "Bucket"}}},
				},
			},
		}}
		body, _ := json.Marshal(doc)
		return new(string(body))
	}

	if _, err := c.CreateStack(
		ctx,
		&cfn.CreateStackInput{StackName: new("s3control"), TemplateBody: template("core")},
	); err != nil {
		t.Fatal(err)
	}

	settled(ctx, t, c, "s3control")
	if _, err := c.UpdateStack(
		ctx,
		&cfn.UpdateStackInput{StackName: new("s3control"), TemplateBody: template("platform")},
	); err != nil {
		t.Fatal(err)
	}

	settled(ctx, t, c, "s3control")
	point, err := s3ControlClient(
		srv,
	).GetAccessPoint(ctx, &s3control.GetAccessPointInput{AccountId: new("000000000000"), Name: new("s3control-cfn-point")})
	if err != nil || *point.PublicAccessBlockConfiguration.BlockPublicPolicy {
		t.Fatalf("point %+v %v", point, err)
	}

	if _, err := c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("s3control")}); err != nil {
		t.Fatal(err)
	}
}
