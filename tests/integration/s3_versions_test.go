package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// versioned creates a bucket and sets its versioning status.
func versioned(t *testing.T, c *s3.Client, bucket string, status s3types.BucketVersioningStatus) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}

	_, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket), VersioningConfiguration: &s3types.VersioningConfiguration{Status: status},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// putVersion writes an object and returns the version ID S3 reported.
func putVersion(t *testing.T, c *s3.Client, bucket, key, body string) string {
	t.Helper()
	out, err := c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.VersionId)
}

// readVersion returns the body and version ID of an object, or of one version
// when version is not empty.
func readVersion(t *testing.T, c *s3.Client, bucket, key, version string) (body, got string) {
	t.Helper()
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if version != "" {
		in.VersionId = aws.String(version)
	}

	out, err := c.GetObject(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(data), aws.ToString(out.VersionId)
}

// listing walks every ListObjectVersions page and returns the versions, then
// the delete markers (the SDK keeps them in separate lists), as "key@version"
// entries in listing order. The latest version is marked with a leading "*",
// and delete markers with "x".
func listing(t *testing.T, c *s3.Client, in *s3.ListObjectVersionsInput) []string {
	t.Helper()
	var versions, markers []string
	entry := func(prefix string, latest *bool, key, version *string) string {
		if aws.ToBool(latest) {
			prefix = "*" + prefix
		}

		return prefix + aws.ToString(key) + "@" + aws.ToString(version)
	}

	for {
		page, err := c.ListObjectVersions(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}

		for i := range page.Versions {
			v := &page.Versions[i]
			versions = append(versions, entry("", v.IsLatest, v.Key, v.VersionId))
		}

		for _, m := range page.DeleteMarkers {
			markers = append(markers, entry("x", m.IsLatest, m.Key, m.VersionId))
		}

		if !aws.ToBool(page.IsTruncated) {
			return append(versions, markers...)
		}

		in.KeyMarker, in.VersionIdMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
}

func TestS3Versioning(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("docs")}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("docs")})
	if err != nil || got.Status != "" {
		t.Fatalf("got %+v, %v", got, err)
	}

	// Until versioning is enabled, objects are null versions and responses
	// carry no version ID.
	if v := putVersion(t, c, "docs", "a.txt", "zero"); v != "" {
		t.Fatalf("unversioned write reported version %q", v)
	}

	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String("docs"), VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err = c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("docs")})
	if err != nil || got.Status != s3types.BucketVersioningStatusEnabled {
		t.Fatalf("got %+v, %v", got, err)
	}

	v1 := putVersion(t, c, "docs", "a.txt", "one")
	v2 := putVersion(t, c, "docs", "a.txt", "two")
	if v1 == "" || v2 == "" || v1 == v2 || v1 == "null" {
		t.Fatalf("versions %q and %q", v1, v2)
	}

	for version, want := range map[string]string{"": "two", v1: "one", v2: "two", "null": "zero"} {
		body, reported := readVersion(t, c, "docs", "a.txt", version)
		if body != want || version != "" && reported != version || version == "" && reported != v2 {
			t.Fatalf("version %q: got %q as %q", version, body, reported)
		}
	}

	_, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a.txt"), VersionId: aws.String("missing"),
	})
	if code, status := s3Code(err); code != "NoSuchVersion" || status != http.StatusNotFound {
		t.Fatalf("missing version: %v", err)
	}

	// Deleting without a version adds a delete marker that hides the object.
	deleted, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("docs"), Key: aws.String("a.txt")})
	if err != nil || !aws.ToBool(deleted.DeleteMarker) || aws.ToString(deleted.VersionId) == "" {
		t.Fatalf("got %+v, %v", deleted, err)
	}

	marker := aws.ToString(deleted.VersionId)
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("docs"), Key: aws.String("a.txt")})
	if code, status := s3Code(err); code != "NoSuchKey" || status != http.StatusNotFound {
		t.Fatalf("hidden object: %v", err)
	}

	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a.txt"), VersionId: aws.String(marker),
	})
	if _, status := s3Code(err); status != http.StatusMethodNotAllowed {
		t.Fatalf("delete marker: %v", err)
	}

	objects, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("docs")})
	if err != nil || len(objects.Contents) != 0 {
		t.Fatalf("got %+v, %v", objects, err)
	}

	want := []string{"a.txt@" + v2, "a.txt@" + v1, "a.txt@null", "*xa.txt@" + marker}
	if entries := listing(t, c, &s3.ListObjectVersionsInput{Bucket: aws.String("docs")}); !equal(entries, want) {
		t.Fatalf("got %v, want %v", entries, want)
	}

	// One version per page walks the same listing.
	entries := listing(t, c, &s3.ListObjectVersionsInput{Bucket: aws.String("docs"), MaxKeys: aws.Int32(1)})
	if !equal(entries, want) {
		t.Fatalf("paged: got %v, want %v", entries, want)
	}

	// Removing the marker makes the newest version current again; removing
	// that version makes the one before it current.
	for _, version := range []string{marker, v2} {
		out, deleteErr := c.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String("docs"), Key: aws.String("a.txt"), VersionId: aws.String(version),
		})
		if deleteErr != nil || aws.ToString(out.VersionId) != version ||
			aws.ToBool(out.DeleteMarker) != (version == marker) {
			t.Fatalf("delete %s: got %+v, %v", version, out, deleteErr)
		}
	}

	if body, reported := readVersion(t, c, "docs", "a.txt", ""); body != "one" || reported != v1 {
		t.Fatalf("got %q as %q", body, reported)
	}

	want = []string{"*a.txt@" + v1, "a.txt@null"}
	if entries = listing(t, c, &s3.ListObjectVersionsInput{Bucket: aws.String("docs")}); !equal(entries, want) {
		t.Fatalf("got %v, want %v", entries, want)
	}

	// Tags belong to a version.
	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String("docs"), Key: aws.String("a.txt"), VersionId: aws.String("null"),
		Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("era"), Value: aws.String("old")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tagged, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String("docs"), Key: aws.String("a.txt"), VersionId: aws.String("null"),
	})
	if err != nil || len(tagged.TagSet) != 1 || aws.ToString(tagged.VersionId) != "null" {
		t.Fatalf("got %+v, %v", tagged, err)
	}

	if tags := objectTags(t, c, "docs", "a.txt"); len(tags) != 0 {
		t.Fatalf("current version tags %v", tags)
	}

	// A copy can name its source version.
	copied, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("b.txt"),
		CopySource: aws.String("docs/" + url.PathEscape("a.txt") + "?versionId=null"),
	})
	if err != nil || aws.ToString(copied.CopySourceVersionId) != "null" || aws.ToString(copied.VersionId) == "" {
		t.Fatalf("got %+v, %v", copied, err)
	}

	if body, _ := readVersion(t, c, "docs", "b.txt", ""); body != "zero" {
		t.Fatalf("copied %q", body)
	}

	// A bucket with versions is not empty until every version is deleted.
	for _, key := range []string{"a.txt", "b.txt"} {
		if _, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String("docs"), Key: aws.String(key),
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("docs")})
	if code, _ := s3Code(err); code != "BucketNotEmpty" {
		t.Fatalf("got %v", err)
	}

	for _, entry := range listing(t, c, &s3.ListObjectVersionsInput{Bucket: aws.String("docs")}) {
		key, version, _ := strings.Cut(strings.TrimLeft(entry, "*x"), "@")
		if _, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String("docs"), Key: aws.String(key), VersionId: aws.String(version),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("docs")}); err != nil {
		t.Fatal(err)
	}
}

func TestS3VersioningSuspended(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	versioned(t, c, "logs", s3types.BucketVersioningStatusEnabled)
	v1 := putVersion(t, c, "logs", "a", "one")

	_, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String("logs"), VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusSuspended,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// While suspended, writes and deletes replace the null version and keep
	// the others.
	for _, body := range []string{"two", "three"} {
		if v := putVersion(t, c, "logs", "a", body); v != "null" {
			t.Fatalf("suspended write reported version %q", v)
		}
	}

	want := []string{"*a@null", "a@" + v1}
	if entries := listing(t, c, &s3.ListObjectVersionsInput{Bucket: aws.String("logs")}); !equal(entries, want) {
		t.Fatalf("got %v, want %v", entries, want)
	}

	deleted, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("logs"), Key: aws.String("a")})
	if err != nil || !aws.ToBool(deleted.DeleteMarker) || aws.ToString(deleted.VersionId) != "null" {
		t.Fatalf("got %+v, %v", deleted, err)
	}

	want = []string{"a@" + v1, "*xa@null"}
	if entries := listing(t, c, &s3.ListObjectVersionsInput{Bucket: aws.String("logs")}); !equal(entries, want) {
		t.Fatalf("got %v, want %v", entries, want)
	}

	// Versioning cannot be turned off, and MFA delete is not supported.
	for _, cfg := range []*s3types.VersioningConfiguration{
		{}, {Status: s3types.BucketVersioningStatusEnabled, MFADelete: s3types.MFADeleteEnabled},
	} {
		_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
			Bucket: aws.String("logs"), VersioningConfiguration: cfg,
		})
		if err == nil {
			t.Fatalf("%+v accepted", cfg)
		}
	}
}

func TestS3ListObjectVersionsPrefixes(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("site")}); err != nil {
		t.Fatal(err)
	}

	// An unversioned bucket lists its objects as null versions.
	for _, key := range []string{"a/1", "a/2", "b", "c/1"} {
		putObject(t, c, "site", key, key, nil)
	}

	out, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: aws.String("site"), Delimiter: aws.String("/"),
	})
	if err != nil || len(out.CommonPrefixes) != 2 || len(out.Versions) != 1 ||
		aws.ToString(out.Versions[0].VersionId) != "null" || !aws.ToBool(out.Versions[0].IsLatest) {
		t.Fatalf("got %+v, %v", out, err)
	}

	want := []string{"*a/1@null", "*a/2@null"}
	entries := listing(t, c, &s3.ListObjectVersionsInput{
		Bucket: aws.String("site"), Prefix: aws.String("a/"), MaxKeys: aws.Int32(1),
	})
	if !equal(entries, want) {
		t.Fatalf("got %v, want %v", entries, want)
	}
}

func TestS3VersionNotifications(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	queues := sqsClient(s, "test")
	ctx := context.Background()
	queueURL, arn := queueARN(t, queues, "changes")
	versioned(t, c, "docs", s3types.BucketVersioningStatusEnabled)
	_, err := c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
		Bucket: aws.String("docs"), NotificationConfiguration: &s3types.NotificationConfiguration{
			QueueConfigurations: []s3types.QueueConfiguration{{
				QueueArn: aws.String(arn), Events: []s3types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if body := receiveOne(t, queues, queueURL); !strings.Contains(body, `"s3:TestEvent"`) {
		t.Fatalf("test event %q", body)
	}

	version := putVersion(t, c, "docs", "a", "one")
	if r := decodeRecord(t, receiveOne(t, queues, queueURL)); r.S3.Object.VersionID != version {
		t.Fatalf("created %+v", r)
	}

	deleted, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("docs"), Key: aws.String("a")})
	if err != nil {
		t.Fatal(err)
	}

	r := decodeRecord(t, receiveOne(t, queues, queueURL))
	if r.EventName != "ObjectRemoved:DeleteMarkerCreated" || r.S3.Object.VersionID != aws.ToString(deleted.VersionId) {
		t.Fatalf("deleted %+v", r)
	}

	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a"), VersionId: aws.String(version),
	})
	if err != nil {
		t.Fatal(err)
	}

	if r = decodeRecord(t, receiveOne(t, queues, queueURL)); r.EventName != "ObjectRemoved:Delete" {
		t.Fatalf("deleted version %+v", r)
	}

	// Deleting a version that does not exist removes nothing and is not notified.
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a"), VersionId: aws.String(version),
	})
	if err != nil || receiveOne(t, queues, queueURL) != "" {
		t.Fatalf("repeated delete: %v", err)
	}
}

func equal(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}
