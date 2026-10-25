package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3control"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestS3ControlAliasAuthorization(t *testing.T) {
	ctx := context.Background()
	srv := policyServer(t, config.AuthorizationEnforce)
	b := s3Client(srv, "test", "us-east-1")
	c := s3ControlClient(srv)
	if _, err := b.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("control-authorization")}); err != nil {
		t.Fatal(err)
	}

	point, err := c.CreateAccessPoint(
		ctx,
		&s3control.CreateAccessPointInput{
			AccountId: new(policyAccount),
			Name:      new("authorization-point"),
			Bucket:    new("control-authorization"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = b.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: point.Alias, Key: new("object"), Body: strings.NewReader("content")},
	); err != nil {
		t.Fatal(err)
	}

	deny := new(
		`{"Statement":[{"Effect":"Deny","Principal":"*"` +
			`,"Action":"s3:GetObject","Resource":"` + *point.AccessPointArn + `/object/*"}]}`,
	)
	if _, err = c.PutAccessPointPolicy(
		ctx,
		&s3control.PutAccessPointPolicyInput{
			AccountId: new(policyAccount),
			Name:      new("authorization-point"),
			Policy:    deny,
		},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = b.GetObject(ctx, &s3.GetObjectInput{Bucket: point.Alias, Key: new("object")}); err == nil {
		t.Fatal("access point deny was not enforced")
	}

	obj, err := b.GetObject(ctx, &s3.GetObjectInput{Bucket: new("control-authorization"), Key: new("object")})
	if err != nil {
		t.Fatal(err)
	}

	obj.Body.Close()
}
