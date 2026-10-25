package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	"github.com/aws/aws-sdk-go-v2/service/s3control/types"
	"github.com/aws/smithy-go"
)

func TestS3ControlErrors(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	c := s3ControlClient(srv)
	b := s3Client(srv, "test", "us-east-1")
	account := new("000000000000")
	if _, err := b.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("control-errors-bucket")}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateAccessPoint(
		ctx,
		&s3control.CreateAccessPointInput{
			AccountId: account,
			Name:      new("control-errors"),
			Bucket:    new("control-errors-bucket"),
		},
	); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		call       func() error
		name, code string
	}{
		{name: "missing bucket", code: "NoSuchBucket", call: func() error {
			_, err := c.CreateAccessPoint(ctx, &s3control.CreateAccessPointInput{
				AccountId: account,
				Name:      new("missing-bucket"),
				Bucket:    new("absent-bucket"),
			})
			return err
		}},
		{name: "missing VPC", code: "InvalidVpcID.NotFound", call: func() error {
			_, err := c.CreateAccessPoint(ctx, &s3control.CreateAccessPointInput{
				AccountId:        account,
				Name:             new("missing-vpc"),
				Bucket:           new("control-errors-bucket"),
				VpcConfiguration: &types.VpcConfiguration{VpcId: new("vpc-00000000")},
			})
			return err
		}},
		{name: "duplicate name", code: "AccessPointAlreadyOwnedByYou", call: func() error {
			_, err := c.CreateAccessPoint(ctx, &s3control.CreateAccessPointInput{
				AccountId: account,
				Name:      new("control-errors"),
				Bucket:    new("control-errors-bucket"),
			})
			return err
		}},
		{name: "wrong account", code: "AccessDenied", call: func() error {
			_, err := c.GetAccessPoint(ctx, &s3control.GetAccessPointInput{
				AccountId: new("111111111111"),
				Name:      new("control-errors"),
			})
			return err
		}},
		{name: "missing policy", code: "NoSuchAccessPointPolicy", call: func() error {
			_, err := c.GetAccessPointPolicy(ctx, &s3control.GetAccessPointPolicyInput{
				AccountId: account,
				Name:      new("control-errors"),
			})
			return err
		}},
		{name: "attached bucket", code: "BucketHasAccessPointsAttached", call: func() error {
			_, err := b.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: new("control-errors-bucket")})
			return err
		}},
		{name: "bad token", code: "InvalidToken", call: func() error {
			_, err := c.ListAccessPoints(ctx, &s3control.ListAccessPointsInput{AccountId: account, NextToken: new("!")})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var apiErr smithy.APIError
			if err := tc.call(); !errors.As(err, &apiErr) || apiErr.ErrorCode() != tc.code {
				t.Fatalf("want %s: %v", tc.code, err)
			}
		})
	}
}
