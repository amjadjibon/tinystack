package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3ByteRanges(t *testing.T) {
	server := server(t)
	client := s3Client(server, "test", "us-east-1")
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("range-bucket")}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("range-bucket"), Key: aws.String("file"), Body: strings.NewReader("0123456789"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("range-bucket"), Key: aws.String("empty"), Body: strings.NewReader(""),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name         string
		method       string
		key          string
		rangeHeader  string
		ifMatch      string
		ifNoneMatch  string
		body         string
		contentRange string
		acceptRanges string
		length       string
		status       int
	}{
		{"full get", "GET", "file", "", "", "", "0123456789", "", "", "10", 200},
		{"closed", "GET", "file", "bytes=2-5", "", "", "2345", "bytes 2-5/10", "bytes", "4", 206},
		{"open", "GET", "file", "bytes=7-", "", "", "789", "bytes 7-9/10", "bytes", "3", 206},
		{"suffix", "GET", "file", "bytes=-3", "", "", "789", "bytes 7-9/10", "bytes", "3", 206},
		{"clamped", "GET", "file", "bytes=8-99", "", "", "89", "bytes 8-9/10", "bytes", "2", 206},
		{"overlong suffix", "GET", "file", "bytes=-99", "", "", "0123456789", "bytes 0-9/10", "bytes", "10", 206},
		{"unsatisfiable", "GET", "file", "bytes=10-", "", "", "", "bytes */10", "", "", 416},
		{"reversed", "GET", "file", "bytes=5-2", "", "", "", "bytes */10", "", "", 416},
		{"multiple", "GET", "file", "bytes=0-1,3-4", "", "", "", "bytes */10", "", "", 416},
		{"empty", "GET", "empty", "bytes=0-0", "", "", "", "bytes */0", "", "", 416},
		{"not modified", "GET", "file", "bytes=10-", "", `"781e5e245d69b566979b86e28d23f2c7"`, "", "", "", "", 304},
		{"failed precondition before range", "GET", "file", "bytes=10-", `"wrong"`, "", "", "", "", "", 412},
		{"head full", "HEAD", "file", "", "", "", "", "", "", "10", 200},
		{"head range", "HEAD", "file", "bytes=2-5", "", "", "", "", "bytes", "4", 200},
		{"head unsatisfiable", "HEAD", "file", "bytes=10-", "", "", "", "bytes */10", "", "", 416},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(
				ctx,
				tc.method,
				fmt.Sprintf("%s/range-bucket/%s", server.URL, tc.key),
				http.NoBody,
			)
			if err != nil {
				t.Fatal(err)
			}
			if tc.rangeHeader != "" {
				req.Header.Set("Range", tc.rangeHeader)
			}
			if tc.ifMatch != "" {
				req.Header.Set("If-Match", tc.ifMatch)
			}
			if tc.ifNoneMatch != "" {
				req.Header.Set("If-None-Match", tc.ifNoneMatch)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status || resp.Header.Get("Content-Range") != tc.contentRange ||
				resp.Header.Get("Accept-Ranges") != tc.acceptRanges ||
				(tc.length != "" && resp.Header.Get("Content-Length") != tc.length) {
				t.Fatalf("HTTP %d headers=%v body=%q", resp.StatusCode, resp.Header, body)
			}
			if tc.status < 400 && string(body) != tc.body {
				t.Fatalf("body %q, want %q", body, tc.body)
			}
			if tc.method == "HEAD" && len(body) != 0 {
				t.Fatalf("HEAD body %q", body)
			}
			if tc.status == 416 && tc.method == "GET" && !strings.Contains(string(body), "<Code>InvalidRange</Code>") {
				t.Fatalf("error body %q", body)
			}
		})
	}

	got, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("range-bucket"), Key: aws.String("file"), Range: aws.String("bytes=1-3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	partial, err := io.ReadAll(got.Body)
	if err != nil || string(partial) != "123" || aws.ToString(got.ContentRange) != "bytes 1-3/10" ||
		aws.ToInt64(got.ContentLength) != 3 {
		t.Fatalf("SDK ranged GET: body %q, output %+v, error %v", partial, got, err)
	}
}
