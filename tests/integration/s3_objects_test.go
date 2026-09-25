package integration_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func putObject(t *testing.T, c *s3.Client, bucket, key, body string, in *s3.PutObjectInput) {
	t.Helper()
	if in == nil {
		in = &s3.PutObjectInput{}
	}

	in.Bucket, in.Key, in.Body = aws.String(bucket), aws.String(key), strings.NewReader(body)
	if _, err := c.PutObject(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func objectTags(t *testing.T, c *s3.Client, bucket, key string) map[string]string {
	t.Helper()
	out, err := c.GetObjectTagging(context.Background(), &s3.GetObjectTaggingInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatal(err)
	}

	tags := map[string]string{}
	for _, tag := range out.TagSet {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}

	return tags
}

func TestS3ObjectTagging(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("docs")}); err != nil {
		t.Fatal(err)
	}

	putObject(t, c, "docs", "a.txt", "a", &s3.PutObjectInput{Tagging: aws.String("team=core&stage=dev%20one")})
	if tags := objectTags(t, c, "docs", "a.txt"); len(tags) != 2 || tags["stage"] != "dev one" {
		t.Fatal(tags)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("docs"), Key: aws.String("a.txt")})
	if err != nil || aws.ToInt32(got.TagCount) != 2 {
		t.Fatalf("%+v %v", got, err)
	}

	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket:  aws.String("docs"),
		Key:     aws.String("a.txt"),
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("owner"), Value: aws.String("ops")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if tags := objectTags(t, c, "docs", "a.txt"); len(tags) != 1 || tags["owner"] != "ops" {
		t.Fatal(tags)
	}

	if _, err = c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
		Bucket: aws.String("docs"),
		Key:    aws.String("a.txt"),
	}); err != nil {
		t.Fatal(err)
	}

	if tags := objectTags(t, c, "docs", "a.txt"); len(tags) != 0 {
		t.Fatal(tags)
	}

	many := make([]s3types.Tag, 11)
	for i := range many {
		many[i] = s3types.Tag{Key: aws.String(string(rune('a' + i))), Value: aws.String("v")}
	}

	for _, set := range [][]s3types.Tag{
		many,
		{{Key: aws.String("aws:owner"), Value: aws.String("x")}},
		{{Key: aws.String("k"), Value: aws.String("1")}, {Key: aws.String("k"), Value: aws.String("2")}},
	} {
		_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
			Bucket:  aws.String("docs"),
			Key:     aws.String("a.txt"),
			Tagging: &s3types.Tagging{TagSet: set},
		})
		if _, status := s3Code(err); status != http.StatusBadRequest {
			t.Fatalf("%d tags: %v", len(set), err)
		}
	}

	_, err = c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String("docs"), Key: aws.String("nope")})
	if code, _ := s3Code(err); code != "NoSuchKey" {
		t.Fatalf("missing object: %v", err)
	}
}

func TestS3CopyObject(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	for _, bucket := range []string{"src", "dst"} {
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatal(err)
		}
	}

	putObject(t, c, "src", "dir/report one.txt", "hello", &s3.PutObjectInput{
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"owner": "core"},
		Tagging:     aws.String("team=core"),
	})

	source := "src/" + url.PathEscape("dir/report one.txt")
	copied, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String("dst"),
		Key:        aws.String("copy.txt"),
		CopySource: aws.String(source),
	})
	if err != nil || aws.ToString(copied.CopyObjectResult.ETag) == "" {
		t.Fatalf("%+v %v", copied, err)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("dst"), Key: aws.String("copy.txt")})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(got.Body)
	if string(body) != "hello" || aws.ToString(got.ContentType) != "text/plain" || got.Metadata["owner"] != "core" ||
		aws.ToString(got.ETag) != aws.ToString(copied.CopyObjectResult.ETag) {
		t.Fatalf("%s %+v", body, got)
	}

	if tags := objectTags(t, c, "dst", "copy.txt"); tags["team"] != "core" {
		t.Fatal(tags)
	}

	// REPLACE directives take the request's metadata and tags.
	_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String("src"),
		Key:               aws.String("dir/report one.txt"),
		CopySource:        aws.String(source),
		MetadataDirective: s3types.MetadataDirectiveReplace,
		ContentType:       aws.String("text/markdown"),
		Metadata:          map[string]string{"owner": "ops"},
		TaggingDirective:  s3types.TaggingDirectiveReplace,
		Tagging:           aws.String("team=ops"),
	})
	if err != nil {
		t.Fatal(err)
	}

	head, err := c.HeadObject(
		ctx,
		&s3.HeadObjectInput{Bucket: aws.String("src"), Key: aws.String("dir/report one.txt")},
	)
	if err != nil || aws.ToString(head.ContentType) != "text/markdown" || head.Metadata["owner"] != "ops" {
		t.Fatalf("%+v %v", head, err)
	}

	if tags := objectTags(t, c, "src", "dir/report one.txt"); tags["team"] != "ops" {
		t.Fatal(tags)
	}

	for _, tc := range []struct {
		bucket, key, source string
		ifMatch             *string
		code                string
		status              int
	}{
		// Copying an object onto itself must change something.
		{"src", "dir/report one.txt", source, nil, "InvalidRequest", http.StatusBadRequest},
		{"dst", "x", "src/missing", nil, "NoSuchKey", http.StatusNotFound},
		{"dst", "x", "nobucket/k", nil, "NoSuchBucket", http.StatusNotFound},
		{"dst", "x", source, aws.String(`"stale"`), "PreconditionFailed", http.StatusPreconditionFailed},
		{"dst", "x", source + "?versionId=1", nil, "NotImplemented", http.StatusNotImplemented},
	} {
		_, err = c.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:            aws.String(tc.bucket),
			Key:               aws.String(tc.key),
			CopySource:        aws.String(tc.source),
			CopySourceIfMatch: tc.ifMatch,
		})
		if code, status := s3Code(err); code != tc.code || status != tc.status {
			t.Fatalf("%s: %v", tc.source, err)
		}
	}

	// A plain PUT to the same path is still PutObject.
	putObject(t, c, "dst", "copy.txt", "replaced", nil)
}

func TestS3PresignedURLs(t *testing.T) {
	s := verifyingServer(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("shared")}); err != nil {
		t.Fatal(err)
	}

	presigner := s3.NewPresignClient(c)
	upload, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("shared"),
		Key:    aws.String("docs/hello world.txt"),
	}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(ctx, upload.Method, upload.URL, bytes.NewReader([]byte("hi there")))
	if err != nil {
		t.Fatal(err)
	}

	for k, vs := range upload.SignedHeader {
		req.Header[k] = vs
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT: %d", resp.StatusCode)
	}

	download, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("shared"),
		Key:    aws.String("docs/hello world.txt"),
	}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	get := func(rawURL string) (int, string) {
		t.Helper()
		r, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		res, doErr := http.DefaultClient.Do(r)
		if doErr != nil {
			t.Fatal(doErr)
		}

		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	if status, body := get(download.URL); status != http.StatusOK || body != "hi there" {
		t.Fatalf("presigned GET: %d %s", status, body)
	}

	// A tampered signature, a changed key, or an expired URL is refused.
	tampered := strings.Replace(download.URL, "X-Amz-Signature=", "X-Amz-Signature=0", 1)
	if status, body := get(
		tampered,
	); status != http.StatusForbidden ||
		!strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("tampered: %d %s", status, body)
	}

	moved := strings.Replace(download.URL, "hello%20world", "other", 1)
	if status, _ := get(moved); status != http.StatusForbidden {
		t.Fatalf("changed key: %d", status)
	}

	expired, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("shared"),
		Key:    aws.String("docs/hello world.txt"),
	}, func(o *s3.PresignOptions) {
		o.Expires = time.Second
		o.ClientOptions = append(o.ClientOptions, func(opts *s3.Options) {
			opts.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)
	if status, body := get(
		expired.URL,
	); status != http.StatusForbidden ||
		!strings.Contains(body, "Request has expired") {
		t.Fatalf("expired: %d %s", status, body)
	}

	// An unknown key cannot presign.
	stranger, err := s3.NewPresignClient(s3Client(s, "stranger", "us-east-1")).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("shared"),
		Key:    aws.String("docs/hello world.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if status, body := get(
		stranger.URL,
	); status != http.StatusForbidden ||
		!strings.Contains(body, "InvalidAccessKeyId") {
		t.Fatalf("stranger: %d %s", status, body)
	}
}
