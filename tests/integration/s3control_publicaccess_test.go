package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	"github.com/aws/aws-sdk-go-v2/service/s3control/types"
)

func TestS3ControlAccountPublicAccessBlock(t *testing.T) {
	ctx := context.Background()
	server := server(t)
	c := s3ControlClient(server)
	b := s3Client(server, "test", "us-east-1")
	account := new("000000000000")
	if _, err := c.GetPublicAccessBlock(ctx, &s3control.GetPublicAccessBlockInput{AccountId: account}); err == nil {
		t.Fatal("unset configuration succeeded")
	}

	if _, err := b.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("account-bpa-bucket")}); err != nil {
		t.Fatal(err)
	}

	if _, err := b.DeletePublicAccessBlock(
		ctx,
		&s3.DeletePublicAccessBlockInput{Bucket: new("account-bpa-bucket")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutPublicAccessBlock(
		ctx,
		&s3control.PutPublicAccessBlockInput{
			AccountId:                      account,
			PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{BlockPublicPolicy: new(true)},
		},
	); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetPublicAccessBlock(ctx, &s3control.GetPublicAccessBlockInput{AccountId: account})
	if err != nil || !*got.PublicAccessBlockConfiguration.BlockPublicPolicy {
		t.Fatalf("get %+v %v", got, err)
	}

	policy := new(
		`{"Statement":[{"Effect":"Allow","Principal":"*"` +
			`,"Action":"s3:GetObject","Resource":"arn:aws:s3:::account-bpa-bucket/*"}]}`,
	)
	if _, err = c.PutAccessPointPolicy(
		ctx,
		&s3control.PutAccessPointPolicyInput{AccountId: account, Name: new("missing"), Policy: policy},
	); err == nil {
		t.Fatal("missing access point succeeded")
	}

	if _, err = b.PutBucketPolicy(
		ctx,
		&s3.PutBucketPolicyInput{Bucket: new("account-bpa-bucket"), Policy: policy},
	); err == nil {
		t.Fatal("account block did not reject policy")
	}

	if _, err = c.DeletePublicAccessBlock(
		ctx,
		&s3control.DeletePublicAccessBlockInput{AccountId: account},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = b.PutBucketPolicy(
		ctx,
		&s3.PutBucketPolicyInput{Bucket: new("account-bpa-bucket"), Policy: policy},
	); err != nil {
		t.Fatal(err)
	}
}
