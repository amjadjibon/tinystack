package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
)

func TestLambdaPermissions(t *testing.T) {
	s := serverWith(t, &fakeRuntime{})
	c := lambdaClient(s, "test")
	ctx := context.Background()
	createFunction(t, c, "api", "app.echo")

	if _, err := c.GetPolicy(ctx, &sdklambda.GetPolicyInput{FunctionName: aws.String("api")}); err == nil {
		t.Fatal("a function without a policy returned one")
	} else if code, status := lambdaCode(err); code != "ResourceNotFoundException" || status != 404 {
		t.Fatalf("%s %d", code, status)
	}

	added, err := c.AddPermission(ctx, &sdklambda.AddPermissionInput{
		FunctionName: aws.String("api"),
		StatementId:  aws.String("apigw"),
		Action:       aws.String("lambda:InvokeFunction"),
		Principal:    aws.String("apigateway.amazonaws.com"),
		SourceArn:    aws.String("arn:aws:execute-api:us-east-1:000000000000:abc123/*"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var st struct {
		Principal map[string]string
		Condition map[string]map[string]string
		Sid       string
		Resource  string
	}
	if err = json.Unmarshal([]byte(aws.ToString(added.Statement)), &st); err != nil {
		t.Fatal(err)
	}

	if st.Sid != "apigw" || st.Principal["Service"] != "apigateway.amazonaws.com" ||
		st.Resource != "arn:aws:lambda:us-east-1:000000000000:function:api" ||
		st.Condition["ArnLike"]["AWS:SourceArn"] == "" {
		t.Fatalf("%+v", st)
	}

	_, err = c.AddPermission(ctx, &sdklambda.AddPermissionInput{
		FunctionName: aws.String("api"), StatementId: aws.String("apigw"),
		Action: aws.String("lambda:InvokeFunction"), Principal: aws.String("sns.amazonaws.com"),
	})
	if code, status := lambdaCode(err); code != "ResourceConflictException" || status != 409 {
		t.Fatalf("duplicate statement: %s %d", code, status)
	}

	_, err = c.AddPermission(ctx, &sdklambda.AddPermissionInput{
		FunctionName: aws.String("api"), StatementId: aws.String("account"),
		Action: aws.String("lambda:GetFunction"), Principal: aws.String("111122223333"),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetPolicy(ctx, &sdklambda.GetPolicyInput{FunctionName: aws.String("api")})
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Statement []struct {
			Principal map[string]string
			Sid       string
		}
	}
	if err = json.Unmarshal([]byte(aws.ToString(got.Policy)), &doc); err != nil {
		t.Fatal(err)
	}

	if len(doc.Statement) != 2 || doc.Statement[1].Principal["AWS"] != "arn:aws:iam::111122223333:root" ||
		aws.ToString(got.RevisionId) == "" {
		t.Fatalf("%s", aws.ToString(got.Policy))
	}

	for _, sid := range []string{"apigw", "account"} {
		_, err = c.RemovePermission(ctx, &sdklambda.RemovePermissionInput{
			FunctionName: aws.String("api"), StatementId: aws.String(sid),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.RemovePermission(ctx, &sdklambda.RemovePermissionInput{
		FunctionName: aws.String("api"), StatementId: aws.String("apigw"),
	})
	if code, _ := lambdaCode(err); code != "ResourceNotFoundException" {
		t.Fatalf("removing a missing statement: %v", err)
	}

	// A version's policy is its own and goes with it.
	_, err = c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: aws.String("api")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.AddPermission(ctx, &sdklambda.AddPermissionInput{
		FunctionName: aws.String("api"), Qualifier: aws.String("1"), StatementId: aws.String("v1"),
		Action: aws.String("lambda:InvokeFunction"), Principal: aws.String("events.amazonaws.com"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.GetPolicy(ctx, &sdklambda.GetPolicyInput{FunctionName: aws.String("api")}); err == nil {
		t.Fatal("$LATEST shares the version's policy")
	}

	if _, err = c.DeleteFunction(ctx, &sdklambda.DeleteFunctionInput{FunctionName: aws.String("api")}); err != nil {
		t.Fatal(err)
	}

	createFunction(t, c, "api", "app.echo")
	_, err = c.PublishVersion(ctx, &sdklambda.PublishVersionInput{FunctionName: aws.String("api")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetPolicy(ctx, &sdklambda.GetPolicyInput{FunctionName: aws.String("api"), Qualifier: aws.String("1")})
	if err == nil {
		t.Fatal("a deleted function's policy outlived it")
	}
}
