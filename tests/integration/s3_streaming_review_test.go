package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/amjadjibon/tinystack/internal/faults"
)

func TestS3StreamingRejectedSignatureCleansSpool(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := verifyingServer(t)
	body := "payload with a correct hash and a bad signature"
	r := httptest.NewRequest(http.MethodPut, s.URL+"/spool-cleanup/object", strings.NewReader(body))
	sum := sha256.Sum256([]byte(body))
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	signedWithHeader(t, r, time.Now())
	auth := r.Header.Get("Authorization")
	start := strings.Index(auth, "Signature=")
	if start < 0 {
		t.Fatal("missing request signature")
	}
	r.Header.Set("Authorization", auth[:start]+"Signature="+strings.Repeat("0", 64))
	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "SignatureDoesNotMatch") {
		t.Fatalf("expected signature rejection, got %d %s", w.Code, w.Body.String())
	}
	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("signature rejection leaked spool files: %v", files)
	}
}

func TestS3StreamingHashMismatchCleansSpool(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := verifyingServer(t)
	body := "payload with a mismatched hash"
	r := httptest.NewRequest(http.MethodPut, s.URL+"/spool-cleanup/mismatch", strings.NewReader(body))
	r.Header.Set("X-Amz-Content-Sha256", strings.Repeat("0", 64))
	signedWithHeader(t, r, time.Now())

	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "XAmzContentSHA256Mismatch") {
		t.Fatalf("expected XAmzContentSHA256Mismatch, got %d %s", w.Code, w.Body.String())
	}
	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("hash mismatch leaked spool files: %v", files)
	}
}

type trackingCloser struct {
	io.Reader
	closed bool
}

func (c *trackingCloser) Close() error {
	c.closed = true
	return nil
}

func TestS3StreamingOriginalBodyClosedAndCleaned(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := verifyingServer(t)
	c := s3Client(s, "test", "us-east-1")
	_, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String("spool-cleanup"),
	})
	if err != nil {
		t.Fatal(err)
	}

	bodyText := "body that should be closed after spooling"
	bodySum := sha256.Sum256([]byte(bodyText))
	tracker := &trackingCloser{Reader: strings.NewReader(bodyText)}

	r := httptest.NewRequest(http.MethodPut, s.URL+"/spool-cleanup/closed-test", tracker)
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(bodySum[:]))
	signedWithHeader(t, r, time.Now())

	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", w.Code, w.Body.String())
	}
	if !tracker.closed {
		t.Fatal("original request body was not closed during spoolAndHash")
	}
	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("successful upload leaked spool files: %v", files)
	}
}

func TestS3StreamingDecodeFailureCleansSpool(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := verifyingServer(t)
	body := "body with malformed decode length"
	sum := sha256.Sum256([]byte(body))

	r := httptest.NewRequest(http.MethodPut, s.URL+"/spool-cleanup/decode-fail", strings.NewReader(body))
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("x-amz-decoded-content-length", "not-a-number")
	signedWithHeader(t, r, time.Now())

	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got %d %s", w.Code, w.Body.String())
	}
	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("decode failure leaked spool files: %v", files)
	}
}

func TestS3StreamingMalformedOverwritePreservesPriorObject(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	bucket := "malformed-overwrite-bucket"
	key := "target.txt"
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Put initial object
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader("initial valid content"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Overwrite with malformed aws-chunked data after terminator
	malformedBody := "5\r\nhello\r\n0\r\n\r\nextra data after terminator"
	req, err := http.NewRequest(http.MethodPut, s.URL+"/"+bucket+"/"+key, strings.NewReader(malformedBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", "5")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(respBytes), "InvalidArgument") {
		t.Fatalf("expected 400 InvalidArgument, got %d: %s", resp.StatusCode, string(respBytes))
	}

	// 3. Verify prior object is preserved
	getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer getOut.Body.Close()
	data, _ := io.ReadAll(getOut.Body)
	if string(data) != "initial valid content" {
		t.Fatalf("prior object was corrupted or overwritten, got: %q", string(data))
	}

	// 4. Verify no leaked temporary files
	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("leaked spool files: %v", files)
	}
}

func TestS3StreamingOrdinaryHTTPChecksumTrailers(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	bucket := "http-trailers-bucket"
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}

	// Case 1: Positive case with valid ordinary HTTP checksum trailer
	// CRC32 of "hello world" is DUoRhQ==
	key := "item.txt"
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("hello world"))
		pw.Close()
	}()

	req, err := http.NewRequest(http.MethodPut, s.URL+"/"+bucket+"/"+key, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.TransferEncoding = []string{"chunked"}
	req.Trailer = http.Header{
		"x-amz-checksum-crc32": nil,
	}
	req.Header.Set("Trailer", "x-amz-checksum-crc32")
	req.Header.Set("x-amz-trailer", "x-amz-checksum-crc32")
	req.Trailer.Set("x-amz-checksum-crc32", "DUoRhQ==")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for valid trailer, got %d", resp.StatusCode)
	}

	// Verify object and checksum
	getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		ChecksumMode: s3types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer getOut.Body.Close()
	data, _ := io.ReadAll(getOut.Body)
	if string(data) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", string(data))
	}
	if aws.ToString(getOut.ChecksumCRC32) != "DUoRhQ==" {
		t.Fatalf("expected ChecksumCRC32 DUoRhQ==, got %s", aws.ToString(getOut.ChecksumCRC32))
	}

	// Case 2: Failed checksum overwrite preserves existing data
	pr2, pw2 := io.Pipe()
	go func() {
		pw2.Write([]byte("overwritten data"))
		pw2.Close()
	}()
	req2, err := http.NewRequest(http.MethodPut, s.URL+"/"+bucket+"/"+key, pr2)
	if err != nil {
		t.Fatal(err)
	}
	req2.TransferEncoding = []string{"chunked"}
	req2.Trailer = http.Header{
		"x-amz-checksum-crc32": nil,
	}
	req2.Header.Set("Trailer", "x-amz-checksum-crc32")
	req2.Trailer.Set("x-amz-checksum-crc32", "AAAAAA==") // incorrect

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	respBytes2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusBadRequest || !strings.Contains(string(respBytes2), "BadDigest") {
		t.Fatalf("expected 400 BadDigest, got %d: %s", resp2.StatusCode, string(respBytes2))
	}

	// Verify prior object preserved
	getOut2, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer getOut2.Body.Close()
	data2, _ := io.ReadAll(getOut2.Body)
	if string(data2) != "hello world" {
		t.Fatalf("expected prior data 'hello world', got %q", string(data2))
	}

	// Case 3: Missing advertised trailer preserves existing data
	pr3, pw3 := io.Pipe()
	go func() {
		pw3.Write([]byte("another overwrite attempt"))
		pw3.Close()
	}()
	req3, err := http.NewRequest(http.MethodPut, s.URL+"/"+bucket+"/"+key, pr3)
	if err != nil {
		t.Fatal(err)
	}
	req3.TransferEncoding = []string{"chunked"}
	req3.Trailer = http.Header{"X-Amz-Checksum-Crc32": nil}
	// Trailer advertised but not sent

	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	respBytes3, _ := io.ReadAll(resp3.Body)
	if resp3.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(respBytes3), "InvalidArgument") {
		t.Fatalf(
			"expected 400 InvalidArgument for missing advertised trailer, got %d: %s",
			resp3.StatusCode,
			string(respBytes3),
		)
	}

	// Verify prior object still preserved
	getOut3, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer getOut3.Body.Close()
	data3, _ := io.ReadAll(getOut3.Body)
	if string(data3) != "hello world" {
		t.Fatalf("expected prior data 'hello world', got %q", string(data3))
	}

	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("leaked spool files: %v", files)
	}
}

func TestS3StreamingFaultInjectionCleansSpool(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := verifyingServer(t)

	// Inject fault error for S3 PutObject
	rule := faults.Rule{
		Service:   "s3",
		Operation: "PutObject",
		Error:     &faults.Error{Code: "InternalError", StatusCode: 500, Message: "fault injection"},
	}
	ruleJSON, _ := json.Marshal([]faults.Rule{rule})
	faultAPI(t, s, http.MethodPost, "/_tinystack/faults", string(ruleJSON), "", http.StatusOK)

	body := "fault injection test body"
	sum := sha256.Sum256([]byte(body))
	r := httptest.NewRequest(http.MethodPut, s.URL+"/fault-bucket/fault-key", strings.NewReader(body))
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	signedWithHeader(t, r, time.Now())

	w := httptest.NewRecorder()
	s.Config.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 InternalError from fault injection, got %d %s", w.Code, w.Body.String())
	}

	files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("fault injection leaked spool files: %v", files)
	}
}
