package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func consoleS3Request(
	t *testing.T, s *httptest.Server, operation, account, region string, input map[string]any,
) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"service": "s3", "operation": operation, "account": account, "region": region, "input": input,
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+consoleActionsPath,
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.URL)
	return r
}

func TestConsoleS3Lifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	const bucket = "console-s3-bucket"
	const region = "eu-west-1"
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		input["Bucket"] = bucket
		return consoleResponse(t, s, consoleS3Request(t, s, operation, "000000000000", region, input),
			http.StatusOK)["output"].(map[string]any)
	}
	write("CreateBucket", map[string]any{"CreateBucketConfiguration": map[string]any{"LocationConstraint": region}})
	write("PutBucketVersioning", map[string]any{"VersioningConfiguration": map[string]any{"Status": "Enabled"}})
	write("PutBucketTagging", map[string]any{
		"Tagging": map[string]any{"TagSet": []any{map[string]any{"Key": "team", "Value": "console"}}},
	})
	write("PutBucketCors", map[string]any{"CORSConfiguration": map[string]any{"CORSRules": []any{
		map[string]any{"AllowedOrigins": []any{"https://example.test"}, "AllowedMethods": []any{"GET"}},
	}}})

	// Both ASCII that looks like base64 and invalid UTF-8 must round-trip without ambiguity.
	key := "folder/é + ?%.bin"
	body := []byte{0, 1, 255, 128, 'a', '\n'}
	version := write("PutObject", map[string]any{
		"Key": key, "Body": base64.StdEncoding.EncodeToString(body), "ContentType": "application/octet-stream",
	})["VersionId"].(string)
	write("PutObject", map[string]any{"Key": key, "Body": base64.StdEncoding.EncodeToString([]byte("YWJj"))})
	read := write("GetObject", map[string]any{"Key": key})
	if read["Body"] != base64.StdEncoding.EncodeToString([]byte("YWJj")) {
		t.Fatal(read)
	}

	read = write("GetObject", map[string]any{"Key": key, "VersionId": version})
	if read["Body"] != base64.StdEncoding.EncodeToString(body) {
		t.Fatal(read)
	}

	write("PutObjectTagging", map[string]any{
		"Key": key, "VersionId": version,
		"Tagging": map[string]any{"TagSet": []any{map[string]any{"Key": "kind", "Value": "binary"}}},
	})
	write("PutObject", map[string]any{"Key": "folder/", "Body": ""})
	list := write("ListObjectsV2", map[string]any{"Delimiter": "/"})
	if len(list["CommonPrefixes"].([]any)) != 1 {
		t.Fatal(list)
	}

	write("CopyObject", map[string]any{
		"Key": "copy.bin", "CopySource": bucket + "/folder/%C3%A9%20%2B%20%3F%25.bin?versionId=" + version,
	})
	sdk := s3Client(s, "test", region)
	object, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: new(bucket), Key: new("copy.bin")})
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal(got, err)
	}

	tags, err := sdk.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: new(bucket)})
	if err != nil || len(tags.TagSet) != 1 || *tags.TagSet[0].Value != "console" {
		t.Fatal(tags, err)
	}

	for _, tc := range []struct {
		account, region string
		status          int
	}{
		{"111111111111", region, http.StatusForbidden},
		{"000000000000", "us-east-1", http.StatusMovedPermanently},
	} {
		consoleResponse(t, s, consoleS3Request(t, s, "GetObject", tc.account, tc.region,
			map[string]any{"Bucket": bucket, "Key": key}), tc.status)
	}
	write("DeleteObject", map[string]any{"Key": key})
	versions := write("ListObjectVersions", map[string]any{"MaxKeys": float64(1)})
	if versions["IsTruncated"] != true || versions["NextKeyMarker"] == nil {
		t.Fatal(versions)
	}

	next := write("ListObjectVersions", map[string]any{
		"MaxKeys": float64(1), "KeyMarker": versions["NextKeyMarker"],
		"VersionIdMarker": versions["NextVersionIdMarker"],
	})
	nextVersions, _ := next["Versions"].([]any)
	nextMarkers, _ := next["DeleteMarkers"].([]any)
	if len(nextVersions)+len(nextMarkers) != 1 {
		t.Fatal(next)
	}

	consoleResponse(t, s, consoleS3Request(t, s, "DeleteBucket", "000000000000", region,
		map[string]any{"Bucket": bucket}), http.StatusConflict)
	all := write("ListObjectVersions", map[string]any{})
	var deletes []any
	for _, field := range []string{"Versions", "DeleteMarkers"} {
		entries, _ := all[field].([]any)
		for _, item := range entries {
			entry := item.(map[string]any)
			deletes = append(deletes, map[string]any{"Key": entry["Key"], "VersionId": entry["VersionId"]})
		}
	}
	write("DeleteObjects", map[string]any{"Delete": map[string]any{"Objects": deletes}})
	upload := write("ListMultipartUploads", map[string]any{})
	if list, ok := upload["Uploads"].([]any); ok && len(list) != 0 {
		t.Fatal(upload)
	}

	// Unfinished uploads also prevent deletion and can be aborted through the console.
	multipart, err := sdk.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: new(bucket), Key: new("pending"),
	})
	if err != nil {
		t.Fatal(err)
	}

	consoleResponse(t, s, consoleS3Request(t, s, "DeleteBucket", "000000000000", region,
		map[string]any{"Bucket": bucket}), http.StatusConflict)
	write("AbortMultipartUpload", map[string]any{"Key": "pending", "UploadId": *multipart.UploadId})
	write("DeleteBucket", map[string]any{})
}

func TestConsoleS3ValidationAndSignatures(t *testing.T) {
	s := server(t)
	for _, tc := range []struct {
		input             map[string]any
		operation, region string
	}{
		{map[string]any{"Bucket": "invalid-region"}, "CreateBucket", ""},
		{map[string]any{"Bucket": "invalid-region"}, "CreateBucket", "../us-east-1"},
		{map[string]any{"Bucket": "never-written", "Key": "x", "Body": "%%%"}, "PutObject", "us-east-1"},
		{map[string]any{"Bucket": "never-written", "Key": "x", "Body": []any{float64(1)}}, "PutObject", "us-east-1"},
		{map[string]any{"Bucket": "never-written", "Key": "x", "Body": "", "Unknown": "x"}, "PutObject", "us-east-1"},
		{map[string]any{"Bucket": "never-written", "Key": "x"}, "CreateMultipartUpload", "us-east-1"},
	} {
		consoleResponse(t, s, consoleS3Request(t, s, tc.operation, "000000000000", tc.region, tc.input),
			http.StatusBadRequest)
	}

	verified := verifyingServer(t)
	for _, tc := range []struct {
		name, service, signedRegion, account string
		signed                               bool
		status                               int
	}{
		{"unsigned", "s3", "us-east-1", "111111111111", false, http.StatusForbidden},
		{"wrong-service", "iam", "us-east-1", "111111111111", true, http.StatusForbidden},
		{"wrong-region", "s3", "eu-west-1", "111111111111", true, http.StatusForbidden},
		{"wrong-account", "s3", "us-east-1", "000000000000", true, http.StatusForbidden},
		{"valid", "s3", "us-east-1", "111111111111", true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := consoleS3Request(t, verified, "CreateBucket", tc.account, "us-east-1",
				map[string]any{"Bucket": "signed-" + tc.name})
			if tc.signed {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}

				// Exercise full-body signing beyond the inspector's capture bound.
				data = append(data, []byte(strings.Repeat(" ", 10000))...)
				r.Body = io.NopCloser(bytes.NewReader(data))
				r.ContentLength = int64(len(data))
				sum := sha256.Sum256(data)
				if err = v4.NewSigner().SignHTTP(context.Background(),
					aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"}, r, hex.EncodeToString(sum[:]),
					tc.service, tc.signedRegion, time.Now()); err != nil {
					t.Fatal(err)
				}
			}

			consoleResponse(t, verified, r, tc.status)
		})
	}
}
