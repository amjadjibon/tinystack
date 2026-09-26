package integration_test

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 ETags are MD5 digests.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func s3Client(s *httptest.Server, key, region string) *s3.Client {
	return s3.New(
		s3.Options{
			Region:       region,
			BaseEndpoint: aws.String(s.URL),
			UsePathStyle: true,
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
			}),
			Retryer: aws.NopRetryer{},
		},
	)
}

// s3Code returns the error code and HTTP status of an S3 error.
func s3Code(err error) (code string, status int) {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code = apiErr.ErrorCode()
	}

	var httpErr *awshttp.ResponseError
	if errors.As(err, &httpErr) {
		status = httpErr.HTTPStatusCode()
	}

	return code, status
}

func TestS3Buckets(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	for range 2 { // us-east-1 keeps succeeding for the owner.
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("Bad_Name")}); err == nil {
		t.Fatal("invalid bucket name accepted")
	} else if code, status := s3Code(err); code != "InvalidBucketName" || status != 400 {
		t.Fatal(code, status)
	}

	// Bucket names are unique across accounts and regions.
	other := s3Client(s, "team-b", "us-east-1")
	var taken *s3types.BucketAlreadyExists
	if _, err := other.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")}); !errors.As(
		err,
		&taken,
	) {
		t.Fatalf("another account took the name: %v", err)
	}

	if _, err := other.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("photos")}); err == nil {
		t.Fatal("another account reached the bucket")
	} else if _, status := s3Code(err); status != http.StatusForbidden {
		t.Fatal(err)
	}

	eu := s3Client(s, "test", "eu-west-1")
	if _, err := eu.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("eu-bucket")}); err == nil {
		t.Fatal("regional bucket created without a location constraint")
	} else if code, _ := s3Code(err); code != "IllegalLocationConstraintException" {
		t.Fatal(err)
	}

	if _, err := eu.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("eu-bucket"),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraintEuWest1,
		},
	}); err != nil {
		t.Fatal(err)
	}

	var owned *s3types.BucketAlreadyOwnedByYou
	if _, err := eu.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("photos"),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraintEuWest1,
		},
	}); !errors.As(err, &owned) {
		t.Fatalf("name reused in another region: %v", err)
	}

	// A bucket must be addressed through its own region.
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("eu-bucket")}); err == nil {
		t.Fatal("bucket reached from another region")
	} else if _, status := s3Code(err); status != http.StatusMovedPermanently {
		t.Fatal(err)
	}

	head, err := eu.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("eu-bucket")})
	if err != nil || aws.ToString(head.BucketRegion) != "eu-west-1" {
		t.Fatal(head, err)
	}

	// Listing spans every region of the account.
	listed, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(listed.Buckets) != 2 || *listed.Buckets[0].Name != "eu-bucket" ||
		*listed.Buckets[0].BucketRegion != "eu-west-1" || listed.Buckets[1].CreationDate == nil ||
		listed.Owner == nil || listed.Owner.ID == nil {
		t.Fatal(listed, err)
	}

	if theirs, listErr := other.ListBuckets(ctx, &s3.ListBucketsInput{}); listErr != nil || len(theirs.Buckets) != 0 {
		t.Fatal(theirs, listErr)
	}

	if _, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("photos"), Key: aws.String("x"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("photos")}); err == nil {
		t.Fatal("non-empty bucket deleted")
	} else if code, status := s3Code(err); code != "BucketNotEmpty" || status != http.StatusConflict {
		t.Fatal(code, status)
	}

	if _, err = c.DeleteObject(
		ctx,
		&s3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("x")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatal(err)
	}

	var notFound *s3types.NotFound
	if _, err = c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("photos")}); !errors.As(err, &notFound) {
		t.Fatalf("deleted bucket still exists: %v", err)
	}

	// The released name can be taken by another account.
	if _, err = other.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("photos")}); err != nil {
		t.Fatal(err)
	}
}

func TestS3Objects(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	bucket := aws.String("docs")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	sum := md5.Sum([]byte("hello")) // #nosec G401 -- S3 ETag.
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	put, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            bucket,
		Key:               aws.String("dir/a b.txt"),
		Body:              strings.NewReader("hello"),
		ContentType:       aws.String("text/plain"),
		Metadata:          map[string]string{"owner": "ada"},
		ChecksumAlgorithm: s3types.ChecksumAlgorithmCrc32,
	})
	if err != nil || aws.ToString(put.ETag) != etag {
		t.Fatal(put, err)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("dir/a b.txt")})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if err != nil || string(body) != "hello" || aws.ToString(got.ContentType) != "text/plain" ||
		got.Metadata["owner"] != "ada" || aws.ToString(got.ETag) != etag || got.LastModified == nil ||
		aws.ToInt64(got.ContentLength) != 5 {
		t.Fatalf("%q %+v %v", body, got, err)
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("dir/a b.txt")})
	if err != nil || aws.ToInt64(head.ContentLength) != 5 || aws.ToString(head.ETag) != etag {
		t.Fatal(head, err)
	}

	for _, tc := range []struct {
		in     *s3.GetObjectInput
		code   string
		status int
	}{
		{&s3.GetObjectInput{Key: aws.String("missing")}, "NoSuchKey", 404},
		{&s3.GetObjectInput{Key: aws.String("dir/a b.txt"), IfMatch: aws.String(`"nope"`)}, "PreconditionFailed", 412},
		{&s3.GetObjectInput{Key: aws.String("dir/a b.txt"), IfNoneMatch: &etag}, "NotModified", 304},
		{&s3.GetObjectInput{Key: aws.String("dir/a b.txt"), VersionId: aws.String("v1")}, "NoSuchVersion", 404},
	} {
		tc.in.Bucket = bucket
		_, getErr := c.GetObject(ctx, tc.in)
		if code, status := s3Code(getErr); status != tc.status || (tc.status != 304 && code != tc.code) {
			t.Fatalf("%s: got %s %d (%v)", tc.code, code, status, getErr)
		}
	}

	// Conditional writes and digests.
	if _, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("dir/a b.txt"), Body: strings.NewReader("new"), IfNoneMatch: aws.String("*"),
	}); err == nil {
		t.Fatal("If-None-Match * overwrote an object")
	} else if _, status := s3Code(err); status != http.StatusPreconditionFailed {
		t.Fatal(err)
	}

	if _, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("bad"), Body: strings.NewReader("data"),
		ContentMD5: aws.String("AAAAAAAAAAAAAAAAAAAAAA=="),
	}); err == nil {
		t.Fatal("wrong Content-MD5 accepted")
	} else if code, _ := s3Code(err); code != "BadDigest" {
		t.Fatal(err)
	}

	if _, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("x"), Body: strings.NewReader("x"), StorageClass: s3types.StorageClassGlacier,
	}); err == nil {
		t.Fatal("unsupported storage class accepted")
	}

	if _, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("x"), Body: strings.NewReader("x"), ACL: s3types.ObjectCannedACLPublicRead,
	}); err == nil {
		t.Fatal("unsupported ACL header accepted")
	} else if code, _ := s3Code(err); code != "NotImplemented" {
		t.Fatal(err)
	}

	if _, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String("dir/a b.txt")}); err != nil {
		t.Fatal(err)
	}

	var missing *s3types.NoSuchKey
	if _, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("dir/a b.txt")}); !errors.As(
		err,
		&missing,
	) {
		t.Fatalf("deleted object still readable: %v", err)
	}

	var noBucket *s3types.NoSuchBucket
	if _, err = c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("nope")}); !errors.As(err, &noBucket) {
		t.Fatalf("listing a missing bucket: %v", err)
	}
}

func TestS3ListObjectsV2(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	bucket := aws.String("tree")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	keys := []string{"a.txt", "dir/a.txt", "dir/b.txt", "dir/sub/c.txt", "z.txt"}
	for _, key := range keys {
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: bucket, Key: aws.String(key), Body: bytes.NewReader([]byte(key)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	names := func(out *s3.ListObjectsV2Output) string {
		var parts []string
		for _, o := range out.Contents {
			parts = append(parts, *o.Key)
		}

		for _, p := range out.CommonPrefixes {
			parts = append(parts, *p.Prefix)
		}

		return strings.Join(parts, ",")
	}

	top, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, Delimiter: aws.String("/")})
	if err != nil || names(top) != "a.txt,z.txt,dir/" || aws.ToInt32(top.KeyCount) != 3 {
		t.Fatal(names(top), err)
	}

	dir, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: bucket, Prefix: aws.String("dir/"), Delimiter: aws.String("/"),
	})
	if err != nil || names(dir) != "dir/a.txt,dir/b.txt,dir/sub/" {
		t.Fatal(names(dir), err)
	}

	// Pages of two, with a common prefix counting as one entry.
	var all []string
	pages := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{
		Bucket: bucket, MaxKeys: aws.Int32(2), Delimiter: aws.String("/"),
	})
	for pages.HasMorePages() {
		page, pageErr := pages.NextPage(ctx)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		all = append(all, names(page))
	}

	if fmt.Sprint(all) != "[a.txt,dir/ z.txt]" {
		t.Fatal(all)
	}

	after, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, StartAfter: aws.String("dir/b.txt")})
	if err != nil || names(after) != "dir/sub/c.txt,z.txt" {
		t.Fatal(names(after), err)
	}
}

func TestS3RESTAddressing(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("site")}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("site"), Key: aws.String("index.html"), Body: strings.NewReader("<h1>hi</h1>"),
	}); err != nil {
		t.Fatal(err)
	}

	do := func(method, path, host string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, s.URL+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		if host != "" {
			req.Host = host
		}

		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Virtual-hosted addressing names the bucket in the host.
	if status, body := do(http.MethodGet, "/index.html", "site.localhost"); status != 200 || body != "<h1>hi</h1>" {
		t.Fatal(status, body)
	}

	if status, body := do(http.MethodGet, "/site/index.html", ""); status != 200 || body != "<h1>hi</h1>" {
		t.Fatal(status, body)
	}

	// Subresources that are not implemented fail instead of reaching another operation.
	if status, body := do(http.MethodGet, "/site?acl", ""); status != http.StatusNotImplemented ||
		!strings.Contains(body, "<Code>NotImplemented</Code>") {
		t.Fatal(status, body)
	}

	// Streaming uploads use aws-chunked with a trailing checksum that is verified.
	put := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, s.URL+"/site/chunked.txt", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	chunks := "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:%s\r\n\r\n"
	if status, body := put(fmt.Sprintf(chunks, "DUoRhQ==")); status != 200 {
		t.Fatal(status, body)
	}

	if status, body := do(http.MethodGet, "/site/chunked.txt", ""); status != 200 || body != "hello world" {
		t.Fatal(status, body)
	}

	if status, body := put(fmt.Sprintf(chunks, "AAAAAA==")); status != 400 ||
		!strings.Contains(body, "<Code>BadDigest</Code>") {
		t.Fatal(status, body)
	}

	if status, body := do(http.MethodGet, "/site?list-type=2&prefix=index", ""); status != 200 ||
		!strings.Contains(body, "<Key>index.html</Key>") ||
		!strings.Contains(body, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`) {
		t.Fatal(status, body)
	}
}
