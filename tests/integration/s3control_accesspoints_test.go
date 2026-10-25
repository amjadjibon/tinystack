package integration_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	"github.com/aws/aws-sdk-go-v2/service/s3control/types"
)

func TestS3ControlAccessPoints(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	server, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	c := s3ControlClient(server)
	b := s3Client(server, "test", "us-east-1")
	account := new("000000000000")
	bucket := new("access-point-bucket")
	if _, err := b.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	point, err := c.CreateAccessPoint(
		ctx,
		&s3control.CreateAccessPointInput{
			AccountId: account,
			Name:      new("test-point"),
			Bucket:    bucket,
			Tags:      []types.Tag{{Key: new("team"), Value: new("test")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetAccessPoint(ctx, &s3control.GetAccessPointInput{AccountId: account, Name: new("test-point")})
	if err != nil || *got.Alias != *point.Alias || got.Endpoints["ipv4"] == "" {
		t.Fatalf("get: %+v %v", got, err)
	}

	if _, err = c.CreateAccessPoint(
		ctx,
		&s3control.CreateAccessPointInput{AccountId: account, Name: new("second-point"), Bucket: bucket},
	); err != nil {
		t.Fatal(err)
	}

	page, err := c.ListAccessPoints(
		ctx,
		&s3control.ListAccessPointsInput{AccountId: account, Bucket: bucket, MaxResults: 1},
	)
	if err != nil || len(page.AccessPointList) != 1 || page.NextToken == nil {
		t.Fatalf("page %+v %v", page, err)
	}

	page, err = c.ListAccessPoints(
		ctx,
		&s3control.ListAccessPointsInput{AccountId: account, Bucket: bucket, MaxResults: 1, NextToken: page.NextToken},
	)
	if err != nil || len(page.AccessPointList) != 1 || page.NextToken != nil {
		t.Fatalf("page 2 %+v %v", page, err)
	}

	if _, err = c.TagResource(
		ctx,
		&s3control.TagResourceInput{
			AccountId:   account,
			ResourceArn: point.AccessPointArn,
			Tags:        []types.Tag{{Key: new("extra"), Value: new("yes")}},
		},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(
		ctx,
		&s3control.ListTagsForResourceInput{AccountId: account, ResourceArn: point.AccessPointArn},
	)
	if err != nil || len(tags.Tags) != 2 {
		t.Fatalf("tags %+v %v", tags, err)
	}

	if _, err = c.UntagResource(
		ctx,
		&s3control.UntagResourceInput{
			AccountId:   account,
			ResourceArn: point.AccessPointArn,
			TagKeys:     []string{"extra"},
		},
	); err != nil {
		t.Fatal(err)
	}

	policy := new(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:root"}` +
			`,"Action":"s3:GetObject","Resource":"` + *point.AccessPointArn + `/object/*"}]}`,
	)
	if _, err = c.PutAccessPointPolicy(
		ctx,
		&s3control.PutAccessPointPolicyInput{AccountId: account, Name: new("test-point"), Policy: policy},
	); err != nil {
		t.Fatal(err)
	}

	pol, err := c.GetAccessPointPolicy(
		ctx,
		&s3control.GetAccessPointPolicyInput{AccountId: account, Name: new("test-point")},
	)
	if err != nil || *pol.Policy != *policy {
		t.Fatalf("policy %+v %v", pol, err)
	}

	status, err := c.GetAccessPointPolicyStatus(
		ctx,
		&s3control.GetAccessPointPolicyStatusInput{AccountId: account, Name: new("test-point")},
	)
	if err != nil || status.PolicyStatus.IsPublic {
		t.Fatalf("status %+v %v", status, err)
	}

	if _, err = b.PutObject(
		ctx,
		&s3.PutObjectInput{Bucket: point.Alias, Key: new("object"), Body: strings.NewReader("alias body")},
	); err != nil {
		t.Fatal(err)
	}

	stop()
	server, stop = persistent(t, dir, &fakeRuntime{})
	c = s3ControlClient(server)
	b = s3Client(server, "test", "us-east-1")
	obj, err := b.GetObject(ctx, &s3.GetObjectInput{Bucket: point.Alias, Key: new("object")})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(obj.Body)
	obj.Body.Close()
	if err != nil || string(body) != "alias body" {
		t.Fatalf("body %q %v", body, err)
	}

	if _, err = c.DeleteAccessPointPolicy(
		ctx,
		&s3control.DeleteAccessPointPolicyInput{AccountId: account, Name: new("test-point")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.GetAccessPointPolicy(
		ctx,
		&s3control.GetAccessPointPolicyInput{AccountId: account, Name: new("test-point")},
	); err == nil {
		t.Fatal("missing policy succeeded")
	}

	for _, name := range []string{"test-point", "second-point"} {
		if _, err = c.DeleteAccessPoint(
			ctx,
			&s3control.DeleteAccessPointInput{AccountId: account, Name: new(name)},
		); err != nil {
			t.Fatal(err)
		}
	}
}
