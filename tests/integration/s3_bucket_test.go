package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3DeleteObjects(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("batch")}); err != nil {
		t.Fatal(err)
	}

	var objects []s3types.ObjectIdentifier
	for i := range 3 {
		key := fmt.Sprintf("logs/%d.txt", i)
		putObject(t, c, "batch", key, "x", nil)
		objects = append(objects, s3types.ObjectIdentifier{Key: aws.String(key)})
	}

	// A key that does not exist is deleted too.
	objects = append(objects, s3types.ObjectIdentifier{Key: aws.String("missing")})
	out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("batch"), Delete: &s3types.Delete{Objects: objects},
	})
	if err != nil || len(out.Deleted) != 4 || len(out.Errors) != 0 {
		t.Fatalf("%+v %v", out, err)
	}

	listed, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("batch")})
	if err != nil || len(listed.Contents) != 0 {
		t.Fatalf("left: %+v %v", listed, err)
	}

	putObject(t, c, "batch", "kept", "x", nil)
	out, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("batch"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("kept")}}, Quiet: aws.Bool(true),
	}})
	if err != nil || len(out.Deleted) != 0 || len(out.Errors) != 0 {
		t.Fatalf("quiet: %+v %v", out, err)
	}

	// A versioned bucket hides each key behind a delete marker.
	if _, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("history")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String("history"),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})
	if err != nil {
		t.Fatal(err)
	}

	version := putVersion(t, c, "history", "a", "1")
	out, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("history"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("a")}},
	}})
	if err != nil || len(out.Deleted) != 1 || !aws.ToBool(out.Deleted[0].DeleteMarker) ||
		aws.ToString(out.Deleted[0].DeleteMarkerVersionId) == "" {
		t.Fatalf("marker: %+v %v", out, err)
	}

	marker := aws.ToString(out.Deleted[0].DeleteMarkerVersionId)
	out, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("history"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{
			{Key: aws.String("a"), VersionId: aws.String(marker)},
			{Key: aws.String("a"), VersionId: aws.String(version)},
		},
	}})
	if err != nil || len(out.Deleted) != 2 || !aws.ToBool(out.Deleted[0].DeleteMarker) ||
		aws.ToString(out.Deleted[1].VersionId) != version {
		t.Fatalf("versions: %+v %v", out, err)
	}

	if _, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("history")}); err != nil {
		t.Fatalf("the bucket still has versions: %v", err)
	}

	_, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("batch"), Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{}},
	})
	if code, status := s3Code(err); code != "MalformedXML" || status != 400 {
		t.Fatalf("no objects: %s %d %v", code, status, err)
	}

	_, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("nothing"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("a")}},
	}})
	if code, _ := s3Code(err); code != "NoSuchBucket" {
		t.Fatalf("missing bucket: %v", err)
	}
}

func TestS3GetBucketLocation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	east := s3Client(s, "test", "us-east-1")
	if _, err := east.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("east")}); err != nil {
		t.Fatal(err)
	}

	out, err := east.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("east")})
	if err != nil || out.LocationConstraint != "" {
		t.Fatalf("us-east-1: %+v %v", out, err)
	}

	west := s3Client(s, "test", "eu-west-1")
	_, err = west.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("west"),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraintEuWest1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err = west.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("west")})
	if err != nil || out.LocationConstraint != s3types.BucketLocationConstraintEuWest1 {
		t.Fatalf("eu-west-1: %+v %v", out, err)
	}

	// Any Region answers.
	out, err = east.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("west")})
	if err != nil || out.LocationConstraint != s3types.BucketLocationConstraintEuWest1 {
		t.Fatalf("from us-east-1: %+v %v", out, err)
	}

	_, err = s3Client(s, "team-b", "us-east-1").GetBucketLocation(ctx, &s3.GetBucketLocationInput{
		Bucket: aws.String("east"),
	})
	if code, status := s3Code(err); code != "AccessDenied" || status != 403 {
		t.Fatalf("another account: %s %d %v", code, status, err)
	}

	_, err = east.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("nothing")})
	if code, _ := s3Code(err); code != "NoSuchBucket" {
		t.Fatalf("missing bucket: %v", err)
	}
}
