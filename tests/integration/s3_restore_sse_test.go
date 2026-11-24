package integration_test

import (
	"context"
	"crypto/md5"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3RestoreArchive(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Storage.S3LifecycleDay = time.Second
	c, bucket := checksumBucket(t, serverConfig(t, cfg, &fakeRuntime{}))
	putObject(t, c, *bucket, "archive", "cold data", nil)
	setS3Lifecycle(t, c, bucket, s3types.LifecycleRule{
		Status: s3types.ExpirationStatusEnabled,
		Filter: &s3types.LifecycleRuleFilter{ObjectSizeGreaterThan: new(int64(0))},
		Transitions: []s3types.Transition{{
			Days: new(int32(0)), StorageClass: s3types.TransitionStorageClassDeepArchive,
		}},
	})

	head := &s3.HeadObjectInput{Bucket: bucket, Key: new("archive")}
	waitS3(t, "archive transition", func() bool {
		headOut, headErr := c.HeadObject(ctx, head)
		return headErr == nil && headOut.StorageClass == s3types.StorageClassDeepArchive
	})

	get := &s3.GetObjectInput{Bucket: bucket, Key: head.Key}
	if _, err := c.GetObject(ctx, get); err == nil {
		t.Fatal("archive could be read without restore")
	} else if code, _ := s3Code(err); code != "InvalidObjectState" {
		t.Fatal(err)
	}

	in := &s3.RestoreObjectInput{
		Bucket: bucket, Key: head.Key,
		RestoreRequest: &s3types.RestoreRequest{Days: new(int32(4)), Tier: s3types.TierStandard},
	}
	if _, err := c.RestoreObject(ctx, in); err != nil {
		t.Fatal(err)
	}

	out, err := c.HeadObject(ctx, head)
	if err != nil || out.Restore == nil || *out.Restore != `ongoing-request="true"` {
		t.Fatalf("pending restore: %+v %v", out, err)
	}

	if _, restoreErr := c.RestoreObject(ctx, in); restoreErr == nil {
		t.Fatal("duplicate restore was accepted while in progress")
	} else if code, status := s3Code(restoreErr); code != "RestoreAlreadyInProgress" || status != http.StatusConflict {
		t.Fatal(restoreErr)
	}

	waitS3(t, "restore completion", func() bool {
		headOut, headErr := c.HeadObject(ctx, head)
		return headErr == nil && headOut.Restore != nil && strings.Contains(*headOut.Restore, `ongoing-request="false"`)
	})
	read, err := c.GetObject(ctx, get)
	if err != nil {
		t.Fatal(err)
	}

	defer read.Body.Close()
	data, err := io.ReadAll(read.Body)
	if err != nil || string(data) != "cold data" {
		t.Fatalf("restored body: %q %v", data, err)
	}

	waitS3(t, "restored copy expiry", func() bool {
		live, getErr := c.GetObject(ctx, get)
		if getErr == nil {
			_ = live.Body.Close()
			return false
		}

		code, _ := s3Code(getErr)
		return code == "InvalidObjectState"
	})
}

func TestS3CustomerKey(t *testing.T) {
	ctx := context.Background()
	c, bucket := checksumBucket(t, verifyingServer(t))
	newKey := func(value string) (key, keyMD5 *string) {
		raw := []byte(strings.Repeat(value, 32))
		return new(b64(raw)), new(b64(digest(md5.New(), raw)))
	}
	key, md5Sum := newKey("a")
	otherKey, otherMD5 := newKey("b")
	put, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: new("sse"), Body: strings.NewReader("private"),
		SSECustomerAlgorithm: new("AES256"), SSECustomerKey: key, SSECustomerKeyMD5: md5Sum,
	})
	if err != nil || put.SSECustomerKeyMD5 == nil || *put.SSECustomerKeyMD5 != *md5Sum {
		t.Fatalf("put SSE-C: %+v %v", put, err)
	}

	get := &s3.GetObjectInput{Bucket: bucket, Key: new("sse")}
	if _, getErr := c.GetObject(ctx, get); getErr == nil {
		t.Fatal("read accepted without a customer key")
	}

	get.SSECustomerAlgorithm, get.SSECustomerKey, get.SSECustomerKeyMD5 = new("AES256"), otherKey, otherMD5
	if _, getErr := c.GetObject(ctx, get); getErr == nil {
		t.Fatal("read accepted the wrong key")
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: get.Key,
		SSECustomerAlgorithm: new("AES256"), SSECustomerKey: key, SSECustomerKeyMD5: md5Sum,
	})
	if err != nil || head.SSECustomerKeyMD5 == nil || *head.SSECustomerKeyMD5 != *md5Sum {
		t.Fatalf("head SSE-C: %+v %v", head, err)
	}

	copyRequest := &s3.CopyObjectInput{Bucket: bucket, Key: new("copied"), CopySource: new(*bucket + "/sse")}
	if _, copyErr := c.CopyObject(ctx, copyRequest); copyErr == nil {
		t.Fatal("copy accepted without a source key")
	}

	copyRequest.CopySourceSSECustomerAlgorithm, copyRequest.CopySourceSSECustomerKey = new("AES256"), key
	copyRequest.CopySourceSSECustomerKeyMD5 = md5Sum
	copyRequest.SSECustomerAlgorithm, copyRequest.SSECustomerKey, copyRequest.SSECustomerKeyMD5 = new(
		"AES256",
	), otherKey, otherMD5
	if _, copyErr := c.CopyObject(ctx, copyRequest); copyErr != nil {
		t.Fatal(copyErr)
	}

	get.Key = copyRequest.Key
	out, err := c.GetObject(ctx, get)
	if err != nil {
		t.Fatal(err)
	}

	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil || string(data) != "private" {
		t.Fatalf("copied SSE-C body: %q %v", data, err)
	}
}
