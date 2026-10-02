package integration_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

func TestIAMOpenIDConnectProviders(t *testing.T) {
	c := iamClient(server(t), "test", "us-east-1")
	ctx := context.Background()
	created, err := c.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url: aws.String("https://token.actions.githubusercontent.com"), ClientIDList: []string{"sts.amazonaws.com"},
		ThumbprintList: []string{"6938fd4d98bab03faadb97b34396831e3780aea1"},
		Tags:           []iamtypes.Tag{{Key: aws.String("team"), Value: aws.String("ci")}},
	})
	arn := "arn:aws:iam::000000000000:oidc-provider/token.actions.githubusercontent.com"
	if err != nil || aws.ToString(created.OpenIDConnectProviderArn) != arn {
		t.Fatalf("created: %+v %v", created, err)
	}

	var exists *iamtypes.EntityAlreadyExistsException
	if _, err = c.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url: aws.String("https://token.actions.githubusercontent.com"),
	}); !errors.As(err, &exists) {
		t.Fatalf("again: %v", err)
	}

	if _, err = c.AddClientIDToOpenIDConnectProvider(ctx, &iam.AddClientIDToOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String(arn), ClientID: aws.String("my-app"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String(arn),
	})
	if err != nil || aws.ToString(got.Url) != "token.actions.githubusercontent.com" ||
		!slices.Equal(got.ClientIDList, []string{"my-app", "sts.amazonaws.com"}) || len(got.Tags) != 1 ||
		len(got.ThumbprintList) != 1 {
		t.Fatalf("got: %+v %v", got, err)
	}

	if _, err = c.RemoveClientIDFromOpenIDConnectProvider(ctx, &iam.RemoveClientIDFromOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String(arn), ClientID: aws.String("my-app"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UpdateOpenIDConnectProviderThumbprint(ctx, &iam.UpdateOpenIDConnectProviderThumbprintInput{
		OpenIDConnectProviderArn: aws.String(arn), ThumbprintList: []string{},
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String(arn),
	})
	if err != nil || len(got.ClientIDList) != 1 || len(got.ThumbprintList) != 0 {
		t.Fatalf("updated: %+v %v", got, err)
	}

	listed, err := c.ListOpenIDConnectProviders(ctx, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil || len(listed.OpenIDConnectProviderList) != 1 {
		t.Fatalf("list: %+v %v", listed, err)
	}

	var invalid *iamtypes.InvalidInputException
	if _, err = c.CreateOpenIDConnectProvider(ctx, &iam.CreateOpenIDConnectProviderInput{
		Url: aws.String("http://insecure.example"),
	}); err == nil {
		t.Fatal("an http URL was accepted")
	}

	if _, err = c.DeleteOpenIDConnectProvider(ctx, &iam.DeleteOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String(arn),
	}); err != nil {
		t.Fatal(err)
	}

	var missing *iamtypes.NoSuchEntityException
	if _, err = c.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String(arn),
	}); !errors.As(err, &missing) {
		t.Fatalf("deleted: %v", err)
	}

	if _, err = c.GetOpenIDConnectProvider(ctx, &iam.GetOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String("not-an-arn"),
	}); !errors.As(err, &invalid) {
		t.Fatalf("bad ARN: %v", err)
	}
}
